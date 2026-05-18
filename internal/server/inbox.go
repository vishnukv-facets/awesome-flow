package server

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"flow/internal/flowdb"
)

// handleTaskInbox dispatches GET (read) and POST (write) on
// /api/tasks/<slug>/inbox. Both paths are scoped to a single task and
// don't require authentication beyond the local-only HTTP listener.
func (s *Server) handleTaskInbox(w http.ResponseWriter, r *http.Request, task *flowdb.Task) {
	switch r.Method {
	case http.MethodGet:
		s.serveInbox(w, task)
	case http.MethodPost:
		s.appendInbox(w, r, task)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) serveInbox(w http.ResponseWriter, task *flowdb.Task) {
	path := inboxPath(s.cfg.FlowRoot, task.Slug)
	entries, err := readInboxEntries(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, fmt.Errorf("read inbox: %w", err), http.StatusInternalServerError)
		return
	}
	view := InboxView{
		Slug:        task.Slug,
		Path:        path,
		Entries:     entries,
		UnreadCount: unreadInboxCount(task, entries),
	}
	if task.InboxSeenAt.Valid {
		v := task.InboxSeenAt.String
		view.SeenAt = &v
	}
	writeJSON(w, view)
}

// appendInbox writes a new entry to inbox.md from the UI composer. Body:
// {"sender": "you", "message": "..."} — same shape `flow tell` writes.
func (s *Server) appendInbox(w http.ResponseWriter, r *http.Request, task *flowdb.Task) {
	defer r.Body.Close()
	var body struct {
		Sender  string `json:"sender"`
		Message string `json:"message"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil {
		writeError(w, fmt.Errorf("decode body: %w", err), http.StatusBadRequest)
		return
	}
	body.Sender = strings.TrimSpace(body.Sender)
	if body.Sender == "" {
		body.Sender = "user"
	}
	body.Message = strings.TrimSpace(body.Message)
	if body.Message == "" {
		writeError(w, fmt.Errorf("message is required"), http.StatusBadRequest)
		return
	}

	path := inboxPath(s.cfg.FlowRoot, task.Slug)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	stamp := time.Now().UTC().Format("2006-01-02 15:04:05Z")
	entry := fmt.Sprintf("## %s — from: %s\n\n%s\n\n", stamp, body.Sender, body.Message)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() == 0 {
		header := "# Inbox\n\nMessages from parent tasks and the user. The bound agent\n" +
			"reads new entries at the start of every session and acts on them.\n\n"
		if _, err := f.WriteString(header); err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
	}
	if _, err := f.WriteString(entry); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	if _, err := s.cfg.DB.Exec(
		`UPDATE tasks SET updated_at = ? WHERE slug = ?`,
		flowdb.NowISO(), task.Slug,
	); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}

	// Push a live event so other subscribers (sidebar badge) re-render
	// without polling.
	s.publishInboxChanged(task.Slug, body.Sender, body.Message)

	writeJSON(w, map[string]any{"ok": true})
}

// handleInboxNotify is the side-band endpoint `flow tell` POSTs to so
// the WS hub can fan an event out to UI subscribers. Body:
// {"task_slug": "...", "sender": "...", "preview": "..."}. We don't
// re-read the file here — the CLI already wrote it and bumped
// updated_at. We just publish.
func (s *Server) handleInboxNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var body struct {
		TaskSlug string `json:"task_slug"`
		Sender   string `json:"sender"`
		Preview  string `json:"preview"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	body.TaskSlug = strings.TrimSpace(body.TaskSlug)
	if body.TaskSlug == "" {
		writeError(w, fmt.Errorf("task_slug is required"), http.StatusBadRequest)
		return
	}
	s.publishInboxChanged(body.TaskSlug, body.Sender, body.Preview)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) publishInboxChanged(slug, sender, preview string) {
	if s.events == nil {
		return
	}
	s.events.publish(eventEnvelope{
		Type:     "inbox_changed",
		TaskSlug: slug,
		Data:     json.RawMessage(fmt.Sprintf(`{"sender":%q,"preview":%q}`, sender, truncateText(preview, 200))),
	})
}

// readInboxEntries parses ~/.flow/tasks/<slug>/inbox.md into structured
// entries. The format is deliberately simple — `## TIMESTAMP — from: WHO`
// headers separating message bodies. We avoid a markdown parser; this
// is one regex's worth of scanning.
func readInboxEntries(path string) ([]InboxEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []InboxEntry
	var current *InboxEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "## ") && strings.Contains(line, " — from: ") {
			if current != nil {
				current.Body = strings.TrimSpace(current.Body)
				entries = append(entries, *current)
			}
			head := strings.TrimPrefix(line, "## ")
			parts := strings.SplitN(head, " — from: ", 2)
			current = &InboxEntry{Timestamp: parts[0]}
			if len(parts) == 2 {
				current.Sender = parts[1]
			}
			continue
		}
		if current == nil {
			continue // pre-header preamble (`# Inbox` etc.)
		}
		current.Body += line + "\n"
	}
	if err := scanner.Err(); err != nil {
		return entries, err
	}
	if current != nil {
		current.Body = strings.TrimSpace(current.Body)
		entries = append(entries, *current)
	}
	return entries, nil
}

func unreadInboxCount(task *flowdb.Task, entries []InboxEntry) int {
	if len(entries) == 0 {
		return 0
	}
	if !task.InboxSeenAt.Valid || strings.TrimSpace(task.InboxSeenAt.String) == "" {
		return len(entries)
	}
	seenAt, err := time.Parse(time.RFC3339, task.InboxSeenAt.String)
	if err != nil {
		return len(entries)
	}
	count := 0
	for _, e := range entries {
		// Entry timestamps are stored as "2006-01-02 15:04:05Z" — parse
		// to compare; treat parse failure as "old enough" to avoid
		// over-counting on malformed lines.
		entryAt, err := time.Parse("2006-01-02 15:04:05Z", e.Timestamp)
		if err != nil {
			continue
		}
		if entryAt.After(seenAt) {
			count++
		}
	}
	return count
}

func inboxPath(flowRoot, slug string) string {
	return filepath.Join(flowRoot, "tasks", slug, "inbox.md")
}

// handleTaskLifecycle serves GET /api/tasks/<slug>/lifecycle: a
// chronologically-newest-first slice of monitor_events for this task's
// session, mapped to the simple (time, kind, status) shape the UI
// timeline renders.
func (s *Server) handleTaskLifecycle(w http.ResponseWriter, r *http.Request, task *flowdb.Task) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !task.SessionID.Valid || strings.TrimSpace(task.SessionID.String) == "" {
		writeJSON(w, LifecycleView{Slug: task.Slug, Events: []LifecycleEvent{}})
		return
	}
	provider := task.SessionProvider
	if provider == "" {
		provider = "claude"
	}
	prefix := agentHookSourceIDPrefix(provider, task.SessionID.String)
	rows, err := s.cfg.DB.Query(
		`SELECT last_seen_at, kind, severity, body
		 FROM monitor_events
		 WHERE source = ? AND source_id LIKE ?
		 ORDER BY last_seq DESC, last_seen_at DESC
		 LIMIT 100`,
		agentHookMonitorSource, prefix+"%",
	)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	view := LifecycleView{Slug: task.Slug}
	for rows.Next() {
		var seenAt, kind, severity string
		var body sql.NullString
		if err := rows.Scan(&seenAt, &kind, &severity, &body); err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		ev := LifecycleEvent{
			Time:     seenAt,
			Kind:     kind,
			Status:   agentHookRuntimeStatus(kind),
			Severity: severity,
		}
		if body.Valid {
			ev.Body = truncateText(body.String, 200)
		}
		view.Events = append(view.Events, ev)
	}
	if err := rows.Err(); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, view)
}
