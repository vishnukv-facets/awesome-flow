package flowdb

import (
	"database/sql"
	"fmt"
	"strings"
)

// AgentRuntimeStateInput is the upsert payload Server.handleAgentHook writes
// when Claude Code or Codex emits a session lifecycle event (SessionStart,
// PostToolUse, Stop, etc.). Provider must be "claude" or "codex"; Status
// must be one of the runtime statuses below. Seq is the monotonic per-
// session sequence number when the hook supplies one (0 falls back to
// unconditional apply for legacy hooks).
type AgentRuntimeStateInput struct {
	Provider  string
	SessionID string
	TaskSlug  string
	Status    string
	EventKind string
	Message   string
	Seq       int64
	RawJSON   string
}

// UpsertAgentRuntimeState records the latest runtime state for a session.
// Idempotent on (provider, session_id). Conditional apply: if the incoming
// seq is older than the stored row, the update is dropped (stale event).
// seq=0 always applies for backwards-compat with pre-seq hooks.
func UpsertAgentRuntimeState(db *sql.DB, input AgentRuntimeStateInput) error {
	provider := strings.ToLower(strings.TrimSpace(input.Provider))
	switch provider {
	case "claude", "codex":
	default:
		return fmt.Errorf("invalid agent runtime provider %q", input.Provider)
	}
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		return fmt.Errorf("agent runtime session_id is required")
	}
	status := strings.ToLower(strings.TrimSpace(input.Status))
	switch status {
	case "running", "waiting", "idle", "dead", "released":
	default:
		return fmt.Errorf("invalid agent runtime status %q", input.Status)
	}
	eventKind := strings.ToLower(strings.TrimSpace(input.EventKind))
	eventKind = strings.ReplaceAll(eventKind, "-", "_")
	if eventKind == "" {
		eventKind = status
	}
	_, err := db.Exec(
		`INSERT INTO agent_runtime_states (
			provider, session_id, task_slug, status, event_kind, message, updated_at, last_seq, raw_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, session_id) DO UPDATE SET
			task_slug = excluded.task_slug,
			status = excluded.status,
			event_kind = excluded.event_kind,
			message = excluded.message,
			updated_at = excluded.updated_at,
			last_seq = MAX(excluded.last_seq, agent_runtime_states.last_seq),
			raw_json = excluded.raw_json
		WHERE excluded.last_seq = 0
		   OR excluded.last_seq >= agent_runtime_states.last_seq`,
		provider, sessionID, nullStringOrTrimmed(input.TaskSlug), status, eventKind,
		nullStringOrTrimmed(input.Message), NowISO(), input.Seq, nullStringOrTrimmed(input.RawJSON),
	)
	return err
}

// AgentRuntimeStateBySessionID returns the most recent runtime state row for
// (provider, sessionID), or sql.ErrNoRows when none. Empty inputs short-circuit
// to ErrNoRows rather than scanning.
func AgentRuntimeStateBySessionID(db *sql.DB, provider, sessionID string) (*AgentRuntimeState, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	sessionID = strings.TrimSpace(sessionID)
	if provider == "" || sessionID == "" {
		return nil, sql.ErrNoRows
	}
	row := db.QueryRow(
		`SELECT provider, session_id, task_slug, status, event_kind, message, updated_at, last_seq, raw_json
		 FROM agent_runtime_states
		 WHERE provider = ? AND session_id = ?`,
		provider, sessionID,
	)
	var state AgentRuntimeState
	if err := row.Scan(&state.Provider, &state.SessionID, &state.TaskSlug, &state.Status, &state.EventKind, &state.Message, &state.UpdatedAt, &state.LastSeq, &state.RawJSON); err != nil {
		return nil, err
	}
	return &state, nil
}

func nullStringOrTrimmed(s string) sql.NullString {
	s = strings.TrimSpace(s)
	return sql.NullString{String: s, Valid: s != ""}
}
