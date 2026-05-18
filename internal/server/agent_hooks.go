package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"flow/internal/flowdb"
)

const (
	agentHookMonitorSource       = "agent_hook"
	agentTranscriptMonitorSource = "agent_transcript"
)

// CurrentHookVersion is bumped whenever the agent-event hook wire format
// or contract changes in a way that older installed copies should be
// nudged to upgrade. The installer in internal/agenthooks stamps this
// value via `--hook-version` into every registered hook command line;
// when the server ingests a hook payload tagged with a lower version it
// surfaces an upgrade hint at the next SessionStart.
//
// Bumped to 3 when the SessionStart hook added inbox-message detection
// (appendInboxHint in internal/app/hook.go) — old installs still work,
// they just don't surface the INBOX UPDATED nudge.
const CurrentHookVersion = 3

type agentHookIngestResponse struct {
	OK             bool   `json:"ok"`
	Provider       string `json:"provider,omitempty"`
	Event          string `json:"event,omitempty"`
	Kind           string `json:"kind,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	Task           string `json:"task,omitempty"`
	NotificationID string `json:"notification_id,omitempty"`
	Seq            int64  `json:"seq,omitempty"`
	HookOwned      bool   `json:"hook_owned,omitempty"`
	HookVersion    int    `json:"hook_version,omitempty"`
	SubagentID     string `json:"subagent_id,omitempty"`
	HookOutdated   bool   `json:"hook_outdated,omitempty"`
}

func (s *Server) handleAgentHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var payload map[string]any
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&payload); err != nil {
		writeError(w, fmt.Errorf("decode hook payload: %w", err), http.StatusBadRequest)
		return
	}
	raw, _ := json.Marshal(payload)
	resp, err := s.ingestAgentHook(r, payload, string(raw))
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, resp)
}

func (s *Server) ingestAgentHook(r *http.Request, payload map[string]any, raw string) (agentHookIngestResponse, error) {
	provider := agentHookProvider(r, payload)
	eventName := agentHookString(payload, "hook_event_name", "hookEventName")
	if eventName == "" {
		return agentHookIngestResponse{}, fmt.Errorf("hook_event_name is required")
	}
	sessionID := agentHookString(payload, "session_id", "sessionID")
	if sessionID == "" {
		sessionID = agentHookString(payload, "thread_id", "threadID")
	}
	kind := agentHookKind(eventName, payload)
	if kind == "" {
		kind = normalizeAgentHookPart(eventName)
	}
	// Side-band metadata stamped by `flow hook agent-event`. Older hook
	// installs that don't set these fields fall back to 0/false, which
	// the upserts treat as "no opinion" and apply unconditionally.
	seq := agentHookInt64(payload, "flow_seq", "flowSeq")
	hookOwned := agentHookBool(payload, "flow_hook_owned", "flowHookOwned")
	hookVersion := agentHookInt(payload, "flow_hook_version", "flowHookVersion")
	subagentID := agentHookString(payload, "agent_id", "agentID", "subagent_id", "subagentID")

	resp := agentHookIngestResponse{
		OK:           true,
		Provider:     provider,
		Event:        eventName,
		Kind:         kind,
		SessionID:    sessionID,
		Seq:          seq,
		HookOwned:    hookOwned,
		HookVersion:  hookVersion,
		SubagentID:   subagentID,
		HookOutdated: hookVersion > 0 && hookVersion < CurrentHookVersion,
	}
	if sessionID != "" {
		if task, err := flowdb.TaskBySessionID(s.cfg.DB, sessionID); err == nil {
			resp.Task = task.Slug
		}
	}
	body := agentHookBody(payload)
	// Subagent stop/start events don't represent the parent agent's state.
	// Recording them on agent_runtime_states would flicker the parent
	// between idle and running. We still log them to monitor_events so the
	// UI can show subagent activity per-task; we just don't flip the
	// session-level state.
	if runtimeStatus := agentHookRuntimeStatus(kind); runtimeStatus != "" && sessionID != "" && !isSubagentEvent(kind, subagentID) {
		if err := flowdb.UpsertAgentRuntimeState(s.cfg.DB, flowdb.AgentRuntimeStateInput{
			Provider:  provider,
			SessionID: sessionID,
			TaskSlug:  resp.Task,
			Status:    runtimeStatus,
			EventKind: kind,
			Message:   body,
			Seq:       seq,
			RawJSON:   raw,
		}); err != nil {
			return agentHookIngestResponse{}, err
		}
	}

	if agentHookClearsAttention(eventName, payload) && sessionID != "" && !isSubagentEvent(kind, subagentID) {
		_ = s.clearAgentHookAttention(provider, sessionID)
	}
	if agentHookIsLowValueEvent(kind) {
		s.publishHookEvent(resp, payload)
		return resp, nil
	}

	sourceID := agentHookSourceID(provider, sessionID, kind, payload)
	title := agentHookTitle(provider, kind, resp.Task, payload)
	event, _, err := flowdb.UpsertMonitorEvent(s.cfg.DB, flowdb.MonitorEventInput{
		Source:   agentHookMonitorSource,
		Kind:     kind,
		SourceID: sourceID,
		Title:    title,
		Body:     body,
		URL:      agentHookURL(resp.Task),
		Severity: agentHookSeverity(kind),
		Seq:      seq,
		RawJSON:  raw,
	})
	if err != nil {
		return agentHookIngestResponse{}, err
	}
	if agentHookShouldNotify(kind) {
		level := agentHookNotificationLevel(kind)
		if err := flowdb.CreateNotificationForEvent(s.cfg.DB, *event, level); err != nil {
			return agentHookIngestResponse{}, err
		}
		resp.NotificationID = "notif-" + event.ID
	}
	s.publishHookEvent(resp, payload)
	return resp, nil
}

// isSubagentEvent returns true for events emitted by a Claude subagent (a
// nested agent_id is set) — these must not flip the parent's session
// state or clear parent-owned attention rows. SubagentStart/SubagentStop
// kinds are always subagent-scoped regardless of payload shape.
func isSubagentEvent(kind, subagentID string) bool {
	switch kind {
	case "subagent_start", "subagent_stop":
		return true
	}
	return strings.TrimSpace(subagentID) != ""
}

func (s *Server) clearAgentHookAttention(provider, sessionID string) error {
	prefix := agentHookSourceIDPrefix(provider, sessionID)
	rows, err := s.cfg.DB.Query(
		`SELECT id FROM monitor_events
		 WHERE source = ? AND source_id LIKE ? AND status IN ('new','notified')
		   AND kind IN ('permission_request','permission_prompt','elicitation','elicitation_dialog','idle_prompt')`,
		agentHookMonitorSource, prefix+"%",
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		_ = flowdb.UpdateMonitorEventStatus(s.cfg.DB, id, "done")
		_ = flowdb.UpdateNotificationStatus(s.cfg.DB, "notif-"+id, "actioned")
	}
	return nil
}

func (s *Server) agentHookWaitingFor(tv TaskView) *uiWaitingFor {
	if tv.SessionID == nil || strings.TrimSpace(*tv.SessionID) == "" {
		return nil
	}
	provider := "claude"
	if tv.SessionProvider != nil && strings.TrimSpace(*tv.SessionProvider) != "" {
		provider = *tv.SessionProvider
	}
	prefix := agentHookSourceIDPrefix(provider, *tv.SessionID)
	row := s.cfg.DB.QueryRow(
		`SELECT kind, title, body FROM monitor_events
		 WHERE source = ? AND source_id LIKE ? AND status IN ('new','notified')
		   AND kind IN ('permission_request','permission_prompt','elicitation','elicitation_dialog','idle_prompt')
		 ORDER BY last_seen_at DESC LIMIT 1`,
		agentHookMonitorSource, prefix+"%",
	)
	var kind, title string
	var body sql.NullString
	if err := row.Scan(&kind, &title, &body); err != nil {
		return nil
	}
	waitKind := "agent"
	switch kind {
	case "permission_request", "permission_prompt":
		waitKind = "permission"
	case "elicitation", "elicitation_dialog", "idle_prompt":
		waitKind = "question"
	}
	why := title
	if body.Valid && strings.TrimSpace(body.String) != "" {
		why = body.String
	}
	return &uiWaitingFor{Kind: waitKind, Cmd: "Open session " + tv.Slug, Why: truncateText(why, 220)}
}

func (s *Server) agentHookRuntimeState(tv TaskView, provider string) *flowdb.AgentRuntimeState {
	if tv.SessionID == nil || strings.TrimSpace(*tv.SessionID) == "" {
		return nil
	}
	state, err := flowdb.AgentRuntimeStateBySessionID(s.cfg.DB, provider, *tv.SessionID)
	if err != nil {
		state = s.agentHookRuntimeStateFromMonitorEvents(provider, *tv.SessionID)
		if state == nil {
			return nil
		}
	}
	if runtimeStateBeforeSessionStart(state.UpdatedAt, tv) {
		return nil
	}
	return state
}

func (s *Server) agentHookHealth(tv TaskView, provider string, transcript []uiTranscript, state *flowdb.AgentRuntimeState) *uiHookHealth {
	if provider != "codex" || tv.SessionID == nil || strings.TrimSpace(*tv.SessionID) == "" {
		return nil
	}
	hookStatus := codexLocalHookStatus(tv.WorkDir)
	if !hookStatus.installed {
		message := "Flow Codex hooks are not installed in this workdir yet, so live notifications may be stale."
		if hookStatus.stale {
			message = "Flow Codex hooks still point at an old command in this workdir, so live notifications may be stale."
		}
		return &uiHookHealth{
			Status:  "missing",
			Message: message,
			Action:  "Reopen or resume this session from Flow to install repo-local hooks.",
		}
	}
	if hookStatus.trusted {
		return nil
	}
	return &uiHookHealth{
		Status:  "needs_approval",
		Message: "Codex is blocking Flow hooks until they are reviewed, so questions and stop/start notifications may be incomplete.",
		Action:  "Open this Codex session and run /hooks, then approve the Flow hooks.",
	}
}

func codexManagedHooksInstalled(workDir string) bool {
	return codexLocalHookStatus(workDir).installed
}

type codexHookStatus struct {
	installed bool
	stale     bool
	trusted   bool
}

func codexLocalHookStatus(workDir string) codexHookStatus {
	hooksPath := filepath.Join(workDir, ".codex", "hooks.json")
	body, err := os.ReadFile(hooksPath)
	if err != nil {
		return codexHookStatus{}
	}
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return codexHookStatus{}
	}
	status := codexHookStatus{}
	entries := codexManagedHookEntries(hooksPath, raw)
	for _, entry := range entries {
		if !strings.Contains(entry.command, "hook agent-event") || hookCommandProvider(entry.command) != "codex" {
			continue
		}
		if hookCommandUsesBareFlow(entry.command) {
			status.installed = true
			continue
		}
		status.stale = true
	}
	if status.installed {
		status.trusted = codexHookStatesTrusted(codexConfigPath(), entries)
	}
	return status
}

type codexHookEntry struct {
	sourcePath string
	event      string
	groupIndex int
	hookIndex  int
	command    string
}

func codexManagedHookEntries(hooksPath string, raw any) []codexHookEntry {
	cfg, _ := raw.(map[string]any)
	hooks, _ := cfg["hooks"].(map[string]any)
	if len(hooks) == 0 {
		return nil
	}
	sourcePath := hooksPath
	if abs, err := filepath.Abs(hooksPath); err == nil {
		sourcePath = abs
	}
	out := []codexHookEntry{}
	for eventName, eventGroups := range hooks {
		event := codexStateEventName(eventName)
		groups, ok := eventGroups.([]any)
		if !ok || event == "" {
			continue
		}
		for groupIndex, groupRaw := range groups {
			group, _ := groupRaw.(map[string]any)
			hookList, _ := group["hooks"].([]any)
			for hookIndex, hookRaw := range hookList {
				hook, _ := hookRaw.(map[string]any)
				command, _ := hook["command"].(string)
				if strings.Contains(command, "hook agent-event") && hookCommandProvider(command) == "codex" {
					out = append(out, codexHookEntry{
						sourcePath: sourcePath,
						event:      event,
						groupIndex: groupIndex,
						hookIndex:  hookIndex,
						command:    command,
					})
				}
			}
		}
	}
	return out
}

func hookCommandProvider(command string) string {
	fields := strings.Fields(command)
	for i, field := range fields {
		field = strings.Trim(field, `"'`)
		if field == "--provider" && i+1 < len(fields) {
			return strings.Trim(fields[i+1], `"'`)
		}
		if strings.HasPrefix(field, "--provider=") {
			return strings.Trim(strings.TrimPrefix(field, "--provider="), `"'`)
		}
	}
	return ""
}

func hookCommandUsesBareFlow(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	return strings.Trim(fields[0], `"'`) == "flow"
}

func codexHookStatesTrusted(configPath string, entries []codexHookEntry) bool {
	flowEntries := []codexHookEntry{}
	for _, entry := range entries {
		if hookCommandUsesBareFlow(entry.command) {
			flowEntries = append(flowEntries, entry)
		}
	}
	if len(flowEntries) == 0 {
		return false
	}
	state, err := readCodexTrustedHookState(configPath)
	if err != nil {
		return false
	}
	for _, entry := range flowEntries {
		if !state[entry.stateKey()] {
			return false
		}
	}
	return true
}

func readCodexTrustedHookState(configPath string) (map[string]bool, error) {
	body, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	state := map[string]bool{}
	current := ""
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[hooks.state.\"") && strings.HasSuffix(trimmed, "\"]") {
			current = strings.TrimSuffix(strings.TrimPrefix(trimmed, "[hooks.state.\""), "\"]")
			state[current] = false
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			current = ""
			continue
		}
		if current != "" && strings.HasPrefix(trimmed, "trusted_hash") && strings.Contains(trimmed, "\"sha256:") {
			state[current] = true
		}
	}
	return state, nil
}

func (entry codexHookEntry) stateKey() string {
	return fmt.Sprintf("%s:%s:%d:%d", entry.sourcePath, entry.event, entry.groupIndex, entry.hookIndex)
}

func codexConfigPath() string {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		return filepath.Join(home, "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".codex", "config.toml")
}

func codexStateEventName(event string) string {
	event = strings.TrimSpace(event)
	if event == "" {
		return ""
	}
	out := strings.Builder{}
	var prev rune
	for i, r := range event {
		if r == '-' || r == ' ' {
			r = '_'
		}
		if unicode.IsUpper(r) {
			if i > 0 && prev != '_' {
				out.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		out.WriteRune(r)
		prev = r
	}
	return out.String()
}

func (s *Server) agentHookRuntimeStateFromMonitorEvents(provider, sessionID string) *flowdb.AgentRuntimeState {
	prefix := agentHookSourceIDPrefix(provider, sessionID)
	rows, err := s.cfg.DB.Query(
		`SELECT kind, status, body, last_seen_at, raw_json
		 FROM monitor_events
		 WHERE source = ? AND source_id LIKE ?
		 ORDER BY last_seen_at DESC
		 LIMIT 20`,
		agentHookMonitorSource, prefix+"%",
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var kind, eventStatus, lastSeen string
		var body, raw sql.NullString
		if err := rows.Scan(&kind, &eventStatus, &body, &lastSeen, &raw); err != nil {
			return nil
		}
		if agentHookAttentionKind(kind) && eventStatus != "new" && eventStatus != "notified" {
			continue
		}
		runtimeStatus := agentHookRuntimeStatus(kind)
		if runtimeStatus == "" {
			continue
		}
		return &flowdb.AgentRuntimeState{
			Provider:  provider,
			SessionID: sessionID,
			Status:    runtimeStatus,
			EventKind: kind,
			Message:   body,
			UpdatedAt: lastSeen,
			RawJSON:   raw,
		}
	}
	return nil
}

func runtimeStateBeforeSessionStart(updatedAt string, tv TaskView) bool {
	baseline := ""
	if tv.SessionStarted != nil {
		baseline = laterTimestamp(baseline, *tv.SessionStarted)
	}
	if tv.SessionLastResumed != nil {
		baseline = laterTimestamp(baseline, *tv.SessionLastResumed)
	}
	if baseline == "" || updatedAt == "" {
		return false
	}
	updated, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		return updatedAt < baseline
	}
	base, err := time.Parse(time.RFC3339, baseline)
	if err != nil {
		return updatedAt < baseline
	}
	return updated.Before(base)
}

func agentHookProvider(r *http.Request, payload map[string]any) string {
	provider := ""
	if r != nil {
		provider = strings.TrimSpace(r.URL.Query().Get("provider"))
	}
	if provider == "" {
		provider = agentHookString(payload, "provider", "session_provider", "sessionProvider")
	}
	if provider == "" {
		path := agentHookString(payload, "transcript_path", "transcriptPath")
		switch {
		case strings.Contains(path, ".codex"):
			provider = "codex"
		case strings.Contains(path, ".claude"):
			provider = "claude"
		}
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "codex":
		return "codex"
	default:
		return "claude"
	}
}

func agentHookKind(eventName string, payload map[string]any) string {
	event := normalizeAgentHookPart(eventName)
	if event == "notification" {
		if typ := normalizeAgentHookPart(agentHookString(payload, "notification_type", "notificationType")); typ != "" {
			return typ
		}
	}
	if event == "pre_tool_use" {
		tool := normalizeAgentHookPart(agentHookString(payload, "tool_name", "toolName"))
		switch {
		case tool == "ask_user_question", tool == "exit_plan_mode", tool == "request_user_input", strings.Contains(tool, "request_user_input"):
			return "elicitation"
		}
	}
	if event == "teammate_idle" {
		return "idle_prompt"
	}
	return event
}

func agentHookClearsAttention(eventName string, payload map[string]any) bool {
	switch normalizeAgentHookPart(eventName) {
	case "user_prompt_submit", "post_tool_use", "post_tool_use_failure", "post_tool_batch", "elicitation_result", "permission_denied", "session_start", "stop", "stop_failure", "session_end":
		return true
	case "notification":
		switch normalizeAgentHookPart(agentHookString(payload, "notification_type", "notificationType")) {
		case "elicitation_complete", "elicitation_response", "auth_success":
			return true
		}
	}
	return false
}

func agentHookIsLowValueEvent(kind string) bool {
	switch kind {
	case "pre_tool_use", "post_tool_use", "post_tool_use_failure", "post_tool_batch", "user_prompt_submit", "elicitation_result":
		return true
	default:
		return false
	}
}

func agentHookShouldNotify(kind string) bool {
	switch kind {
	case "permission_request", "permission_prompt", "elicitation", "elicitation_dialog", "idle_prompt", "permission_denied", "session_start", "subagent_start", "subagent_stop", "task_created", "task_completed", "stop", "stop_failure", "session_end":
		return true
	default:
		return false
	}
}

func agentHookRuntimeStatus(kind string) string {
	switch kind {
	case "permission_request", "permission_prompt", "elicitation", "elicitation_dialog", "idle_prompt":
		return "waiting"
	case "stop", "task_completed":
		// Stop is between-turn: the agent finished a turn and is waiting
		// for the next user input. Session is resumable; the session_id
		// stays useful for `flow do <slug>`.
		return "idle"
	case "session_start":
		// SessionStart fires when the host opens a fresh or resumed session.
		// At that instant the agent is sitting at the prompt waiting for the
		// user's next input — not actively running. Marking this as
		// "running" pins the session to running indefinitely when no later
		// hooks fire (e.g., user opens the session and walks away).
		return "idle"
	case "session_end":
		// SessionEnd is terminal: the user :q'd, the process exited, or
		// the host tore the session down. The session_id is no longer
		// resumable — a future flow do should spawn fresh rather than
		// pass --resume against a dead transcript. UI surfaces this
		// distinctly from idle.
		return "released"
	case "stop_failure":
		return "dead"
	case "subagent_start", "subagent_stop", "task_created", "user_prompt_submit", "pre_tool_use", "post_tool_use", "post_tool_use_failure", "post_tool_batch", "elicitation_result", "permission_denied":
		return "running"
	default:
		return ""
	}
}

func agentHookAttentionKind(kind string) bool {
	switch kind {
	case "permission_request", "permission_prompt", "elicitation", "elicitation_dialog", "idle_prompt":
		return true
	default:
		return false
	}
}

func agentHookLifecycleKind(kind string) bool {
	switch kind {
	case "session_start", "session_end", "task_created", "task_completed", "subagent_start", "subagent_stop", "stop", "stop_failure":
		return true
	default:
		return false
	}
}

func agentHookNotificationLevel(kind string) string {
	switch kind {
	case "permission_request", "permission_prompt", "elicitation", "elicitation_dialog", "idle_prompt":
		return "approval"
	case "permission_denied":
		return "warning"
	case "stop_failure":
		return "error"
	case "session_start", "subagent_start", "subagent_stop", "task_completed", "session_end", "stop":
		return "info"
	default:
		return "info"
	}
}

func agentHookSeverity(kind string) string {
	switch agentHookNotificationLevel(kind) {
	case "approval", "error":
		return "high"
	case "warning":
		return "medium"
	default:
		return "low"
	}
}

func agentHookTitle(provider, kind, task string, payload map[string]any) string {
	label := provider
	if task != "" {
		label += " " + task
	}
	switch kind {
	case "permission_request", "permission_prompt":
		return label + " needs approval"
	case "permission_denied":
		return label + " permission denied"
	case "elicitation", "elicitation_dialog", "idle_prompt":
		return label + " needs input"
	case "stop":
		return label + " stopped"
	case "stop_failure":
		return label + " stopped with an error"
	case "session_start":
		return label + " started"
	case "subagent_start":
		if agentType := agentHookString(payload, "agent_type", "agentType"); agentType != "" {
			return label + " subagent started: " + agentType
		}
		return label + " subagent started"
	case "subagent_stop":
		if agentType := agentHookString(payload, "agent_type", "agentType"); agentType != "" {
			return label + " subagent stopped: " + agentType
		}
		return label + " subagent stopped"
	case "task_created":
		if subject := agentHookString(payload, "task_subject", "taskSubject"); subject != "" {
			return label + " created task: " + subject
		}
		return label + " created a task"
	case "task_completed":
		if subject := agentHookString(payload, "task_subject", "taskSubject"); subject != "" {
			return label + " completed task: " + subject
		}
		return label + " completed a task"
	case "session_end":
		reason := agentHookString(payload, "reason")
		if reason != "" {
			return label + " session ended: " + reason
		}
		return label + " session ended"
	default:
		return label + " " + strings.ReplaceAll(kind, "_", " ")
	}
}

func agentHookBody(payload map[string]any) string {
	for _, key := range []string{"message", "title", "reason", "last_assistant_message", "prompt"} {
		if value := agentHookString(payload, key); value != "" {
			return truncateText(value, 600)
		}
	}
	tool := agentHookString(payload, "tool_name", "toolName")
	if tool != "" {
		if b, ok := payload["tool_input"]; ok {
			if raw, err := json.Marshal(b); err == nil {
				return truncateText(tool+" "+string(raw), 600)
			}
		}
		return tool
	}
	return ""
}

func agentHookURL(task string) string {
	if task == "" {
		return ""
	}
	return "/session/" + task
}

func agentHookSourceID(provider, sessionID, kind string, payload map[string]any) string {
	parts := []string{provider}
	if sessionID != "" {
		parts = append(parts, sessionID)
	} else if path := agentHookString(payload, "transcript_path", "transcriptPath"); path != "" {
		parts = append(parts, path)
	} else {
		parts = append(parts, agentHookString(payload, "cwd"))
	}
	parts = append(parts, kind)
	for _, key := range []string{"tool_use_id", "toolUseID", "turn_id", "turnID", "notification_type", "notificationType", "reason"} {
		if v := agentHookString(payload, key); v != "" {
			parts = append(parts, v)
			return strings.Join(parts, ":")
		}
	}
	parts = append(parts, fmt.Sprintf("%d", time.Now().UnixNano()))
	return strings.Join(parts, ":")
}

func agentHookSourceIDPrefix(provider, sessionID string) string {
	return provider + ":" + sessionID + ":"
}

func agentHookInt64(payload map[string]any, keys ...string) int64 {
	for _, key := range keys {
		v, ok := payload[key]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int64(n)
		case int:
			return int64(n)
		case int64:
			return n
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return i
			}
		case string:
			if i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
				return i
			}
		}
	}
	return 0
}

func agentHookInt(payload map[string]any, keys ...string) int {
	return int(agentHookInt64(payload, keys...))
}

func agentHookBool(payload map[string]any, keys ...string) bool {
	for _, key := range keys {
		v, ok := payload[key]
		if !ok {
			continue
		}
		switch b := v.(type) {
		case bool:
			return b
		case string:
			s := strings.ToLower(strings.TrimSpace(b))
			return s == "true" || s == "1" || s == "yes"
		case float64:
			return b != 0
		}
	}
	return false
}

func agentHookString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key]; ok {
			switch v := value.(type) {
			case string:
				if strings.TrimSpace(v) != "" {
					return strings.TrimSpace(v)
				}
			case fmt.Stringer:
				if strings.TrimSpace(v.String()) != "" {
					return strings.TrimSpace(v.String())
				}
			}
		}
	}
	return ""
}

func normalizeAgentHookPart(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	var prevLower bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			if prevLower {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
			prevLower = false
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			prevLower = true
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			prevLower = false
		default:
			if b.Len() > 0 {
				b.WriteByte('_')
			}
			prevLower = false
		}
	}
	out := b.String()
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	return strings.Trim(out, "_")
}
