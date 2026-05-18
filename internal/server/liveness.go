package server

import (
	"context"
	"database/sql"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"flow/internal/flowdb"
)

// livenessReconciler is the periodic process-scan reconciler that catches
// sessions whose Stop / SessionEnd hook never fired (Claude crashes,
// network blip, agent killed by oom-killer, etc.). Without it the runtime
// state for a dead session shows stale "running" forever, which is the
// most annoying class of UI lies.
//
// It runs every reconcileInterval; per tick:
//  1. Snapshot the live Claude session IDs from `ps -axo pid,command`.
//  2. Snapshot the live Codex session IDs (last-modified Codex JSONL).
//  3. For each row in tasks where session_id IS NOT NULL and runtime
//     status is in {running, waiting}, if the session id is missing
//     from the live snapshot AND the runtime row hasn't been updated
//     recently, force status=dead.
//
// Reconciler errors are non-fatal — a tick that fails to ps the process
// list just logs and tries again next round. This is liveness telemetry,
// not a correctness boundary.
type livenessReconciler struct {
	srv      *Server
	interval time.Duration
	grace    time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

const (
	defaultReconcileInterval = 30 * time.Second
	// Grace window before forcing dead. A session can be ungracefully
	// torn down while the next hook is in-flight; this avoids flapping
	// the UI between running and dead when a hook arrives moments late.
	defaultReconcileGrace = 90 * time.Second
)

func newLivenessReconciler(srv *Server) *livenessReconciler {
	return &livenessReconciler{
		srv:      srv,
		interval: defaultReconcileInterval,
		grace:    defaultReconcileGrace,
	}
}

func (r *livenessReconciler) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan struct{})
	go r.loop(ctx)
}

func (r *livenessReconciler) stop() {
	r.mu.Lock()
	cancel := r.cancel
	done := r.done
	r.cancel = nil
	r.done = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (r *livenessReconciler) loop(ctx context.Context) {
	defer close(r.done)
	// Tick immediately on start — the first reconcile is the one that
	// catches a session that crashed before the server came up. Then
	// follow the configured cadence.
	r.tick()
	tick := time.NewTicker(r.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.tick()
		}
	}
}

func (r *livenessReconciler) tick() {
	if r.srv == nil || r.srv.cfg.DB == nil {
		return
	}
	live, err := scanLiveSessions()
	if err != nil {
		// Process scan failed — likely a sandbox without ps. Skip this
		// tick rather than declaring everything dead.
		return
	}
	r.reconcileTasks(r.srv.cfg.DB, live)
}

// liveSessionSet captures both Claude and Codex sessions detected as
// live on this host. Claude sessions come from ps, Codex sessions come
// from a Codex-process scan; both end up as lower-case session ids.
type liveSessionSet struct {
	claude map[string]bool
	codex  map[string]bool
}

func (l liveSessionSet) has(provider, sessionID string) bool {
	sessionID = strings.ToLower(strings.TrimSpace(sessionID))
	if sessionID == "" {
		return false
	}
	switch provider {
	case "claude":
		return l.claude[sessionID]
	case "codex":
		// Codex doesn't pass its session id on the command line; flow
		// captures it from Codex's JSONL store after launch. The
		// pragmatic liveness signal is "is the codex process running
		// in any flow-managed cwd" — we treat any live codex process
		// as keeping its session alive. If no codex process is alive,
		// every codex session is candidate-dead.
		return len(l.codex) > 0
	}
	return false
}

var reLiveClaudeSession = regexp.MustCompile(
	`(?:--session-id|--resume)[ =]([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-4[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12})`,
)

var reconcileScanner = scanProcesses

func scanProcesses() ([]byte, error) {
	return exec.Command("ps", "-axo", "pid,command").Output()
}

func scanLiveSessions() (liveSessionSet, error) {
	out, err := reconcileScanner()
	if err != nil {
		return liveSessionSet{}, err
	}
	set := liveSessionSet{
		claude: map[string]bool{},
		codex:  map[string]bool{},
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "claude") {
			for _, m := range reLiveClaudeSession.FindAllStringSubmatch(line, -1) {
				if len(m) >= 2 {
					set.claude[strings.ToLower(m[1])] = true
				}
			}
		}
		if strings.Contains(line, "codex") {
			// Mark codex live; we key by a sentinel since codex's session
			// id isn't on the argv.
			set.codex["any"] = true
		}
	}
	return set, nil
}

type liveTaskRow struct {
	slug      string
	provider  string
	sessionID string
}

func (r *livenessReconciler) reconcileTasks(db *sql.DB, live liveSessionSet) {
	// Tasks whose session is in-flight (status='in-progress') and have a
	// session_id are candidates. We skip backlog/done because those
	// already imply a non-live state — the reconciler shouldn't second-
	// guess workflow status.
	rows, err := db.Query(`
		SELECT slug, session_provider, session_id
		FROM tasks
		WHERE status = 'in-progress'
		  AND session_id IS NOT NULL
		  AND deleted_at IS NULL
	`)
	if err != nil {
		return
	}
	defer rows.Close()
	var stale []liveTaskRow
	for rows.Next() {
		var row liveTaskRow
		if err := rows.Scan(&row.slug, &row.provider, &row.sessionID); err != nil {
			return
		}
		if live.has(row.provider, row.sessionID) {
			continue
		}
		stale = append(stale, row)
	}
	if err := rows.Err(); err != nil {
		return
	}

	if len(stale) == 0 {
		return
	}
	cutoff := time.Now().Add(-r.grace).UTC().Format(time.RFC3339)
	for _, row := range stale {
		state, err := flowdb.AgentRuntimeStateBySessionID(db, row.provider, row.sessionID)
		if err != nil {
			continue
		}
		switch state.Status {
		case "dead", "idle", "released":
			// Already terminal — nothing to reconcile.
			continue
		}
		if state.UpdatedAt > cutoff {
			// Hook is still freshly active; give it a grace window
			// before we flip to dead.
			continue
		}
		_ = flowdb.UpsertAgentRuntimeState(db, flowdb.AgentRuntimeStateInput{
			Provider:  row.provider,
			SessionID: row.sessionID,
			TaskSlug:  row.slug,
			Status:    "dead",
			EventKind: "liveness_reconciled",
			Message:   "process not found in live scan",
			// Forcing seq=0 so the conditional upsert applies regardless
			// of what the hook last wrote — this row is authoritative
			// over any stale running event we beat to the punch.
			Seq:     0,
			RawJSON: "",
		})
		r.srv.publishLiveness(row.provider, row.sessionID, row.slug, "dead", "process not found")
	}
}
