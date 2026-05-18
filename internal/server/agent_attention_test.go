package server

import (
	"encoding/json"
	"flow/internal/flowdb"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestContextWindowForProvider(t *testing.T) {
	if got := contextWindowForProvider("claude"); got != 1000000 {
		t.Fatalf("claude context window = %d, want 1000000", got)
	}
	if got := contextWindowForProvider("codex"); got != 200000 {
		t.Fatalf("codex context window = %d, want 200000", got)
	}
}

func TestSessionTranscriptUsageStats(t *testing.T) {
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "claude.jsonl")
	if err := os.WriteFile(claudePath, []byte(`{"type":"assistant","timestamp":"2026-05-16T12:00:00Z","message":{"role":"assistant","usage":{"input_tokens":10,"cache_read_input_tokens":20,"output_tokens":5},"content":[{"type":"text","text":"Done"}]}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	claude := sessionTranscriptUsageStats(claudePath)
	if claude.TokensUsed != 35 || claude.LastTimestamp != "2026-05-16T12:00:00Z" {
		t.Fatalf("claude stats = %+v, want 35 tokens and timestamp", claude)
	}

	codexPath := filepath.Join(dir, "codex.jsonl")
	codexLine := `{"type":"event_msg","timestamp":"2026-05-16T12:01:00Z","payload":{"type":"token_count","info":{"model_context_window":258400,"last_token_usage":{"input_tokens":100,"cached_input_tokens":50,"output_tokens":25,"reasoning_output_tokens":5,"total_tokens":180},"total_token_usage":{"total_tokens":999}}}}`
	if err := os.WriteFile(codexPath, []byte(codexLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	codex := sessionTranscriptUsageStats(codexPath)
	if codex.TokensUsed != 180 || codex.TokensMax != 258400 || codex.LastTimestamp != "2026-05-16T12:01:00Z" {
		t.Fatalf("codex stats = %+v, want reported usage/window/timestamp", codex)
	}
}

func TestAgentAttentionNotifications(t *testing.T) {
	notifs := agentAttentionNotifications([]uiAgent{
		{
			Slug:       "switcher",
			Name:       "Switcher",
			Provider:   "codex",
			Status:     "waiting",
			SessionID:  "bbbbbbbb-2222-4bbb-8bbb-bbbbbbbbbbbb",
			LastAction: "permission requested",
			WaitingFor: &uiWaitingFor{Kind: "permission", Why: "Would you like to run the following command?"},
		},
	})
	if len(notifs) != 1 {
		t.Fatalf("notifications = %d, want 1", len(notifs))
	}
	if notifs[0].Level != "approval" || notifs[0].Status != "unread" || notifs[0].Source != "agent" {
		t.Fatalf("notification = %+v, want unread agent approval", notifs[0])
	}
}

func TestAgentAttentionNotificationsIgnoreLifecycleState(t *testing.T) {
	notifs := agentAttentionNotifications([]uiAgent{
		{
			Slug:       "performance",
			Name:       "performance",
			Provider:   "codex",
			Status:     "idle",
			SessionID:  "aaaaaaaa-1111-4aaa-8aaa-aaaaaaaaaaaa",
			LastAction: "assistant: done",
		},
		{
			Slug:       "build-ui",
			Name:       "Build UI",
			Provider:   "claude",
			Status:     "running",
			SessionID:  "bbbbbbbb-2222-4bbb-8bbb-bbbbbbbbbbbb",
			LastAction: "running tests",
		},
	})
	if len(notifs) != 0 {
		t.Fatalf("notifications = %+v, want lifecycle states kept out of notifications", notifs)
	}
}

func TestAgentHookPermissionCreatesWaitingNotification(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	sessionID := "bbbbbbbb-2222-4bbb-8bbb-bbbbbbbbbbbb"
	if _, err := db.Exec(
		`UPDATE tasks SET status='in-progress', session_provider='claude', session_id=?, session_started=? WHERE slug='build-ui'`,
		sessionID, flowdb.NowISO(),
	); err != nil {
		t.Fatal(err)
	}
	srv := New(Config{DB: db, FlowRoot: root, Version: "test"})
	payload := map[string]any{
		"hook_event_name": "PermissionRequest",
		"session_id":      sessionID,
		"tool_name":       "Bash",
		"tool_input": map[string]any{
			"command": "git status",
		},
	}
	resp, err := srv.ingestAgentHook(agentHookTestRequest("claude"), payload, agentHookTestRaw(t, payload))
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Task != "build-ui" || resp.Kind != "permission_request" || resp.NotificationID == "" {
		t.Fatalf("response = %+v", resp)
	}

	agent, err := srv.agentForTask("build-ui")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Status != "waiting" || agent.WaitingFor == nil || agent.WaitingFor.Kind != "permission" {
		t.Fatalf("agent = %+v, want waiting for permission", agent)
	}
	monitor := srv.uiMonitor([]uiAgent{*agent})
	if monitor.Unread != 1 || monitor.Approvals != 1 {
		t.Fatalf("monitor = %+v, want one unread approval", monitor)
	}
}

func TestAgentHookPostToolUseClearsWaiting(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	sessionID := "bbbbbbbb-2222-4bbb-8bbb-bbbbbbbbbbbb"
	if _, err := db.Exec(
		`UPDATE tasks SET status='in-progress', session_provider='codex', session_id=?, session_started=? WHERE slug='build-ui'`,
		sessionID, flowdb.NowISO(),
	); err != nil {
		t.Fatal(err)
	}
	srv := New(Config{DB: db, FlowRoot: root, Version: "test"})
	permission := map[string]any{
		"hook_event_name": "PermissionRequest",
		"session_id":      sessionID,
		"tool_name":       "Bash",
	}
	if _, err := srv.ingestAgentHook(agentHookTestRequest("codex"), permission, agentHookTestRaw(t, permission)); err != nil {
		t.Fatal(err)
	}
	done := map[string]any{
		"hook_event_name": "PostToolUse",
		"session_id":      sessionID,
		"tool_name":       "Bash",
		"tool_use_id":     "toolu_123",
	}
	if _, err := srv.ingestAgentHook(agentHookTestRequest("codex"), done, agentHookTestRaw(t, done)); err != nil {
		t.Fatal(err)
	}

	agent, err := srv.agentForTask("build-ui")
	if err != nil {
		t.Fatal(err)
	}
	if agent.WaitingFor != nil || agent.Status == "waiting" {
		t.Fatalf("agent = %+v, want hook attention cleared", agent)
	}
	monitor := srv.uiMonitor([]uiAgent{*agent})
	if monitor.Approvals != 0 {
		t.Fatalf("monitor = %+v, want hook approval cleared", monitor)
	}
}

func TestAgentHookPreToolAskUserQuestionCreatesWaiting(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	sessionID := "bbbbbbbb-2222-4bbb-8bbb-bbbbbbbbbbbb"
	if _, err := db.Exec(
		`UPDATE tasks SET status='in-progress', session_provider='codex', session_id=?, session_started=? WHERE slug='build-ui'`,
		sessionID, flowdb.NowISO(),
	); err != nil {
		t.Fatal(err)
	}
	srv := New(Config{DB: db, FlowRoot: root, Version: "test"})
	payload := map[string]any{
		"hook_event_name": "PreToolUse",
		"session_id":      sessionID,
		"tool_name":       "mcp__functions__request_user_input",
		"tool_input": map[string]any{
			"question": "Which branch should I use?",
		},
	}
	if _, err := srv.ingestAgentHook(agentHookTestRequest("codex"), payload, agentHookTestRaw(t, payload)); err != nil {
		t.Fatal(err)
	}

	agent, err := srv.agentForTask("build-ui")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Status != "waiting" || agent.WaitingFor == nil || agent.WaitingFor.Kind != "question" {
		t.Fatalf("agent = %+v, want waiting for question", agent)
	}
}

func TestCodexTranscriptRequestUserInputCreatesWaitingFallback(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	sessionID := "aaaaaaaa-1111-4aaa-8aaa-aaaaaaaaaaaa"
	if _, err := db.Exec(
		`UPDATE tasks SET status='in-progress', session_provider='codex', session_id=?, session_started=?, work_dir=? WHERE slug='build-ui'`,
		sessionID, "2026-05-17T11:02:52+05:30", root,
	); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(codexHome, "sessions", "2026", "05", "17")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"session_meta","payload":{"id":"` + sessionID + `","timestamp":"2026-05-17T11:02:52+05:30","cwd":"` + root + `"}}`,
		`{"timestamp":"2026-05-17T11:03:00+05:30","type":"response_item","payload":{"type":"function_call","name":"request_user_input","arguments":"{\"questions\":[{\"header\":\"Focus\",\"id\":\"focus\",\"question\":\"Which hook review path should I do next?\",\"options\":[{\"label\":\"Small fixes\",\"description\":\"Patch low-risk issues.\"}]}]}","call_id":"call_pending"}}`,
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "rollout-2026-05-17T11-02-52-"+sessionID+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := New(Config{DB: db, FlowRoot: root, Version: "test"})
	agent, err := srv.agentForTask("build-ui")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Status != "waiting" || agent.RuntimeSource != "transcript" || agent.WaitingFor == nil || !strings.Contains(agent.WaitingFor.Why, "Which hook review path") {
		t.Fatalf("agent = %+v, want transcript-derived waiting question", agent)
	}
	monitor := srv.uiMonitor([]uiAgent{*agent})
	if len(monitor.Notifications) != 1 || !strings.Contains(monitor.Notifications[0].Title, "needs your answer") {
		t.Fatalf("monitor notifications = %+v, want synthetic question notification", monitor.Notifications)
	}
	foundEvent := false
	for _, event := range monitor.Events {
		if event.Source == agentTranscriptMonitorSource && event.Kind == "elicitation" && strings.Contains(event.Body, "Which hook review path") {
			foundEvent = true
		}
	}
	if !foundEvent {
		t.Fatalf("monitor events = %+v, want transcript elicitation event", monitor.Events)
	}
}

func TestCodexTranscriptRequestUserInputIgnoresAnsweredCall(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	sessionID := "aaaaaaaa-1111-4aaa-8aaa-aaaaaaaaaaaa"
	if _, err := db.Exec(
		`UPDATE tasks SET status='in-progress', session_provider='codex', session_id=?, session_started=?, work_dir=? WHERE slug='build-ui'`,
		sessionID, "2026-05-17T11:02:52+05:30", root,
	); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(codexHome, "sessions", "2026", "05", "17")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"session_meta","payload":{"id":"` + sessionID + `","timestamp":"2026-05-17T11:02:52+05:30","cwd":"` + root + `"}}`,
		`{"timestamp":"2026-05-17T11:02:55+05:30","type":"response_item","payload":{"type":"function_call","name":"request_user_input","arguments":"{\"questions\":[{\"question\":\"Old interrupted question?\"}]}","call_id":"call_stale"}}`,
		`{"timestamp":"2026-05-17T11:02:58+05:30","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"resume"}]}}`,
		`{"timestamp":"2026-05-17T11:03:00+05:30","type":"response_item","payload":{"type":"function_call","name":"request_user_input","arguments":"{\"questions\":[{\"question\":\"Which option?\"}]}","call_id":"call_answered"}}`,
		`{"timestamp":"2026-05-17T11:03:10+05:30","type":"response_item","payload":{"type":"function_call_output","call_id":"call_answered","output":"{\"answers\":{\"focus\":{\"answers\":[\"Small fixes\"]}}}"}}`,
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "rollout-2026-05-17T11-02-52-"+sessionID+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := New(Config{DB: db, FlowRoot: root, Version: "test"})
	agent, err := srv.agentForTask("build-ui")
	if err != nil {
		t.Fatal(err)
	}
	if agent.WaitingFor != nil || agent.Status == "waiting" {
		t.Fatalf("agent = %+v, want answered request_user_input ignored", agent)
	}
}

func TestAgentHookStopOverridesLiveProcessRuntime(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	t.Setenv("CODEX_HOME", t.TempDir())
	sessionID := "aaaaaaaa-1111-4aaa-8aaa-aaaaaaaaaaaa"
	started := "2026-05-17T11:02:52+05:30"
	if _, err := db.Exec(
		`UPDATE tasks SET status='in-progress', session_provider='codex', session_id=?, session_started=?, session_last_resumed=? WHERE slug='build-ui'`,
		sessionID, started, started,
	); err != nil {
		t.Fatal(err)
	}
	oldPS := psRunner
	psRunner = func() ([]byte, error) {
		return []byte("123 codex resume --include-non-interactive " + sessionID + "\n"), nil
	}
	t.Cleanup(func() { psRunner = oldPS })

	srv := New(Config{DB: db, FlowRoot: root, Version: "test"})
	payload := map[string]any{
		"hook_event_name":        "Stop",
		"session_id":             sessionID,
		"last_assistant_message": "Finished the audit.",
	}
	if _, err := srv.ingestAgentHook(agentHookTestRequest("codex"), payload, agentHookTestRaw(t, payload)); err != nil {
		t.Fatal(err)
	}

	agent, err := srv.agentForTask("build-ui")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Status != "idle" || agent.RuntimeStatus != "idle" || agent.TaskStatus != "in-progress" || agent.RuntimeSource != "hook" || agent.RuntimeEvent != "stop" {
		t.Fatalf("agent = %+v, want idle runtime from hook and in-progress task", agent)
	}
	if agent.Terminal.Mode != "native" {
		t.Fatalf("terminal mode = %q, want native process still detected", agent.Terminal.Mode)
	}
}

func TestAgentHookUserPromptSubmitMarksRuntimeRunningAfterStop(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	t.Setenv("CODEX_HOME", t.TempDir())
	sessionID := "aaaaaaaa-1111-4aaa-8aaa-aaaaaaaaaaaa"
	started := "2026-05-17T11:02:52+05:30"
	if _, err := db.Exec(
		`UPDATE tasks SET status='in-progress', session_provider='codex', session_id=?, session_started=?, session_last_resumed=? WHERE slug='build-ui'`,
		sessionID, started, started,
	); err != nil {
		t.Fatal(err)
	}
	oldPS := psRunner
	psRunner = func() ([]byte, error) {
		return []byte("123 codex resume --include-non-interactive " + sessionID + "\n"), nil
	}
	t.Cleanup(func() { psRunner = oldPS })

	srv := New(Config{DB: db, FlowRoot: root, Version: "test"})
	stop := map[string]any{"hook_event_name": "Stop", "session_id": sessionID}
	if _, err := srv.ingestAgentHook(agentHookTestRequest("codex"), stop, agentHookTestRaw(t, stop)); err != nil {
		t.Fatal(err)
	}
	prompt := map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sessionID, "prompt": "Improve documentation"}
	if _, err := srv.ingestAgentHook(agentHookTestRequest("codex"), prompt, agentHookTestRaw(t, prompt)); err != nil {
		t.Fatal(err)
	}

	agent, err := srv.agentForTask("build-ui")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Status != "running" || agent.RuntimeSource != "hook" || agent.RuntimeEvent != "user_prompt_submit" {
		t.Fatalf("agent = %+v, want running runtime from user prompt hook", agent)
	}
}

func TestAgentLifecycleNotificationHiddenWhenRuntimeMovesPastStop(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	t.Setenv("CODEX_HOME", t.TempDir())
	sessionID := "aaaaaaaa-1111-4aaa-8aaa-aaaaaaaaaaaa"
	started := "2026-05-17T11:02:52+05:30"
	if _, err := db.Exec(
		`UPDATE tasks SET status='in-progress', session_provider='codex', session_id=?, session_started=?, session_last_resumed=? WHERE slug='build-ui'`,
		sessionID, started, started,
	); err != nil {
		t.Fatal(err)
	}
	oldPS := psRunner
	psRunner = func() ([]byte, error) {
		return []byte("123 codex resume --include-non-interactive " + sessionID + "\n"), nil
	}
	t.Cleanup(func() { psRunner = oldPS })

	srv := New(Config{DB: db, FlowRoot: root, Version: "test"})
	stop := map[string]any{"hook_event_name": "Stop", "session_id": sessionID}
	if _, err := srv.ingestAgentHook(agentHookTestRequest("codex"), stop, agentHookTestRaw(t, stop)); err != nil {
		t.Fatal(err)
	}
	agent, err := srv.agentForTask("build-ui")
	if err != nil {
		t.Fatal(err)
	}
	monitor := srv.uiMonitor([]uiAgent{*agent})
	if len(monitor.Notifications) != 1 || !strings.Contains(monitor.Notifications[0].Title, "stopped") {
		t.Fatalf("monitor after stop = %+v, want visible stop notification", monitor)
	}

	prompt := map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sessionID, "prompt": "Continue"}
	if _, err := srv.ingestAgentHook(agentHookTestRequest("codex"), prompt, agentHookTestRaw(t, prompt)); err != nil {
		t.Fatal(err)
	}
	agent, err = srv.agentForTask("build-ui")
	if err != nil {
		t.Fatal(err)
	}
	monitor = srv.uiMonitor([]uiAgent{*agent})
	for _, notification := range monitor.Notifications {
		if strings.Contains(notification.Title, "stopped") {
			t.Fatalf("monitor after resume = %+v, want stale stop notification hidden", monitor)
		}
	}
	if monitor.Unread != 0 {
		t.Fatalf("unread after resume = %d, want stale lifecycle notification out of unread count", monitor.Unread)
	}
}

func TestCodexHookHealthRequiresTrustedHookState(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	t.Setenv("CODEX_HOME", t.TempDir())
	sessionID := "aaaaaaaa-1111-4aaa-8aaa-aaaaaaaaaaaa"
	if _, err := db.Exec(
		`UPDATE tasks SET status='in-progress', session_provider='codex', session_id=?, session_started=? WHERE slug='build-ui'`,
		sessionID, "2026-05-17T11:02:52+05:30",
	); err != nil {
		t.Fatal(err)
	}
	writeCodexHookConfig(t, root, "flow hook agent-event --provider codex --url 'http://127.0.0.1:8787/api/hooks/agent'")
	if err := flowdb.UpsertAgentRuntimeState(db, flowdb.AgentRuntimeStateInput{
		Provider:  "codex",
		SessionID: sessionID,
		TaskSlug:  "build-ui",
		Status:    "running",
		EventKind: "user_prompt_submit",
	}); err != nil {
		t.Fatal(err)
	}

	srv := New(Config{DB: db, FlowRoot: root, Version: "test"})
	agent, err := srv.agentForTask("build-ui")
	if err != nil {
		t.Fatal(err)
	}
	if agent.HookHealth == nil || agent.HookHealth.Status != "needs_approval" {
		t.Fatalf("hook health = %+v, want needs_approval when Codex has no trusted state", agent.HookHealth)
	}
}

func TestCodexHookHealthUsesTrustedStateNotTranscriptWarning(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	sessionID := "aaaaaaaa-1111-4aaa-8aaa-aaaaaaaaaaaa"
	writeCodexHookConfig(t, root, "flow hook agent-event --provider codex --url 'http://127.0.0.1:8787/api/hooks/agent'")
	writeCodexTrustedHookState(t, codexHome, root)
	srv := New(Config{DB: db, FlowRoot: root, Version: "test"})
	health := srv.agentHookHealth(
		TaskView{Slug: "build-ui", WorkDir: root, SessionID: &sessionID},
		"codex",
		[]uiTranscript{{Type: "assistant", Time: "2026-05-17T11:00:00Z", Text: "6 hooks need review before they can run. Open /hooks to review them."}},
		&flowdb.AgentRuntimeState{UpdatedAt: "2026-05-17T12:00:00Z"},
	)
	if health != nil {
		t.Fatalf("hook health = %+v, want trusted Codex hook state to suppress old transcript warnings", health)
	}
}

func TestCodexHookHealthRejectsAbsoluteManagedHookCommand(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	t.Setenv("CODEX_HOME", t.TempDir())
	sessionID := "aaaaaaaa-1111-4aaa-8aaa-aaaaaaaaaaaa"
	writeCodexHookConfig(t, root, "/Users/vishnukv/facets/codebases/flow-manager/bin/flow hook agent-event --provider codex")
	srv := New(Config{DB: db, FlowRoot: root, Version: "test"})
	health := srv.agentHookHealth(
		TaskView{Slug: "build-ui", WorkDir: root, SessionID: &sessionID},
		"codex",
		nil,
		nil,
	)
	if health == nil || health.Status != "missing" || !strings.Contains(health.Message, "old command") {
		t.Fatalf("hook health = %+v, want stale absolute command flagged", health)
	}
}

func writeCodexHookConfig(t *testing.T, workDir, command string) {
	t.Helper()
	path := filepath.Join(workDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":` + strconv.Quote(command) + `}]}]}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeCodexTrustedHookState(t *testing.T, codexHome, workDir string) {
	t.Helper()
	path := filepath.Join(codexHome, "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	hooksPath := filepath.Join(workDir, ".codex", "hooks.json")
	if abs, err := filepath.Abs(hooksPath); err == nil {
		hooksPath = abs
	}
	body := "[hooks.state]\n\n" +
		"[hooks.state." + strconv.Quote(hooksPath+":session_start:0:0") + "]\n" +
		"trusted_hash = \"sha256:test\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func agentHookTestRequest(provider string) *http.Request {
	return &http.Request{URL: &url.URL{RawQuery: "provider=" + provider}}
}

func agentHookTestRaw(t *testing.T, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAgentAttentionNotificationCanBeDismissed(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv := &Server{cfg: Config{DB: db}}
	agents := []uiAgent{
		{
			Slug:      "switcher",
			Name:      "Switcher",
			Provider:  "codex",
			Status:    "waiting",
			SessionID: "bbbbbbbb-2222-4bbb-8bbb-bbbbbbbbbbbb",
			WaitingFor: &uiWaitingFor{
				Kind: "permission",
				Why:  "Would you like to run the following command?",
			},
		},
	}
	if got := srv.uiMonitor(agents).Unread; got != 1 {
		t.Fatalf("unread before dismiss = %d, want 1", got)
	}

	resp, status := srv.updateNotification(actionRequest{Kind: "notification-dismiss", Target: "agent-switcher-permission"})
	if status != 200 || !resp.OK {
		t.Fatalf("dismiss response = %#v status %d", resp, status)
	}
	monitor := srv.uiMonitor(agents)
	if monitor.Unread != 0 || len(monitor.Notifications) != 0 {
		t.Fatalf("monitor after dismiss = %+v, want no agent notification", monitor)
	}
}

func TestNotificationDismissAllDismissesRealAndSyntheticNotifications(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv := &Server{cfg: Config{DB: db}}
	event, _, err := flowdb.UpsertMonitorEvent(db, flowdb.MonitorEventInput{
		Source:   agentHookMonitorSource,
		Kind:     "stop",
		SourceID: "codex:session-1:stop:1",
		Title:    "codex switcher stopped",
		Severity: "info",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := flowdb.CreateNotificationForEvent(db, *event, "info"); err != nil {
		t.Fatal(err)
	}
	agents := []uiAgent{
		{
			Slug:      "switcher",
			Name:      "Switcher",
			Provider:  "codex",
			Status:    "waiting",
			SessionID: "session-2",
			WaitingFor: &uiWaitingFor{
				Kind: "permission",
				Why:  "Would you like to run the following command?",
			},
		},
	}
	monitor := srv.uiMonitor(agents)
	if monitor.Unread != 2 || len(monitor.Notifications) != 2 {
		t.Fatalf("monitor before dismiss all = %+v, want two unread notifications", monitor)
	}

	resp, status := srv.runAction(actionRequest{
		Kind:            "notification-dismiss-all",
		NotificationIDs: []string{"agent-switcher-permission", "notif-" + event.ID},
	})
	if status != 200 || !resp.OK {
		t.Fatalf("dismiss all response = %#v status %d", resp, status)
	}
	monitor = srv.uiMonitor(agents)
	if monitor.Unread != 0 || len(monitor.Notifications) != 0 {
		t.Fatalf("monitor after dismiss all = %+v, want no notifications", monitor)
	}
}

func TestNotificationReadAllKeepsNotificationsAndClearsUnread(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv := &Server{cfg: Config{DB: db}}
	event, _, err := flowdb.UpsertMonitorEvent(db, flowdb.MonitorEventInput{
		Source:   agentHookMonitorSource,
		Kind:     "session_start",
		SourceID: "claude:session-1:session_start:1",
		Title:    "claude switcher started",
		Severity: "info",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := flowdb.CreateNotificationForEvent(db, *event, "info"); err != nil {
		t.Fatal(err)
	}
	agents := []uiAgent{
		{
			Slug:      "switcher",
			Name:      "Switcher",
			Provider:  "codex",
			Status:    "waiting",
			SessionID: "session-2",
			WaitingFor: &uiWaitingFor{
				Kind: "question",
				Why:  "Which option should I pick?",
			},
		},
	}
	monitor := srv.uiMonitor(agents)
	if monitor.Unread != 2 || len(monitor.Notifications) != 2 {
		t.Fatalf("monitor before read all = %+v, want two unread notifications", monitor)
	}

	resp, status := srv.runAction(actionRequest{
		Kind:            "notification-read-all",
		NotificationIDs: []string{"agent-switcher-question", "notif-" + event.ID},
	})
	if status != 200 || !resp.OK {
		t.Fatalf("read all response = %#v status %d", resp, status)
	}
	monitor = srv.uiMonitor(agents)
	if monitor.Unread != 0 || len(monitor.Notifications) != 2 {
		t.Fatalf("monitor after read all = %+v, want visible read notifications", monitor)
	}
	for _, notification := range monitor.Notifications {
		if notification.Status != "read" {
			t.Fatalf("notification after read all = %+v, want read", notification)
		}
	}
}
