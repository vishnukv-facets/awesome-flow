package monitor

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestGitHubListener_StartIsNoOpWhenDisabled(t *testing.T) {
	t.Setenv("FLOW_GH_ENABLED", "0")
	t.Setenv("FLOW_GH_SELF_LOGINS", "me")

	l := NewGitHubListener(NewGitHubDispatcher(nil, nil))
	if err := l.Start(); err != nil {
		t.Fatalf("Start err = %v", err)
	}
	l.mu.Lock()
	running := l.running
	l.mu.Unlock()
	if running {
		t.Fatal("listener should not be running when FLOW_GH_ENABLED=0")
	}
	l.Stop()
}

func TestGitHubListener_MockPollerDispatchesAssignments(t *testing.T) {
	t.Setenv("FLOW_GH_ENABLED", "1")
	t.Setenv("FLOW_GH_SELF_LOGINS", "me")
	t.Setenv("FLOW_GH_AUTOOPEN", "0")
	db := dispatcherTestDB(t)
	spawns, _, _, restore := stubDispatcherIO(t)
	defer restore()

	var once sync.Once
	polled := make(chan struct{}, 1)
	l := NewGitHubListener(NewGitHubDispatcher(db, nil))
	l.pollInterval = 10 * time.Millisecond
	l.pollFn = func(context.Context) ([]GitHubEvent, error) {
		once.Do(func() {
			polled <- struct{}{}
		})
		return []GitHubEvent{
			{
				Kind:     GitHubEventPRReviewRequested,
				Owner:    "Facets-cloud",
				Repo:     "flow-manager",
				Number:   42,
				Title:    "Add GitHub integration",
				URL:      "https://github.com/Facets-cloud/flow-manager/pull/42",
				EventKey: "pr:Facets-cloud/flow-manager#42:review_requested",
			},
			{
				Kind:     GitHubEventIssueAssigned,
				Owner:    "Facets-cloud",
				Repo:     "flow-manager",
				Number:   7,
				Title:    "Document GitHub polling",
				URL:      "https://github.com/Facets-cloud/flow-manager/issues/7",
				EventKey: "issue:Facets-cloud/flow-manager#7:assigned",
			},
		}, nil
	}

	if err := l.Start(); err != nil {
		t.Fatalf("Start err = %v", err)
	}
	select {
	case <-polled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for poll")
	}
	l.Stop()

	if len(*spawns) != 2 {
		t.Fatalf("spawn count = %d, want 2", len(*spawns))
	}
}

func TestGitHubListener_MockPollerDispatchesTrackedPRComments(t *testing.T) {
	t.Setenv("FLOW_GH_ENABLED", "1")
	t.Setenv("FLOW_GH_SELF_LOGINS", "me")
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedGitHubTask(t, "tracked-pr", db, "gh-pr:Facets-cloud/flow-manager#42")

	polled := make(chan struct{}, 1)
	l := NewGitHubListener(NewGitHubDispatcher(db, nil))
	l.pollInterval = 10 * time.Millisecond
	l.pollFn = func(context.Context) ([]GitHubEvent, error) {
		select {
		case polled <- struct{}{}:
		default:
		}
		return []GitHubEvent{
			{
				Kind:      GitHubEventPRReviewComment,
				Owner:     "Facets-cloud",
				Repo:      "flow-manager",
				Number:    42,
				CommentID: "PRRC_1",
				Author:    "reviewer",
				Body:      "Please tighten the docs.",
				EventKey:  "review-comment:PRRC_1",
			},
		}, nil
	}

	if err := l.Start(); err != nil {
		t.Fatalf("Start err = %v", err)
	}
	select {
	case <-polled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for poll")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := ReadInboxEntries("tracked-pr")
		if len(entries) == 1 {
			l.Stop()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	l.Stop()
	entries, _ := ReadInboxEntries("tracked-pr")
	t.Fatalf("tracked PR comment was not appended; entries=%v", entries)
}
