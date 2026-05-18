package app

import (
	"flow/internal/flowdb"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cmdTell implements `flow tell <task> "<message>" [--from <slug-or-name>]
// [--no-notify]`.
//
// It appends a stamped entry to ~/.flow/tasks/<task>/inbox.md and bumps
// the task's updated_at. The receiving agent picks the message up on its
// next SessionStart, when cmdHookSessionStart compares the inbox mtime
// against tasks.inbox_seen_at and prepends a "new message from parent"
// hint to additionalContext if newer.
//
// --from defaults to whatever the calling session's bound task is (via
// $CLAUDE_CODE_SESSION_ID reverse-lookup), or "user" when there's no
// caller binding. --no-notify suppresses the side-effectful UI event
// publish (used by tests that don't want to spin up the server).
func cmdTell(args []string) int {
	fs := flagSet("tell")
	from := fs.String("from", "", "sender label (default: caller's bound task slug, else 'user')")
	noNotify := fs.Bool("no-notify", false, "skip the UI inbox_changed event publish")
	if leadingHelpArg(args) {
		fs.Usage()
		return 0
	}

	// Parse positional [task, message] up front so the rest of args goes
	// to flags.
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "error: tell requires a task slug and a message")
		return 2
	}
	target, message := args[0], args[1]
	if strings.HasPrefix(target, "-") {
		fmt.Fprintln(os.Stderr, "error: tell requires a task slug before the message")
		return 2
	}
	rest := args[2:]
	if handled, rc := parseFlagSet(fs, rest); handled {
		return rc
	}

	message = strings.TrimSpace(message)
	if message == "" {
		fmt.Fprintln(os.Stderr, "error: tell requires a non-empty message")
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

	task, err := flowdb.GetTask(db, target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: task %q not found\n", target)
		return 1
	}

	sender := strings.TrimSpace(*from)
	if sender == "" {
		// Caller's bound task is the most useful default. If unbound,
		// fall back to "user" — the human is at a terminal typing.
		if caller := lookupBoundTaskSlug(); caller != "" {
			sender = caller
		} else {
			sender = "user"
		}
	}

	root, err := flowRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	inboxPath := filepath.Join(root, "tasks", task.Slug, "inbox.md")
	if err := os.MkdirAll(filepath.Dir(inboxPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	stamp := time.Now().UTC().Format("2006-01-02 15:04:05Z")
	entry := fmt.Sprintf("## %s — from: %s\n\n%s\n\n", stamp, sender, message)
	f, err := os.OpenFile(inboxPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open inbox: %v\n", err)
		return 1
	}
	if st, err := f.Stat(); err == nil && st.Size() == 0 {
		header := "# Inbox\n\nMessages from parent tasks and the user. The bound agent\n" +
			"reads new entries at the start of every session and acts on them.\n\n"
		if _, err := f.WriteString(header); err != nil {
			f.Close()
			fmt.Fprintf(os.Stderr, "error: write inbox header: %v\n", err)
			return 1
		}
	}
	if _, err := f.WriteString(entry); err != nil {
		f.Close()
		fmt.Fprintf(os.Stderr, "error: append inbox: %v\n", err)
		return 1
	}
	if err := f.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "error: close inbox: %v\n", err)
		return 1
	}

	if _, err := db.Exec(
		`UPDATE tasks SET updated_at = ? WHERE slug = ?`,
		flowdb.NowISO(), task.Slug,
	); err != nil {
		fmt.Fprintf(os.Stderr, "warning: bump updated_at: %v\n", err)
	}

	fmt.Printf("delivered to %s (sender: %s)\n", task.Slug, sender)

	if !*noNotify {
		// Notify the live UI so the inbox badge updates without polling.
		// Fire and forget — server may not be running, that's fine.
		notifyInboxChanged(task.Slug, sender, message)
	}
	return 0
}

// notifyInboxChanged pokes the local flow UI server so it publishes an
// "inbox_changed" event to any WS subscribers. Best-effort: silent
// failure when the server isn't running.
func notifyInboxChanged(slug, sender, message string) {
	endpoint := strings.TrimSpace(os.Getenv("FLOW_UI_URL"))
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8787"
	}
	url := strings.TrimRight(endpoint, "/") + "/api/inbox/notify"
	payload := fmt.Sprintf(`{"task_slug":%q,"sender":%q,"preview":%q}`,
		slug, sender, truncateInboxPreview(message, 200))
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func truncateInboxPreview(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
