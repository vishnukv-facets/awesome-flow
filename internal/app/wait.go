package app

import (
	"database/sql"
	"encoding/json"
	"flow/internal/flowdb"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// cmdWait implements `flow wait <task> --until <state> [--timeout <dur>]
// [--server <url>]`.
//
// It blocks until either:
//   - the task reaches the requested state, OR
//   - the timeout fires (default 5 minutes; --timeout 0 for unbounded).
//
// The --until value can be any of:
//   - task lifecycle: backlog | in-progress | done
//   - runtime status: running | waiting | idle | dead | released
//
// Implementation: DB short-circuit first (so a task already in the
// target state exits 0 immediately), then dial /ws/events?task_slug=<slug>
// and watch the live stream. We deliberately reuse the SAME WS channel
// the UI consumes — no separate poll path, no parallel server endpoint.
func cmdWait(args []string) int {
	fs := flagSet("wait")
	until := fs.String("until", "done", "target state: backlog|in-progress|done|running|waiting|idle|dead|released")
	timeoutFlag := fs.Duration("timeout", 5*time.Minute, "max wait time; 0 = wait forever")
	server := fs.String("server", "", "flow UI base URL (default: $FLOW_UI_URL or http://127.0.0.1:8787)")
	if leadingHelpArg(args) {
		fs.Usage()
		return 0
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "error: wait requires a task slug")
		return 2
	}
	slug := args[0]
	if handled, rc := parseFlagSet(fs, args[1:]); handled {
		return rc
	}

	target := strings.ToLower(strings.TrimSpace(*until))
	if !isValidWaitTarget(target) {
		fmt.Fprintf(os.Stderr, "error: --until must be one of: backlog, in-progress, done, running, waiting, idle, dead, released; got %q\n", target)
		return 2
	}

	dbPath, err := flowDBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	db, err := flowdb.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()

	task, err := flowdb.GetTask(db, slug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: task %q not found\n", slug)
		return 1
	}

	// Short-circuit: if the task is already in the target state, exit 0
	// without ever opening the WS. This is the common case when the
	// caller is racing against a fast child that finished before they
	// could subscribe.
	if matchesWaitTarget(db, task, target) {
		fmt.Printf("%s is already %s\n", slug, target)
		return 0
	}

	// Open the WS subscription. Filter to this task only — the hub
	// fans out to every subscriber, but server-side filtering keeps
	// our reader inbox small.
	base := strings.TrimSpace(*server)
	if base == "" {
		base = strings.TrimSpace(os.Getenv("FLOW_UI_URL"))
	}
	if base == "" {
		base = "http://127.0.0.1:8787"
	}
	wsURL := toWSURL(base) + "/ws/events?task_slug=" + url.QueryEscape(slug)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: dial %s: %v\n", wsURL, err)
		return 1
	}
	defer conn.Close()

	var deadline time.Time
	if *timeoutFlag > 0 {
		deadline = time.Now().Add(*timeoutFlag)
		_ = conn.SetReadDeadline(deadline)
	}

	for {
		// Recheck the DB on every loop iteration. The WS might have
		// missed a transition that happened between the short-circuit
		// check and the dial — small but real race. The recheck is
		// cheap (single SELECT) and protects against silent stuck waits.
		if refreshed, err := flowdb.GetTask(db, slug); err == nil {
			if matchesWaitTarget(db, refreshed, target) {
				fmt.Printf("%s reached %s\n", slug, target)
				return 0
			}
		}

		_, raw, err := conn.ReadMessage()
		if err != nil {
			if *timeoutFlag > 0 && time.Now().After(deadline) {
				fmt.Fprintf(os.Stderr, "timeout: %s did not reach %s within %s\n", slug, target, *timeoutFlag)
				return 1
			}
			fmt.Fprintf(os.Stderr, "error: ws read: %v\n", err)
			return 1
		}

		var env waitEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		if env.TaskSlug != "" && env.TaskSlug != slug {
			// Defensive: server filter should have dropped these, but
			// don't trust the wire.
			continue
		}
		if envelopeMatches(env, target) {
			fmt.Printf("%s reached %s\n", slug, target)
			return 0
		}
	}
}

// waitEnvelope is the subset of eventEnvelope we care about — we ignore
// most fields. Decoupled from server.eventEnvelope so a wire-format
// addition doesn't force every CLI user to upgrade in lockstep.
type waitEnvelope struct {
	Type     string `json:"type"`
	TaskSlug string `json:"task_slug"`
	Hook     *struct {
		Kind string `json:"kind"`
	} `json:"hook,omitempty"`
	Liveness *struct {
		Status string `json:"status"`
	} `json:"liveness,omitempty"`
}

func envelopeMatches(env waitEnvelope, target string) bool {
	switch target {
	case "running":
		return env.Hook != nil && isRunningKind(env.Hook.Kind)
	case "waiting":
		return env.Hook != nil && isWaitingKind(env.Hook.Kind)
	case "idle":
		return env.Hook != nil && env.Hook.Kind == "stop"
	case "released":
		return env.Hook != nil && env.Hook.Kind == "session_end"
	case "dead":
		if env.Hook != nil && env.Hook.Kind == "stop_failure" {
			return true
		}
		return env.Liveness != nil && env.Liveness.Status == "dead"
	}
	return false
}

func isRunningKind(kind string) bool {
	switch kind {
	case "user_prompt_submit", "pre_tool_use", "post_tool_use", "post_tool_use_failure",
		"post_tool_batch", "session_start", "task_created":
		return true
	}
	return false
}

func isWaitingKind(kind string) bool {
	switch kind {
	case "permission_request", "permission_prompt", "elicitation", "elicitation_dialog", "idle_prompt":
		return true
	}
	return false
}

// matchesWaitTarget checks the DB row + agent_runtime_states for the
// target condition. Used both for the up-front short-circuit and for
// the re-check on every WS iteration.
func matchesWaitTarget(db *sql.DB, task *flowdb.Task, target string) bool {
	switch target {
	case "backlog", "in-progress", "done":
		return task.Status == target
	}
	if !task.SessionID.Valid {
		return false
	}
	state, err := flowdb.AgentRuntimeStateBySessionID(db, task.SessionProvider, task.SessionID.String)
	if err != nil {
		return false
	}
	return state.Status == target
}

func isValidWaitTarget(s string) bool {
	switch s {
	case "backlog", "in-progress", "done",
		"running", "waiting", "idle", "dead", "released":
		return true
	}
	return false
}

// toWSURL flips http:// → ws://, https:// → wss://; passes others through.
func toWSURL(base string) string {
	switch {
	case strings.HasPrefix(base, "http://"):
		return "ws://" + strings.TrimPrefix(base, "http://")
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://")
	}
	return base
}
