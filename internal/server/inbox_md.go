package server

import (
	"bufio"
	"os"
	"strings"
	"time"

	"flow/internal/flowdb"
)

// readInboxEntries parses ~/.flow/tasks/<slug>/inbox.md into structured
// entries. The format is the one `flow tell <slug>` writes — `## TIMESTAMP
// — from: WHO` headers separating message bodies. This is the per-task
// agent-to-agent inbox, distinct from any external notification system.
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

// unreadInboxCount returns how many entries are newer than the task's
// inbox_seen_at watermark. Entries whose timestamps fail to parse are
// skipped (treated as "old enough") to avoid over-counting on malformed
// lines.
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
