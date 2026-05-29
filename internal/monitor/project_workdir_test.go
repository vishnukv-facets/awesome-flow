package monitor

import (
	"context"
	"database/sql"
	"os/exec"
	"strings"
	"testing"

	"flow/internal/flowdb"
)

// gitRepoWithOrigin creates a minimal git repo under t.TempDir() with the
// given origin remote URL and returns its path. Used to exercise the real
// resolveProjectForRepo (which reads .git/config via workdirreg).
func gitRepoWithOrigin(t *testing.T, origin string) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-b", "main")
	run("remote", "add", "origin", origin)
	return repo
}

func seedProjectRow(t *testing.T, db *sql.DB, slug, workDir string) {
	t.Helper()
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO projects (slug, name, status, priority, work_dir, created_at, updated_at)
		 VALUES (?, ?, 'active', 'medium', ?, ?, ?)`,
		slug, slug, workDir, now, now,
	); err != nil {
		t.Fatalf("seed project %s: %v", slug, err)
	}
}

func TestResolveProjectForRepo(t *testing.T) {
	db := dispatcherTestDB(t)
	repo := gitRepoWithOrigin(t, "git@github.com:acme/widget.git")
	seedProjectRow(t, db, "widget", repo)

	t.Run("confident match", func(t *testing.T) {
		slug, ok := resolveProjectForRepo(db, "acme/widget")
		if !ok || slug != "widget" {
			t.Fatalf("resolveProjectForRepo = (%q, %v), want (widget, true)", slug, ok)
		}
	})

	t.Run("case-insensitive match", func(t *testing.T) {
		slug, ok := resolveProjectForRepo(db, "Acme/Widget")
		if !ok || slug != "widget" {
			t.Fatalf("resolveProjectForRepo = (%q, %v), want (widget, true)", slug, ok)
		}
	})

	t.Run("no match for other repo", func(t *testing.T) {
		if slug, ok := resolveProjectForRepo(db, "acme/other"); ok {
			t.Fatalf("resolveProjectForRepo = (%q, true), want no match", slug)
		}
	})

	t.Run("ambiguous match is rejected", func(t *testing.T) {
		repo2 := gitRepoWithOrigin(t, "https://github.com/acme/widget.git")
		seedProjectRow(t, db, "widget-dup", repo2)
		if slug, ok := resolveProjectForRepo(db, "acme/widget"); ok {
			t.Fatalf("resolveProjectForRepo = (%q, true), want no match when two projects share the repo", slug)
		}
	})
}

func githubPRReviewEvent() GitHubEvent {
	return GitHubEvent{
		Kind:     GitHubEventPRReviewRequested,
		Owner:    "acme",
		Repo:     "widget",
		Number:   7,
		Title:    "Add a thing",
		URL:      "https://github.com/acme/widget/pull/7",
		Author:   "octo",
		BaseRef:  "main",
		HeadRef:  "feature/x",
		HeadSHA:  "deadbeef",
		EventKey: "pr:acme/widget#7:review_requested",
		RawJSON:  `{"number":7}`,
	}
}

func TestGitHubDispatcher_AutoAttachesProjectOnConfidentMatch(t *testing.T) {
	t.Setenv("FLOW_GH_AUTOOPEN", "0")
	db := dispatcherTestDB(t)
	spawns, _, _, restore := stubDispatcherIO(t)
	defer restore()

	origResolve := resolveProjectForRepo
	resolveProjectForRepo = func(_ *sql.DB, repoKey string) (string, bool) {
		if repoKey == "acme/widget" {
			return "widget-proj", true
		}
		return "", false
	}
	defer func() { resolveProjectForRepo = origResolve }()

	d := NewGitHubDispatcher(db, nil)
	if err := d.Dispatch(context.Background(), githubPRReviewEvent()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(*spawns) != 1 {
		t.Fatalf("spawn count = %d, want 1", len(*spawns))
	}
	spawn := (*spawns)[0]
	if spawn.Project != "widget-proj" {
		t.Errorf("spawn.Project = %q, want widget-proj (auto-attach on confident match)", spawn.Project)
	}
	// When auto-attached, the brief should not push a manual project pick.
	if strings.Contains(spawn.Brief, "First step — pick a project") {
		t.Errorf("auto-attached brief should not render the project picker:\n%s", spawn.Brief)
	}
	if !strings.Contains(spawn.Brief, "widget-proj") {
		t.Errorf("auto-attached brief should name the attached project:\n%s", spawn.Brief)
	}
}

func TestGitHubDispatcher_NoAutoAttachWhenNoMatch(t *testing.T) {
	t.Setenv("FLOW_GH_AUTOOPEN", "0")
	db := dispatcherTestDB(t)
	spawns, _, _, restore := stubDispatcherIO(t)
	defer restore()

	origResolve := resolveProjectForRepo
	resolveProjectForRepo = func(_ *sql.DB, _ string) (string, bool) { return "", false }
	defer func() { resolveProjectForRepo = origResolve }()

	d := NewGitHubDispatcher(db, nil)
	if err := d.Dispatch(context.Background(), githubPRReviewEvent()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(*spawns) != 1 {
		t.Fatalf("spawn count = %d, want 1", len(*spawns))
	}
	spawn := (*spawns)[0]
	if spawn.Project != "" {
		t.Errorf("spawn.Project = %q, want empty (no confident match → no auto-attach)", spawn.Project)
	}
	// With no auto-attach, the brief must keep the manual project picker.
	if !strings.Contains(spawn.Brief, "First step — pick a project") {
		t.Errorf("non-attached brief should render the project picker:\n%s", spawn.Brief)
	}
}
