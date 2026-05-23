# Same-Session Event Monitor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a generic task inbox monitor that wakes the already-running Flow-owned Claude or Codex session when Slack, GitHub, or future source events append actionable entries to a task's `inbox.jsonl`.

**Architecture:** Source listeners keep one responsibility: translate external events into normalized inbox entries. A task-local monitor tails `inbox.jsonl`, filters for actionable entries, and wakes the task's existing terminal session by injecting a short prompt into the Flow-owned PTY. GitHub PR linkage is stored as task tags (`gh-pr:owner/repo#number`) so PR reviews and comments route back to the originating task.

**Tech Stack:** Go, SQLite via `modernc.org/sqlite`, Flow task tags, existing `internal/monitor` listeners, existing `internal/server` terminal bridge, `gh` CLI for optional PR discovery.

---

## File Structure

- `internal/monitor/inbound_event.go`: extend normalized source event payloads with URL metadata.
- `internal/monitor/inbox.go`: keep append/read helpers and add monitor-specific cursor helpers.
- `internal/monitor/inbox_test.go`: cover event metadata, backward-compatible reads, and monitor cursor persistence.
- `internal/monitor/inbox_monitor.go`: new reusable scanner that reads new inbox entries and calls a wake target for actionable batches.
- `internal/monitor/inbox_monitor_test.go`: test scan, batching, cursor advancement, and retry behavior.
- `internal/server/inbox_wake.go`: new server-side wake target that formats a concise prompt and writes it into the task terminal.
- `internal/server/inbox_monitor_manager.go`: new lifecycle manager for one monitor loop per live task terminal.
- `internal/server/terminal_bridge.go`: expose safe same-session wake helpers and start/stop monitors with terminal lifecycle.
- `internal/server/types.go`: add the inbox monitor manager field to `Server`.
- `internal/server/server.go`: initialize the monitor manager.
- `internal/server/server_test.go`: verify wake injection and monitor lifecycle without launching real Codex.
- `internal/monitor/github_event.go`: add top-level PR review event kinds and URL propagation.
- `internal/monitor/github_client.go`: poll GitHub PR reviews in addition to review comments.
- `internal/monitor/github_client_test.go`: cover review API parsing and event key stability.
- `internal/monitor/github_dispatcher_test.go`: cover review routing to tracked `gh-pr:` tasks.
- `internal/ghref/ghref.go`: new parser for GitHub PR URLs to Flow task tags.
- `internal/ghref/ghref_test.go`: cover accepted and rejected PR URL shapes.
- `internal/app/github_pr_link.go`: discover the current branch PR with `gh pr view --json url` and add a `gh-pr:` tag.
- `internal/app/done.go`: call PR-link discovery during close-out.
- `internal/app/done_test.go`: verify PR tag discovery and non-fatal `gh` failures.
- `internal/app/skill/SKILL.md`: document the generic same-session monitor contract for agents.
- `internal/app/skill_test.go`: ensure the embedded skill documents the generic monitor and Codex same-session wake path.
- `README.md`: document task inbox monitor behavior for users.

## Implementation Tasks

### Task 1: Normalize Inbox Metadata

**Files:**
- Modify: `internal/monitor/inbound_event.go`
- Modify: `internal/monitor/inbox.go`
- Test: `internal/monitor/inbox_test.go`

- [ ] **Step 1: Write failing metadata tests**

Add tests that prove new writes include metadata, old JSONL entries still read, and the monitor cursor is independent from the existing Slack cursor.

```go
func TestAppendInboxEventAddsClassifiedMeta(t *testing.T) {
	root := t.TempDir()
	ev := InboundEvent{
		Kind:        "pr_review_comment",
		ChannelType: "github",
		Text:        "please rename this helper",
		URL:         "https://github.com/acme/app/pull/12#discussion_r1",
	}

	if err := AppendInboxEvent(root, "fix-pr", ev); err != nil {
		t.Fatalf("AppendInboxEvent() error = %v", err)
	}

	entries, err := ReadInboxEntries(root, "fix-pr")
	if err != nil {
		t.Fatalf("ReadInboxEntries() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entry count = %d, want 1", len(entries))
	}
	if entries[0].Meta.Source != "github" {
		t.Fatalf("source = %q, want github", entries[0].Meta.Source)
	}
	if !entries[0].Meta.Actionable {
		t.Fatalf("actionable = false, want true")
	}
	if entries[0].Event.URL != ev.URL {
		t.Fatalf("url = %q, want %q", entries[0].Event.URL, ev.URL)
	}
}

func TestReadInboxEntriesAcceptsLegacyRowsWithoutMeta(t *testing.T) {
	root := t.TempDir()
	path := InboxPath(root, "legacy")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	legacy := `{"enqueued_at":"2026-05-23T10:00:00Z","event":{"kind":"message","channel_type":"slack","text":"ping"}}` + "\n"
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	entries, err := ReadInboxEntries(root, "legacy")
	if err != nil {
		t.Fatalf("ReadInboxEntries() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entry count = %d, want 1", len(entries))
	}
	if entries[0].Meta.Source != "" {
		t.Fatalf("legacy source = %q, want empty", entries[0].Meta.Source)
	}
}

func TestInboxMonitorCursorIsSeparateFromSlackCursor(t *testing.T) {
	root := t.TempDir()
	if err := WriteInboxCursor(root, "task-a", "1716460000.000100"); err != nil {
		t.Fatalf("WriteInboxCursor() error = %v", err)
	}
	if err := WriteInboxMonitorCursor(root, "task-a", 64); err != nil {
		t.Fatalf("WriteInboxMonitorCursor() error = %v", err)
	}

	slackCursor, err := ReadInboxCursor(root, "task-a")
	if err != nil {
		t.Fatalf("ReadInboxCursor() error = %v", err)
	}
	monitorCursor, err := ReadInboxMonitorCursor(root, "task-a")
	if err != nil {
		t.Fatalf("ReadInboxMonitorCursor() error = %v", err)
	}
	if slackCursor != "1716460000.000100" {
		t.Fatalf("slack cursor = %q", slackCursor)
	}
	if monitorCursor != 64 {
		t.Fatalf("monitor cursor = %d, want 64", monitorCursor)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
env FLOW_TASK= GOCACHE=/private/tmp/flow-manager-go-build-cache go test ./internal/monitor -run 'TestAppendInboxEventAddsClassifiedMeta|TestReadInboxEntriesAcceptsLegacyRowsWithoutMeta|TestInboxMonitorCursorIsSeparateFromSlackCursor' -count=1
```

Expected: fail because `InboundEvent.URL`, `InboxEntry.Meta`, and monitor cursor helpers do not exist.

- [ ] **Step 3: Implement minimal metadata and cursor helpers**

Add URL support and classification in `internal/monitor/inbound_event.go`:

```go
type InboundEvent struct {
	Kind        string          `json:"kind"`
	Channel     string          `json:"channel,omitempty"`
	ChannelType string          `json:"channel_type,omitempty"`
	TS          string          `json:"ts,omitempty"`
	ThreadTS    string          `json:"thread_ts,omitempty"`
	UserID      string          `json:"user_id,omitempty"`
	Text        string          `json:"text,omitempty"`
	URL         string          `json:"url,omitempty"`
	Reaction    string          `json:"reaction,omitempty"`
	ItemChannel string          `json:"item_channel,omitempty"`
	ItemTS      string          `json:"item_ts,omitempty"`
	ItemAuthor  string          `json:"item_author,omitempty"`
	TeamID      string          `json:"team_id,omitempty"`
	APIAppID    string          `json:"api_app_id,omitempty"`
	RawJSON     json.RawMessage `json:"raw_json,omitempty"`
}
```

Add metadata support in `internal/monitor/inbox.go`:

```go
type InboxEventMeta struct {
	Source     string `json:"source,omitempty"`
	Actionable bool   `json:"actionable,omitempty"`
}

type InboxEntry struct {
	EnqueuedAt string         `json:"enqueued_at"`
	Event      InboundEvent   `json:"event"`
	Meta       InboxEventMeta `json:"meta,omitempty"`
}

func ClassifyInboxEvent(ev InboundEvent) InboxEventMeta {
	source := ev.ChannelType
	if source == "" {
		source = "unknown"
	}
	actionable := false
	switch source {
	case "github":
		switch ev.Kind {
		case "pr_review_comment", "pr_review_changes_requested", "pr_head_updated":
			actionable = true
		}
	case "slack":
		switch ev.Kind {
		case "message", "app_mention":
			actionable = true
		}
	}
	return InboxEventMeta{Source: source, Actionable: actionable}
}

func MonitorCursorPath(root, slug string) string {
	return filepath.Join(TaskDir(root, slug), "inbox.monitor.cursor")
}

func ReadInboxMonitorCursor(root, slug string) (int64, error) {
	b, err := os.ReadFile(MonitorCursorPath(root, slug))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func WriteInboxMonitorCursor(root, slug string, offset int64) error {
	if err := os.MkdirAll(TaskDir(root, slug), 0o755); err != nil {
		return err
	}
	return os.WriteFile(MonitorCursorPath(root, slug), []byte(strconv.FormatInt(offset, 10)+"\n"), 0o644)
}
```

Update `AppendInboxEvent` so every new row sets `Meta: ClassifyInboxEvent(ev)`.

- [ ] **Step 4: Run metadata tests**

Run the same command from Step 2.

Expected: pass.

- [ ] **Step 5: Commit**

```bash
git add internal/monitor/inbound_event.go internal/monitor/inbox.go internal/monitor/inbox_test.go
git commit -m "Add inbox event metadata"
```

### Task 2: Add Reusable Inbox Scanner

**Files:**
- Create: `internal/monitor/inbox_monitor.go`
- Test: `internal/monitor/inbox_monitor_test.go`

- [ ] **Step 1: Write failing scanner tests**

Create `internal/monitor/inbox_monitor_test.go`:

```go
type recordingWakeTarget struct {
	calls [][]InboxEntry
	err   error
}

func (r *recordingWakeTarget) WakeTask(ctx context.Context, slug string, entries []InboxEntry) error {
	if r.err != nil {
		return r.err
	}
	r.calls = append(r.calls, append([]InboxEntry(nil), entries...))
	return nil
}

func TestInboxMonitorScanOnceWakesForActionableBatch(t *testing.T) {
	root := t.TempDir()
	if err := AppendInboxEvent(root, "review", InboundEvent{Kind: "pr_review_comment", ChannelType: "github", Text: "fix this"}); err != nil {
		t.Fatalf("AppendInboxEvent() error = %v", err)
	}
	if err := AppendInboxEvent(root, "review", InboundEvent{Kind: "pr_merged", ChannelType: "github", Text: "merged"}); err != nil {
		t.Fatalf("AppendInboxEvent() error = %v", err)
	}
	target := &recordingWakeTarget{}
	m := NewInboxMonitor(root, "review", target, InboxMonitorOptions{})

	if err := m.ScanOnce(context.Background()); err != nil {
		t.Fatalf("ScanOnce() error = %v", err)
	}
	if len(target.calls) != 1 {
		t.Fatalf("wake calls = %d, want 1", len(target.calls))
	}
	if got := target.calls[0][0].Event.Text; got != "fix this" {
		t.Fatalf("woken text = %q", got)
	}
}

func TestInboxMonitorScanOnceDoesNotAdvanceCursorWhenWakeFails(t *testing.T) {
	root := t.TempDir()
	if err := AppendInboxEvent(root, "review", InboundEvent{Kind: "message", ChannelType: "slack", Text: "new reply"}); err != nil {
		t.Fatalf("AppendInboxEvent() error = %v", err)
	}
	target := &recordingWakeTarget{err: errors.New("terminal unavailable")}
	m := NewInboxMonitor(root, "review", target, InboxMonitorOptions{})

	if err := m.ScanOnce(context.Background()); err == nil {
		t.Fatalf("ScanOnce() error = nil, want error")
	}
	offset, err := ReadInboxMonitorCursor(root, "review")
	if err != nil {
		t.Fatalf("ReadInboxMonitorCursor() error = %v", err)
	}
	if offset != 0 {
		t.Fatalf("cursor = %d, want 0", offset)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
env FLOW_TASK= GOCACHE=/private/tmp/flow-manager-go-build-cache go test ./internal/monitor -run 'TestInboxMonitorScanOnce' -count=1
```

Expected: fail because `NewInboxMonitor` and `InboxMonitorOptions` do not exist.

- [ ] **Step 3: Implement the scanner**

Create `internal/monitor/inbox_monitor.go`:

```go
package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

type WakeTarget interface {
	WakeTask(ctx context.Context, slug string, entries []InboxEntry) error
}

type InboxMonitorOptions struct {
	PollInterval time.Duration
}

type InboxMonitor struct {
	root         string
	slug         string
	target       WakeTarget
	pollInterval time.Duration
}

func NewInboxMonitor(root, slug string, target WakeTarget, opts InboxMonitorOptions) *InboxMonitor {
	interval := opts.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return &InboxMonitor{root: root, slug: slug, target: target, pollInterval: interval}
}

func (m *InboxMonitor) Run(ctx context.Context) error {
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()
	for {
		if err := m.ScanOnce(ctx); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *InboxMonitor) ScanOnce(ctx context.Context) error {
	offset, err := ReadInboxMonitorCursor(m.root, m.slug)
	if err != nil {
		return err
	}
	f, err := os.Open(InboxPath(m.root, m.slug))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}

	var entries []InboxEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var entry InboxEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return err
		}
		if entry.Meta.Source == "" {
			entry.Meta = ClassifyInboxEvent(entry.Event)
		}
		if entry.Meta.Actionable {
			entries = append(entries, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	newOffset, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		if err := m.target.WakeTask(ctx, m.slug, entries); err != nil {
			return err
		}
	}
	return WriteInboxMonitorCursor(m.root, m.slug, newOffset)
}
```

- [ ] **Step 4: Run scanner tests**

Run:

```bash
env FLOW_TASK= GOCACHE=/private/tmp/flow-manager-go-build-cache go test ./internal/monitor -run 'TestInboxMonitorScanOnce' -count=1
```

Expected: pass.

- [ ] **Step 5: Commit**

```bash
git add internal/monitor/inbox_monitor.go internal/monitor/inbox_monitor_test.go
git commit -m "Add task inbox monitor scanner"
```

### Task 3: Wake the Same Terminal Session

**Files:**
- Create: `internal/server/inbox_wake.go`
- Modify: `internal/server/terminal_bridge.go`
- Test: `internal/server/server_test.go`

- [ ] **Step 1: Write failing wake tests**

Add tests in `internal/server/server_test.go` that exercise prompt formatting without launching Codex:

```go
func TestTerminalPasteInputWrapsPromptForBracketedPaste(t *testing.T) {
	got := terminalPasteInput("flow monitor wake")
	want := "\x1b[200~flow monitor wake\x1b[201~\r"
	if got != want {
		t.Fatalf("terminalPasteInput() = %q, want %q", got, want)
	}
}

func TestFormatInboxWakePromptIncludesSourceAndURL(t *testing.T) {
	entries := []monitor.InboxEntry{{
		Event: monitor.InboundEvent{
			Kind:        "pr_review_comment",
			ChannelType: "github",
			Text:        "please update the migration",
			URL:         "https://github.com/acme/app/pull/7#discussion_r1",
		},
		Meta: monitor.InboxEventMeta{Source: "github", Actionable: true},
	}}

	prompt := formatInboxWakePrompt("review-task", entries)
	if !strings.Contains(prompt, "review-task") {
		t.Fatalf("prompt missing slug: %s", prompt)
	}
	if !strings.Contains(prompt, "github pr_review_comment") {
		t.Fatalf("prompt missing source/kind: %s", prompt)
	}
	if !strings.Contains(prompt, "https://github.com/acme/app/pull/7#discussion_r1") {
		t.Fatalf("prompt missing URL: %s", prompt)
	}
	if !strings.Contains(prompt, "Read the new task inbox entries") {
		t.Fatalf("prompt missing inbox instruction: %s", prompt)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
env FLOW_TASK= GOCACHE=/private/tmp/flow-manager-go-build-cache go test ./internal/server -run 'TestTerminalPasteInputWrapsPromptForBracketedPaste|TestFormatInboxWakePromptIncludesSourceAndURL' -count=1
```

Expected: fail because `terminalPasteInput` and `formatInboxWakePrompt` do not exist.

- [ ] **Step 3: Implement prompt formatting and same-session injection**

Add to `internal/server/terminal_bridge.go`:

```go
func terminalPasteInput(prompt string) string {
	return "\x1b[200~" + prompt + "\x1b[201~\r"
}

func (h *terminalHub) wakeTask(slug, prompt string) error {
	if err := h.sendInput(slug, terminalPasteInput(prompt)); err == nil {
		return nil
	}
	session, err := h.attach(slug, 120, 32)
	if err != nil {
		return err
	}
	_ = session
	return h.sendInput(slug, terminalPasteInput(prompt))
}
```

Create `internal/server/inbox_wake.go`:

```go
package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/your/module/internal/monitor"
)

type inboxWakeTarget struct {
	terminals *terminalHub
}

func (w inboxWakeTarget) WakeTask(ctx context.Context, slug string, entries []monitor.InboxEntry) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return w.terminals.wakeTask(slug, formatInboxWakePrompt(slug, entries))
}

func formatInboxWakePrompt(slug string, entries []monitor.InboxEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Flow task %s has %d new actionable inbox event(s).\n", slug, len(entries))
	b.WriteString("Read the new task inbox entries from inbox.jsonl, inspect the referenced source context, and continue the task in this same session.\n")
	for i, entry := range entries {
		if i >= 5 {
			fmt.Fprintf(&b, "- plus %d more event(s)\n", len(entries)-i)
			break
		}
		fmt.Fprintf(&b, "- %s %s", entry.Meta.Source, entry.Event.Kind)
		if entry.Event.URL != "" {
			fmt.Fprintf(&b, " %s", entry.Event.URL)
		}
		if entry.Event.Text != "" {
			fmt.Fprintf(&b, ": %s", oneLine(entry.Event.Text, 240))
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func oneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= limit {
		return s
	}
	if limit <= 3 {
		return s[:limit]
	}
	return s[:limit-3] + "..."
}
```

Replace `github.com/your/module` with the module path from `go.mod`.

- [ ] **Step 4: Run wake tests**

Run:

```bash
env FLOW_TASK= GOCACHE=/private/tmp/flow-manager-go-build-cache go test ./internal/server -run 'TestTerminalPasteInputWrapsPromptForBracketedPaste|TestFormatInboxWakePromptIncludesSourceAndURL' -count=1
```

Expected: pass.

- [ ] **Step 5: Commit**

```bash
git add internal/server/inbox_wake.go internal/server/terminal_bridge.go internal/server/server_test.go
git commit -m "Wake task sessions from inbox events"
```

### Task 4: Manage One Monitor Per Live Task Session

**Files:**
- Create: `internal/server/inbox_monitor_manager.go`
- Modify: `internal/server/types.go`
- Modify: `internal/server/server.go`
- Modify: `internal/server/terminal_bridge.go`
- Test: `internal/server/server_test.go`

- [ ] **Step 1: Write failing lifecycle tests**

Add a lifecycle test that starts a monitor twice for the same slug and confirms the manager only tracks one live cancel function:

```go
func TestInboxMonitorManagerStartIsIdempotent(t *testing.T) {
	manager := newInboxMonitorManager(t.TempDir(), inboxWakeTarget{})
	manager.start("review")
	manager.start("review")
	defer manager.stop("review")

	manager.mu.Lock()
	count := len(manager.cancel)
	manager.mu.Unlock()
	if count != 1 {
		t.Fatalf("monitor count = %d, want 1", count)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
env FLOW_TASK= GOCACHE=/private/tmp/flow-manager-go-build-cache go test ./internal/server -run TestInboxMonitorManagerStartIsIdempotent -count=1
```

Expected: fail because `newInboxMonitorManager` does not exist.

- [ ] **Step 3: Implement lifecycle manager**

Create `internal/server/inbox_monitor_manager.go`:

```go
package server

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"

	"github.com/your/module/internal/monitor"
)

type inboxMonitorManager struct {
	root   string
	target monitor.WakeTarget

	mu     sync.Mutex
	cancel map[string]context.CancelFunc
}

func newInboxMonitorManager(root string, target monitor.WakeTarget) *inboxMonitorManager {
	return &inboxMonitorManager{root: root, target: target, cancel: make(map[string]context.CancelFunc)}
}

func (m *inboxMonitorManager) start(slug string) {
	m.mu.Lock()
	if _, ok := m.cancel[slug]; ok {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel[slug] = cancel
	m.mu.Unlock()

	go func() {
		err := monitor.NewInboxMonitor(m.root, slug, m.target, monitor.InboxMonitorOptions{}).Run(ctx)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, os.ErrNotExist) {
			log.Printf("flow inbox monitor %s: %v", slug, err)
		}
		m.mu.Lock()
		if current := m.cancel[slug]; current != nil {
			delete(m.cancel, slug)
		}
		m.mu.Unlock()
	}()
}

func (m *inboxMonitorManager) stop(slug string) {
	m.mu.Lock()
	cancel := m.cancel[slug]
	delete(m.cancel, slug)
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
```

Replace `github.com/your/module` with the module path from `go.mod`.

Add `inboxMonitors *inboxMonitorManager` to `Server` in `internal/server/types.go`.

Initialize it in `New` after the terminal hub is created:

```go
s.inboxMonitors = newInboxMonitorManager(root, inboxWakeTarget{terminals: s.terminals})
```

In `internal/server/terminal_bridge.go`, after a task terminal session starts successfully, call:

```go
if h.server != nil && h.server.inboxMonitors != nil {
	h.server.inboxMonitors.start(slug)
}
```

When the terminal session wait loop removes the session from the hub, call:

```go
if h.server != nil && h.server.inboxMonitors != nil {
	h.server.inboxMonitors.stop(slug)
}
```

- [ ] **Step 4: Run lifecycle tests**

Run:

```bash
env FLOW_TASK= GOCACHE=/private/tmp/flow-manager-go-build-cache go test ./internal/server -run TestInboxMonitorManagerStartIsIdempotent -count=1
```

Expected: pass.

- [ ] **Step 5: Commit**

```bash
git add internal/server/inbox_monitor_manager.go internal/server/types.go internal/server/server.go internal/server/terminal_bridge.go internal/server/server_test.go
git commit -m "Manage inbox monitors for live task sessions"
```

### Task 5: Poll and Dispatch GitHub PR Reviews

**Files:**
- Modify: `internal/monitor/github_event.go`
- Modify: `internal/monitor/github_client.go`
- Test: `internal/monitor/github_client_test.go`
- Test: `internal/monitor/github_dispatcher_test.go`

- [ ] **Step 1: Write failing GitHub review tests**

Add tests that expect `ListReviews` to parse submitted review records and dispatch `CHANGES_REQUESTED` to a tracked task's inbox.

```go
func TestGitHubClientListReviews(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/app/pulls/12/reviews" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"id":44,"node_id":"PRR_kw","state":"CHANGES_REQUESTED","body":"needs tests","html_url":"https://github.com/acme/app/pull/12#pullrequestreview-44","submitted_at":"2026-05-23T10:00:00Z","user":{"login":"reviewer"}}]`))
	}))
	defer server.Close()

	client := NewGitHubAPIClient("token", server.URL)
	reviews, err := client.ListReviews(context.Background(), "acme", "app", 12, "")
	if err != nil {
		t.Fatalf("ListReviews() error = %v", err)
	}
	if len(reviews) != 1 {
		t.Fatalf("review count = %d, want 1", len(reviews))
	}
	if reviews[0].State != "CHANGES_REQUESTED" {
		t.Fatalf("state = %q", reviews[0].State)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
env FLOW_TASK= GOCACHE=/private/tmp/flow-manager-go-build-cache go test ./internal/monitor -run 'TestGitHubClientListReviews|TestGitHubDispatcher' -count=1
```

Expected: fail because review polling does not exist.

- [ ] **Step 3: Implement review kinds, client method, and dispatch**

In `internal/monitor/github_event.go`, add:

```go
const (
	GitHubEventPRReviewChangesRequested GitHubEventKind = "pr_review_changes_requested"
	GitHubEventPRReviewApproved         GitHubEventKind = "pr_review_approved"
)
```

Add a review record and client method in `internal/monitor/github_client.go`:

```go
type githubReviewRecord struct {
	ID          int64  `json:"id"`
	NodeID      string `json:"node_id"`
	State       string `json:"state"`
	Body        string `json:"body"`
	HTMLURL     string `json:"html_url"`
	SubmittedAt string `json:"submitted_at"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (c *GitHubAPIClient) ListReviews(ctx context.Context, owner, repo string, number int, since string) ([]githubReviewRecord, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, repo, number)
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var records []githubReviewRecord
	if err := c.doJSON(req, &records); err != nil {
		return nil, err
	}
	if since == "" {
		return records, nil
	}
	var filtered []githubReviewRecord
	for _, record := range records {
		if record.SubmittedAt > since {
			filtered = append(filtered, record)
		}
	}
	return filtered, nil
}
```

In tracked PR polling, call `ListReviews`, map `CHANGES_REQUESTED` to `GitHubEventPRReviewChangesRequested`, map `APPROVED` to `GitHubEventPRReviewApproved`, set `EventKey` to `review:<node_id>` when available, and set `URL` to `HTMLURL`.

Dispatch review events through the same tracked PR route as comments. For `pr_review_changes_requested`, set the task status to `in-progress` before appending the inbox event. For `pr_review_approved`, append a non-actionable inbox event and leave task status unchanged.

- [ ] **Step 4: Run GitHub monitor tests**

Run:

```bash
env FLOW_TASK= GOCACHE=/private/tmp/flow-manager-go-build-cache go test ./internal/monitor -run 'TestGitHubClientListReviews|TestGitHubDispatcher|TestGitHubListener' -count=1
```

Expected: pass.

- [ ] **Step 5: Commit**

```bash
git add internal/monitor/github_event.go internal/monitor/github_client.go internal/monitor/github_client_test.go internal/monitor/github_dispatcher_test.go
git commit -m "Route GitHub PR reviews to task inboxes"
```

### Task 6: Link Current Branch PRs to Flow Tasks

**Files:**
- Create: `internal/ghref/ghref.go`
- Test: `internal/ghref/ghref_test.go`
- Create: `internal/app/github_pr_link.go`
- Modify: `internal/app/done.go`
- Test: `internal/app/done_test.go`

- [ ] **Step 1: Write failing PR tag parser tests**

Create `internal/ghref/ghref_test.go`:

```go
func TestPRTagFromURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "plain PR", url: "https://github.com/acme/app/pull/12", want: "gh-pr:acme/app#12"},
		{name: "review anchor", url: "https://github.com/acme/app/pull/12#pullrequestreview-44", want: "gh-pr:acme/app#12"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := PRTagFromURL(tt.url)
			if !ok {
				t.Fatalf("PRTagFromURL() ok = false")
			}
			if got != tt.want {
				t.Fatalf("tag = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPRTagFromURLRejectsNonPR(t *testing.T) {
	if tag, ok := PRTagFromURL("https://github.com/acme/app/issues/12"); ok {
		t.Fatalf("PRTagFromURL() = %q, true; want false", tag)
	}
}
```

- [ ] **Step 2: Run parser tests to verify they fail**

Run:

```bash
env FLOW_TASK= GOCACHE=/private/tmp/flow-manager-go-build-cache go test ./internal/ghref -count=1
```

Expected: fail because `internal/ghref` does not exist.

- [ ] **Step 3: Implement PR tag parser**

Create `internal/ghref/ghref.go`:

```go
package ghref

import (
	"net/url"
	"strconv"
	"strings"
)

func PRTagFromURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if !strings.EqualFold(u.Host, "github.com") {
		return "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return "", false
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return "", false
	}
	return "gh-pr:" + parts[0] + "/" + parts[1] + "#" + strconv.Itoa(n), true
}
```

- [ ] **Step 4: Add task close-out PR linking**

Create `internal/app/github_pr_link.go`:

```go
var ghPRViewOutput = func(ctx context.Context, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", "--json", "url")
	cmd.Dir = dir
	return cmd.Output()
}

func linkTaskToCurrentBranchPR(ctx context.Context, db *flowdb.DB, task flowdb.Task) error {
	if task.WorkDir == "" {
		return nil
	}
	out, err := ghPRViewOutput(ctx, task.WorkDir)
	if err != nil {
		return nil
	}
	var payload struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return err
	}
	tag, ok := ghref.PRTagFromURL(payload.URL)
	if !ok {
		return nil
	}
	return db.AddTaskTag(task.Slug, tag)
}
```

Call `linkTaskToCurrentBranchPR(context.Background(), db, task)` in `doneCommand` after the task is loaded and before status is updated. Print a warning only for JSON or database errors:

```go
if err := linkTaskToCurrentBranchPR(context.Background(), db, task); err != nil {
	fmt.Fprintf(os.Stderr, "warning: could not link current PR: %v\n", err)
}
```

- [ ] **Step 5: Add close-out tests**

Add a test that stubs `ghPRViewOutput`, runs `flow done`, and verifies the task has `gh-pr:owner/repo#number`.

```go
func TestDoneLinksCurrentBranchPR(t *testing.T) {
	root, cleanup := testFlowRoot(t)
	defer cleanup()
	db := openTestDB(t, root)
	task := insertTask(t, db, "review-task")
	task.WorkDir = t.TempDir()
	updateTaskWorkDir(t, db, task.Slug, task.WorkDir)

	old := ghPRViewOutput
	ghPRViewOutput = func(ctx context.Context, dir string) ([]byte, error) {
		return []byte(`{"url":"https://github.com/acme/app/pull/12"}`), nil
	}
	defer func() { ghPRViewOutput = old }()

	code := doneCommand([]string{task.Slug})
	if code != 0 {
		t.Fatalf("doneCommand() = %d, want 0", code)
	}
	tags, err := db.TaskTags(task.Slug)
	if err != nil {
		t.Fatalf("TaskTags() error = %v", err)
	}
	if !slices.Contains(tags, "gh-pr:acme/app#12") {
		t.Fatalf("tags = %v", tags)
	}
}
```

Use the existing app test helpers for inserting and updating tasks.

- [ ] **Step 6: Run app and parser tests**

Run:

```bash
env FLOW_TASK= GOCACHE=/private/tmp/flow-manager-go-build-cache go test ./internal/ghref ./internal/app -run 'TestPRTagFromURL|TestDoneLinksCurrentBranchPR' -count=1
```

Expected: pass.

- [ ] **Step 7: Commit**

```bash
git add internal/ghref internal/app/github_pr_link.go internal/app/done.go internal/app/done_test.go
git commit -m "Link completed tasks to current GitHub PRs"
```

### Task 7: Update Agent-Facing Bootstrap and User Docs

**Files:**
- Modify: `internal/app/skill/SKILL.md`
- Modify: `internal/app/skill_test.go`
- Modify: `README.md`

- [ ] **Step 1: Write failing skill-doc tests**

Extend the existing skill test with assertions for the generic monitor language:

```go
func TestSkillDocumentsSameSessionInboxMonitor(t *testing.T) {
	skill := embeddedSkill(t)
	for _, want := range []string{
		"inbox.jsonl",
		"same Flow-owned terminal session",
		"Slack, GitHub, or future source",
		"Codex",
		"gh-pr:",
	} {
		if !strings.Contains(skill, want) {
			t.Fatalf("embedded skill missing %q", want)
		}
	}
}
```

- [ ] **Step 2: Run skill tests to verify they fail**

Run:

```bash
env FLOW_TASK= GOCACHE=/private/tmp/flow-manager-go-build-cache go test ./internal/app -run 'TestSkillDocumentsGitHubMonitorBootstrap|TestSkillDocumentsSameSessionInboxMonitor' -count=1
```

Expected: fail until the embedded skill text is updated.

- [ ] **Step 3: Update docs**

In `internal/app/skill/SKILL.md`, add an agent contract section:

```markdown
## Same-session inbox monitor

Flow routes monitored Slack, GitHub, and future source events into the active task's `inbox.jsonl`. When a task terminal is live, Flow also runs a task-local monitor that wakes the same Flow-owned terminal session by sending a short prompt into the existing Claude or Codex session.

When you are woken by this monitor:

1. Read the newest `inbox.jsonl` entries for the current task.
2. Inspect the source link or thread referenced by the entry.
3. Continue the fix in this same agent session.
4. Do not start a separate background solver for the task.

For GitHub PR work, make sure the task carries a `gh-pr:owner/repo#number` tag. `flow done` tries to discover the current branch PR automatically, and you can add the tag manually with `flow update task <slug> --tag gh-pr:owner/repo#number`.
```

In `README.md`, add a short user-facing section that explains monitored sources append to `~/.flow/tasks/<slug>/inbox.jsonl`, live task sessions are woken in-place, and PR review routing depends on `gh-pr:` tags.

- [ ] **Step 4: Run skill tests**

Run:

```bash
env FLOW_TASK= GOCACHE=/private/tmp/flow-manager-go-build-cache go test ./internal/app -run 'TestSkillDocumentsGitHubMonitorBootstrap|TestSkillDocumentsSameSessionInboxMonitor' -count=1
```

Expected: pass.

- [ ] **Step 5: Commit**

```bash
git add internal/app/skill/SKILL.md internal/app/skill_test.go README.md
git commit -m "Document same-session inbox monitoring"
```

### Task 8: Full Verification

**Files:**
- Read-only verification across the repository.

- [ ] **Step 1: Run focused package tests**

Run:

```bash
env FLOW_TASK= GOCACHE=/private/tmp/flow-manager-go-build-cache go test ./internal/monitor ./internal/server ./internal/app ./internal/ghref -count=1
```

Expected: pass.

- [ ] **Step 2: Run full test suite**

Run:

```bash
env FLOW_TASK= GOCACHE=/private/tmp/flow-manager-go-build-cache go test ./... -count=1
```

Expected: pass.

- [ ] **Step 3: Check formatting and diff cleanliness**

Run:

```bash
gofmt -w internal/monitor/inbound_event.go internal/monitor/inbox.go internal/monitor/inbox_monitor.go internal/monitor/inbox_monitor_test.go internal/server/inbox_wake.go internal/server/inbox_monitor_manager.go internal/server/terminal_bridge.go internal/server/server_test.go internal/monitor/github_event.go internal/monitor/github_client.go internal/monitor/github_client_test.go internal/monitor/github_dispatcher_test.go internal/ghref/ghref.go internal/ghref/ghref_test.go internal/app/github_pr_link.go internal/app/done.go internal/app/done_test.go internal/app/skill_test.go
git diff --check
git status --short
```

Expected: `git diff --check` prints no errors. `git status --short` shows only intentional files plus any pre-existing untracked `.DS_Store`.

- [ ] **Step 4: Commit final verification fixes if any files changed**

If `gofmt` or docs checks changed files after the task commits:

```bash
git add internal README.md docs/superpowers/plans/2026-05-23-same-session-event-monitor.md
git commit -m "Verify same-session event monitor"
```
