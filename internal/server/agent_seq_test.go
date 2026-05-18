package server

import (
	"strings"
	"testing"

	"flow/internal/flowdb"
)

// TestAgentHookSeqStaleEventIgnored pins the conditional-upsert behavior
// on the seq column: a hook event whose seq is lower than the latest
// stored seq for the same session must NOT overwrite the newer state.
// Without this, out-of-order delivery (e.g. PreToolUse arriving after
// the matching PostToolUse on a fast burst) would flap UI state.
func TestAgentHookSeqStaleEventIgnored(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	sid := "dddddddd-1111-4bbb-8bbb-bbbbbbbbbbbb"
	if _, err := db.Exec(
		`UPDATE tasks SET status='in-progress', session_provider='claude', session_id=?, session_started=? WHERE slug='build-ui'`,
		sid, flowdb.NowISO(),
	); err != nil {
		t.Fatal(err)
	}
	srv := New(Config{DB: db, FlowRoot: root, Version: "test"})

	// Newer event arrives first: permission request, seq 1000.
	first := map[string]any{
		"hook_event_name": "PermissionRequest",
		"session_id":      sid,
		"tool_name":       "Bash",
		"flow_seq":        int64(1000),
	}
	if _, err := srv.ingestAgentHook(agentHookTestRequest("claude"), first, agentHookTestRaw(t, first)); err != nil {
		t.Fatal(err)
	}

	// Stale event arrives next: stop, seq 500 (older). Must NOT clobber
	// the waiting state.
	stale := map[string]any{
		"hook_event_name": "Stop",
		"session_id":      sid,
		"flow_seq":        int64(500),
	}
	if _, err := srv.ingestAgentHook(agentHookTestRequest("claude"), stale, agentHookTestRaw(t, stale)); err != nil {
		t.Fatal(err)
	}

	state, err := flowdb.AgentRuntimeStateBySessionID(db, "claude", sid)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "waiting" {
		t.Fatalf("stale Stop overrode waiting state: %+v", state)
	}
	if state.LastSeq != 1000 {
		t.Fatalf("last_seq = %d, want 1000 (newer event kept)", state.LastSeq)
	}
}

// TestAgentHookSeqZeroBypassesGuard pins backwards compatibility with
// older hook installations that don't stamp seq: when both sides see
// seq=0, every event applies (so flow doesn't regress for users with
// stale hook scripts).
func TestAgentHookSeqZeroBypassesGuard(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	sid := "dddddddd-2222-4bbb-8bbb-bbbbbbbbbbbb"
	if _, err := db.Exec(
		`UPDATE tasks SET status='in-progress', session_provider='claude', session_id=?, session_started=? WHERE slug='build-ui'`,
		sid, flowdb.NowISO(),
	); err != nil {
		t.Fatal(err)
	}
	srv := New(Config{DB: db, FlowRoot: root, Version: "test"})

	for _, name := range []string{"PermissionRequest", "Stop"} {
		payload := map[string]any{
			"hook_event_name": name,
			"session_id":      sid,
			"tool_name":       "Bash",
		}
		if _, err := srv.ingestAgentHook(agentHookTestRequest("claude"), payload, agentHookTestRaw(t, payload)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	state, err := flowdb.AgentRuntimeStateBySessionID(db, "claude", sid)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "idle" {
		t.Fatalf("status = %q, want idle (Stop took effect under seq=0)", state.Status)
	}
}

// TestSubagentStopDoesNotClearParentAttention pins the subagent-aware
// gate. Per the herdr comparison: a subagent stopping must not be
// treated as the parent agent going idle, and must not clear parent
// attention rows (permission_request, elicitation_dialog) the user
// hasn't responded to yet.
func TestSubagentStopDoesNotClearParentAttention(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	sid := "dddddddd-3333-4bbb-8bbb-bbbbbbbbbbbb"
	if _, err := db.Exec(
		`UPDATE tasks SET status='in-progress', session_provider='claude', session_id=?, session_started=? WHERE slug='build-ui'`,
		sid, flowdb.NowISO(),
	); err != nil {
		t.Fatal(err)
	}
	srv := New(Config{DB: db, FlowRoot: root, Version: "test"})

	perm := map[string]any{
		"hook_event_name": "PermissionRequest",
		"session_id":      sid,
		"tool_name":       "Bash",
	}
	if _, err := srv.ingestAgentHook(agentHookTestRequest("claude"), perm, agentHookTestRaw(t, perm)); err != nil {
		t.Fatal(err)
	}
	// Verify the parent is waiting.
	agent, err := srv.agentForTask("build-ui")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Status != "waiting" {
		t.Fatalf("setup: agent.Status = %q, want waiting", agent.Status)
	}

	// A subagent stops while the parent is still waiting on user input.
	subStop := map[string]any{
		"hook_event_name": "SubagentStop",
		"session_id":      sid,
		"agent_id":        "subagent-abc",
	}
	if _, err := srv.ingestAgentHook(agentHookTestRequest("claude"), subStop, agentHookTestRaw(t, subStop)); err != nil {
		t.Fatal(err)
	}

	agent, err = srv.agentForTask("build-ui")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Status != "waiting" || agent.WaitingFor == nil {
		t.Fatalf("subagent stop wrongly cleared parent attention: %+v", agent)
	}
}

// TestSessionEndMapsToReleasedStatus pins the released semantic: a
// SessionEnd hook event must end up as status=released (not idle), so
// the UI and `flow do` can distinguish a fully torn-down session from
// a between-turn pause.
func TestSessionEndMapsToReleasedStatus(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	sid := "dddddddd-4444-4bbb-8bbb-bbbbbbbbbbbb"
	if _, err := db.Exec(
		`UPDATE tasks SET status='in-progress', session_provider='claude', session_id=?, session_started=? WHERE slug='build-ui'`,
		sid, flowdb.NowISO(),
	); err != nil {
		t.Fatal(err)
	}
	srv := New(Config{DB: db, FlowRoot: root, Version: "test"})

	payload := map[string]any{
		"hook_event_name": "SessionEnd",
		"session_id":      sid,
		"reason":          "user_quit",
	}
	if _, err := srv.ingestAgentHook(agentHookTestRequest("claude"), payload, agentHookTestRaw(t, payload)); err != nil {
		t.Fatal(err)
	}
	state, err := flowdb.AgentRuntimeStateBySessionID(db, "claude", sid)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "released" {
		t.Fatalf("status = %q, want released", state.Status)
	}
	if !strings.EqualFold(state.EventKind, "session_end") {
		t.Fatalf("event_kind = %q, want session_end", state.EventKind)
	}
}

// TestLivenessReconcilerForcesDeadOnMissingProcess pins the reconciler
// contract: a task whose Claude session is not in the live process
// scan AND whose runtime state is older than the grace window is
// forced to status=dead. The hook never fired Stop; the process is
// gone; the UI should reflect that without waiting forever.
func TestLivenessReconcilerForcesDeadOnMissingProcess(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	sid := "dddddddd-5555-4bbb-8bbb-bbbbbbbbbbbb"
	if _, err := db.Exec(
		`UPDATE tasks SET status='in-progress', session_provider='claude', session_id=?, session_started=? WHERE slug='build-ui'`,
		sid, flowdb.NowISO(),
	); err != nil {
		t.Fatal(err)
	}
	srv := New(Config{DB: db, FlowRoot: root, Version: "test"})
	// Plant a stale 'running' state (older than the grace window) so
	// reconcile flips it to dead.
	if err := flowdb.UpsertAgentRuntimeState(db, flowdb.AgentRuntimeStateInput{
		Provider:  "claude",
		SessionID: sid,
		TaskSlug:  "build-ui",
		Status:    "running",
		EventKind: "pre_tool_use",
		Seq:       100,
	}); err != nil {
		t.Fatal(err)
	}
	// Backdate so the row is past the grace window.
	if _, err := db.Exec(
		`UPDATE agent_runtime_states SET updated_at = ? WHERE provider = ? AND session_id = ?`,
		"2020-01-01T00:00:00Z", "claude", sid,
	); err != nil {
		t.Fatal(err)
	}

	// Stub the process scanner to return zero live sessions.
	oldScan := reconcileScanner
	reconcileScanner = func() ([]byte, error) { return []byte(""), nil }
	t.Cleanup(func() { reconcileScanner = oldScan })

	srv.reconcile.tick()

	state, err := flowdb.AgentRuntimeStateBySessionID(db, "claude", sid)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "dead" {
		t.Fatalf("reconciler did not flip to dead: %+v", state)
	}
}

// TestLivenessReconcilerSkipsLiveSession is the symmetric guard: when
// the Claude session is in the live process scan, the reconciler must
// not touch its runtime state.
func TestLivenessReconcilerSkipsLiveSession(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	sid := "dddddddd-6666-4bbb-8bbb-bbbbbbbbbbbb"
	if _, err := db.Exec(
		`UPDATE tasks SET status='in-progress', session_provider='claude', session_id=?, session_started=? WHERE slug='build-ui'`,
		sid, flowdb.NowISO(),
	); err != nil {
		t.Fatal(err)
	}
	srv := New(Config{DB: db, FlowRoot: root, Version: "test"})
	if err := flowdb.UpsertAgentRuntimeState(db, flowdb.AgentRuntimeStateInput{
		Provider:  "claude",
		SessionID: sid,
		TaskSlug:  "build-ui",
		Status:    "running",
		EventKind: "pre_tool_use",
		Seq:       100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`UPDATE agent_runtime_states SET updated_at = ? WHERE provider = ? AND session_id = ?`,
		"2020-01-01T00:00:00Z", "claude", sid,
	); err != nil {
		t.Fatal(err)
	}

	oldScan := reconcileScanner
	reconcileScanner = func() ([]byte, error) {
		return []byte("12345 claude --session-id " + sid + "\n"), nil
	}
	t.Cleanup(func() { reconcileScanner = oldScan })

	srv.reconcile.tick()

	state, err := flowdb.AgentRuntimeStateBySessionID(db, "claude", sid)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "running" {
		t.Fatalf("reconciler clobbered live session: %+v", state)
	}
}
