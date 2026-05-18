package app

import (
	"flow/internal/flowdb"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseDueDate(t *testing.T) {
	// Fixed "now": Wednesday 2026-04-15 14:00 UTC.
	now := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC)

	cases := []struct {
		in      string
		want    string // YYYY-MM-DD
		wantErr bool
	}{
		{"today", "2026-04-15", false},
		{"tomorrow", "2026-04-16", false},
		{"monday", "2026-04-20", false},    // next Monday (Wed→Mon = +5)
		{"wednesday", "2026-04-22", false}, // next Wednesday (not today)
		{"friday", "2026-04-17", false},    // next Friday = +2
		{"3d", "2026-04-18", false},
		{"0d", "2026-04-15", false},
		{"2026-12-25", "2026-12-25", false},
		{"TODAY", "2026-04-15", false}, // case insensitive
		{"garble", "", true},
	}
	for _, c := range cases {
		got, err := parseDueDate(c.in, now)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseDueDate(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseDueDate(%q): unexpected error %v", c.in, err)
			continue
		}
		gotStr := got.Format("2006-01-02")
		if gotStr != c.want {
			t.Errorf("parseDueDate(%q): got %s, want %s", c.in, gotStr, c.want)
		}
	}
}

// TestCmdUpdateTaskRejectsSessionIDFlag pins that the legacy
// --session-id flag is gone — flag.Parse should reject it as
// undefined. Use `flow do --here` to attach a session to a task
// instead.
func TestCmdUpdateTaskRejectsSessionIDFlag(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-no-sid-flag")
	const sid = "11111111-2222-4333-8444-555555555555"
	if rc := cmdUpdate([]string{"task", "ut-no-sid-flag", "--session-id", sid}); rc != 2 {
		t.Errorf("rc=%d, want 2 for removed --session-id flag", rc)
	}
}

func TestCmdUpdateTaskWorkDir(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-wd")

	newDir := filepath.Join(t.TempDir(), "new-spot")
	if rc := cmdUpdate([]string{"task", "ut-wd", "--work-dir", newDir, "--mkdir"}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	db := openFlowDB(t)
	task, _ := flowdb.GetTask(db, "ut-wd")
	if task.WorkDir != newDir {
		t.Errorf("work_dir = %q, want %q", task.WorkDir, newDir)
	}
}

func TestCmdUpdateTaskWorkDirMissingNoMkdir(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-nomkdir")

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if rc := cmdUpdate([]string{"task", "ut-nomkdir", "--work-dir", missing}); rc != 1 {
		t.Errorf("rc=%d, want 1 when path is missing without --mkdir", rc)
	}
}

// TestCmdUpdateTaskBothFields exercises combining multiple field-
// changing flags in one call.
func TestCmdUpdateTaskBothFields(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-both")

	newDir := filepath.Join(t.TempDir(), "combo")
	if rc := cmdUpdate([]string{"task", "ut-both",
		"--priority", "high", "--work-dir", newDir, "--mkdir"}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	db := openFlowDB(t)
	task, _ := flowdb.GetTask(db, "ut-both")
	if task.Priority != "high" {
		t.Errorf("priority = %q, want high", task.Priority)
	}
	if task.WorkDir != newDir {
		t.Errorf("work_dir = %q, want %q", task.WorkDir, newDir)
	}
}

func TestCmdUpdateTaskRequiresFlag(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-noop")

	if rc := cmdUpdate([]string{"task", "ut-noop"}); rc != 2 {
		t.Errorf("rc=%d, want 2 when no field-changing flag is given", rc)
	}
}

func TestCmdUpdateTaskUnknownTask(t *testing.T) {
	setupFlowRoot(t)
	if rc := cmdUpdate([]string{"task", "nope", "--priority", "high"}); rc != 1 {
		t.Errorf("rc=%d, want 1 for unknown task", rc)
	}
}

func TestCmdUpdateUnknownTarget(t *testing.T) {
	setupFlowRoot(t)
	if rc := cmdUpdate([]string{"project", "foo"}); rc != 2 {
		t.Errorf("rc=%d, want 2 for unknown update target", rc)
	}
}

func TestCmdUpdateTaskStatusRollback(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-rollback")
	stubITerm(t)

	// Bootstrap via cmdDo so the task acquires a session_id and is
	// in-progress. flow done now requires a session_id under the
	// session-id invariant.
	if rc := cmdDo([]string{"ut-rollback"}); rc != 0 {
		t.Fatalf("do rc=%d", rc)
	}
	if rc := cmdDone([]string{"ut-rollback"}); rc != 0 {
		t.Fatalf("done rc=%d", rc)
	}
	db := openFlowDB(t)
	task, _ := flowdb.GetTask(db, "ut-rollback")
	if task.Status != "done" {
		t.Fatalf("precondition: status = %q, want done", task.Status)
	}

	// Now roll it back to in-progress via update. session_id is still
	// set (preserved across done) so the invariant holds.
	if rc := cmdUpdate([]string{"task", "ut-rollback", "--status", "in-progress"}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	task, _ = flowdb.GetTask(db, "ut-rollback")
	if task.Status != "in-progress" {
		t.Errorf("status = %q, want in-progress", task.Status)
	}
	if !task.StatusChangedAt.Valid {
		t.Error("status_changed_at should be set after a real status change")
	}
}

// TestCmdUpdateTaskStatusRequiresSessionForNonBacklog pins the
// session-id invariant at the friendly-error layer: setting status
// to in-progress on a task with NULL session_id fails with a
// pointer to flow do / flow do --here.
func TestCmdUpdateTaskStatusRequiresSessionForNonBacklog(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-no-sess")
	if rc := cmdUpdate([]string{"task", "ut-no-sess", "--status", "in-progress"}); rc != 1 {
		t.Errorf("rc=%d, want 1 (sessionless → in-progress should error)", rc)
	}
	db := openFlowDB(t)
	task, _ := flowdb.GetTask(db, "ut-no-sess")
	if task.Status != "backlog" {
		t.Errorf("status = %q, want backlog (rejected update should not flip)", task.Status)
	}
}

func TestCmdUpdateTaskStatusInvalid(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-bad-status")
	if rc := cmdUpdate([]string{"task", "ut-bad-status", "--status", "blocked"}); rc != 2 {
		t.Errorf("rc=%d, want 2 for unknown status", rc)
	}
}

func TestCmdUpdateTaskAssignee(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-assign")

	if rc := cmdUpdate([]string{"task", "ut-assign", "--assignee", "alice"}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	db := openFlowDB(t)
	task, _ := flowdb.GetTask(db, "ut-assign")
	if !task.Assignee.Valid || task.Assignee.String != "alice" {
		t.Errorf("assignee = %+v, want alice", task.Assignee)
	}

	if rc := cmdUpdate([]string{"task", "ut-assign", "--clear-assignee"}); rc != 0 {
		t.Fatalf("clear rc=%d", rc)
	}
	task, _ = flowdb.GetTask(db, "ut-assign")
	if task.Assignee.Valid {
		t.Errorf("assignee should be NULL after clear, got %q", task.Assignee.String)
	}
}

func TestCmdUpdateTaskAssigneeMutuallyExclusive(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-assign-x")
	if rc := cmdUpdate([]string{"task", "ut-assign-x",
		"--assignee", "bob", "--clear-assignee"}); rc != 2 {
		t.Errorf("rc=%d, want 2 when both given", rc)
	}
}

func TestCmdUpdateTaskPriority(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-prio")

	if rc := cmdUpdate([]string{"task", "ut-prio", "--priority", "high"}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	db := openFlowDB(t)
	task, _ := flowdb.GetTask(db, "ut-prio")
	if task.Priority != "high" {
		t.Errorf("priority = %q, want high", task.Priority)
	}
}

func TestCmdUpdateTaskPriorityInvalid(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-prio-bad")
	if rc := cmdUpdate([]string{"task", "ut-prio-bad", "--priority", "urgent"}); rc != 2 {
		t.Errorf("rc=%d, want 2 for invalid priority", rc)
	}
}

func TestCmdUpdateTaskWaiting(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-wait")

	if rc := cmdUpdate([]string{"task", "ut-wait", "--waiting", "Bob"}); rc != 0 {
		t.Fatalf("set rc=%d", rc)
	}
	db := openFlowDB(t)
	task, _ := flowdb.GetTask(db, "ut-wait")
	if !task.WaitingOn.Valid || task.WaitingOn.String != "Bob" {
		t.Errorf("waiting_on = %+v, want Bob", task.WaitingOn)
	}

	if rc := cmdUpdate([]string{"task", "ut-wait", "--clear-waiting"}); rc != 0 {
		t.Fatalf("clear rc=%d", rc)
	}
	task, _ = flowdb.GetTask(db, "ut-wait")
	if task.WaitingOn.Valid {
		t.Errorf("waiting_on should be NULL after clear, got %q", task.WaitingOn.String)
	}
}

func TestCmdUpdateTaskWaitingMutuallyExclusive(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-wait-x")
	if rc := cmdUpdate([]string{"task", "ut-wait-x",
		"--waiting", "Carol", "--clear-waiting"}); rc != 2 {
		t.Errorf("rc=%d, want 2 when both given", rc)
	}
}

func TestCmdUpdateProjectPriority(t *testing.T) {
	setupFlowRoot(t)
	wd := t.TempDir()
	if rc := cmdAdd([]string{"project", "Some Proj", "--slug", "up-sp", "--work-dir", wd}); rc != 0 {
		t.Fatalf("seed project rc=%d", rc)
	}

	if rc := cmdUpdate([]string{"project", "up-sp", "--priority", "low"}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	db := openFlowDB(t)
	p, _ := flowdb.GetProject(db, "up-sp")
	if p.Priority != "low" {
		t.Errorf("project priority = %q, want low", p.Priority)
	}
}

func TestCmdUpdateProjectPriorityInvalid(t *testing.T) {
	setupFlowRoot(t)
	wd := t.TempDir()
	if rc := cmdAdd([]string{"project", "Bad", "--slug", "up-bad", "--work-dir", wd}); rc != 0 {
		t.Fatalf("seed rc=%d", rc)
	}
	if rc := cmdUpdate([]string{"project", "up-bad", "--priority", "urgent"}); rc != 2 {
		t.Errorf("rc=%d, want 2", rc)
	}
}

func TestCmdUpdateProjectRequiresFlag(t *testing.T) {
	setupFlowRoot(t)
	wd := t.TempDir()
	if rc := cmdAdd([]string{"project", "Empty", "--slug", "up-empty", "--work-dir", wd}); rc != 0 {
		t.Fatalf("seed rc=%d", rc)
	}
	if rc := cmdUpdate([]string{"project", "up-empty"}); rc != 2 {
		t.Errorf("rc=%d, want 2 when no flag given", rc)
	}
}

func TestCmdUpdateUnknownProject(t *testing.T) {
	setupFlowRoot(t)
	if rc := cmdUpdate([]string{"project", "nope", "--priority", "high"}); rc != 1 {
		t.Errorf("rc=%d, want 1 for unknown project", rc)
	}
}

func TestCmdUpdateTaskDueDate(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-due")

	if rc := cmdUpdate([]string{"task", "ut-due", "--due-date", "2026-12-31"}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	db := openFlowDB(t)
	task, _ := flowdb.GetTask(db, "ut-due")
	if !task.DueDate.Valid || task.DueDate.String != "2026-12-31" {
		t.Errorf("due_date = %+v, want 2026-12-31", task.DueDate)
	}

	if rc := cmdUpdate([]string{"task", "ut-due", "--clear-due"}); rc != 0 {
		t.Fatalf("clear rc=%d", rc)
	}
	task, _ = flowdb.GetTask(db, "ut-due")
	if task.DueDate.Valid {
		t.Errorf("due_date should be NULL after clear, got %q", task.DueDate.String)
	}
}

func TestCmdUpdateTaskAddTags(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-tag")

	if rc := cmdUpdate([]string{"task", "ut-tag", "--tag", "Frontend", "--tag", "URGENT"}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	db := openFlowDB(t)
	tags, err := flowdb.GetTaskTags(db, "ut-tag")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || tags[0] != "frontend" || tags[1] != "urgent" {
		t.Errorf("got %v, want [frontend urgent]", tags)
	}
}

func TestCmdUpdateTaskTagIdempotent(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-tag-idem")

	if rc := cmdUpdate([]string{"task", "ut-tag-idem", "--tag", "alpha", "--tag", "ALPHA", "--tag", "alpha"}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	db := openFlowDB(t)
	tags, _ := flowdb.GetTaskTags(db, "ut-tag-idem")
	if len(tags) != 1 || tags[0] != "alpha" {
		t.Errorf("got %v, want [alpha]", tags)
	}
}

func TestCmdUpdateTaskRemoveTag(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-tag-rm")

	if rc := cmdUpdate([]string{"task", "ut-tag-rm", "--tag", "a", "--tag", "b", "--tag", "c"}); rc != 0 {
		t.Fatalf("setup rc=%d", rc)
	}
	if rc := cmdUpdate([]string{"task", "ut-tag-rm", "--remove-tag", "b"}); rc != 0 {
		t.Fatalf("remove rc=%d", rc)
	}
	db := openFlowDB(t)
	tags, _ := flowdb.GetTaskTags(db, "ut-tag-rm")
	if len(tags) != 2 || tags[0] != "a" || tags[1] != "c" {
		t.Errorf("got %v, want [a c]", tags)
	}
}

func TestCmdUpdateTaskClearTags(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-tag-clr")

	if rc := cmdUpdate([]string{"task", "ut-tag-clr", "--tag", "x", "--tag", "y"}); rc != 0 {
		t.Fatalf("setup rc=%d", rc)
	}
	if rc := cmdUpdate([]string{"task", "ut-tag-clr", "--clear-tags"}); rc != 0 {
		t.Fatalf("clear rc=%d", rc)
	}
	db := openFlowDB(t)
	tags, _ := flowdb.GetTaskTags(db, "ut-tag-clr")
	if len(tags) != 0 {
		t.Errorf("after clear got %v, want []", tags)
	}
}

func TestCmdUpdateTaskClearTagsAndRemoveTagExclusive(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-tag-x")
	if rc := cmdUpdate([]string{"task", "ut-tag-x", "--clear-tags", "--remove-tag", "foo"}); rc != 2 {
		t.Errorf("rc=%d, want 2 when both --clear-tags and --remove-tag given", rc)
	}
}

func TestCmdUpdateTaskClearAndAddTagsCombo(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "ut-tag-combo")

	if rc := cmdUpdate([]string{"task", "ut-tag-combo", "--tag", "old1", "--tag", "old2"}); rc != 0 {
		t.Fatalf("setup rc=%d", rc)
	}
	// --clear-tags + --tag means: drop all, then add the new ones.
	if rc := cmdUpdate([]string{"task", "ut-tag-combo", "--clear-tags", "--tag", "fresh"}); rc != 0 {
		t.Fatalf("combo rc=%d", rc)
	}
	db := openFlowDB(t)
	tags, _ := flowdb.GetTaskTags(db, "ut-tag-combo")
	if len(tags) != 1 || tags[0] != "fresh" {
		t.Errorf("got %v, want [fresh]", tags)
	}
}

// ---------- rename tests ----------

// TestCmdUpdateProjectRenameSlugCascades verifies that renaming a
// project's slug cascades the change through tasks.project_slug and
// playbooks.project_slug, and that the ~/.flow/projects/<slug>/ dir
// moves to match.
func TestCmdUpdateProjectRenameSlugCascades(t *testing.T) {
	root := setupFlowRoot(t)
	wd := t.TempDir()
	if rc := cmdAdd([]string{"project", "Old Name", "--slug", "old-proj", "--work-dir", wd}); rc != 0 {
		t.Fatalf("seed project rc=%d", rc)
	}
	if rc := cmdAdd([]string{"task", "child task", "--slug", "child-task", "--project", "old-proj"}); rc != 0 {
		t.Fatalf("seed task rc=%d", rc)
	}
	if rc := cmdAdd([]string{"playbook", "child pb", "--slug", "child-pb", "--project", "old-proj", "--work-dir", wd}); rc != 0 {
		t.Fatalf("seed playbook rc=%d", rc)
	}

	if rc := cmdUpdate([]string{"project", "old-proj", "--slug", "new-proj", "--name", "New Name"}); rc != 0 {
		t.Fatalf("rename rc=%d", rc)
	}

	db := openFlowDB(t)
	p, err := flowdb.GetProject(db, "new-proj")
	if err != nil {
		t.Fatalf("GetProject(new-proj): %v", err)
	}
	if p.Name != "New Name" {
		t.Errorf("name = %q, want %q", p.Name, "New Name")
	}
	if _, err := flowdb.GetProject(db, "old-proj"); err == nil {
		t.Errorf("old project slug still resolvable; should be renamed")
	}

	task, _ := flowdb.GetTask(db, "child-task")
	if !task.ProjectSlug.Valid || task.ProjectSlug.String != "new-proj" {
		t.Errorf("task project_slug not cascaded: %+v", task.ProjectSlug)
	}
	pb, _ := flowdb.GetPlaybook(db, "child-pb")
	if !pb.ProjectSlug.Valid || pb.ProjectSlug.String != "new-proj" {
		t.Errorf("playbook project_slug not cascaded: %+v", pb.ProjectSlug)
	}

	if _, err := os.Stat(filepath.Join(root, "projects", "new-proj", "brief.md")); err != nil {
		t.Errorf("new project dir missing brief.md: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "projects", "old-proj")); !os.IsNotExist(err) {
		t.Errorf("old project dir should be gone, got err=%v", err)
	}
}

// TestCmdUpdateProjectRenameSlugTaken pins that renaming to an already-taken
// project slug fails with rc=1 and leaves DB unchanged.
func TestCmdUpdateProjectRenameSlugTaken(t *testing.T) {
	setupFlowRoot(t)
	wd := t.TempDir()
	if rc := cmdAdd([]string{"project", "P1", "--slug", "p1", "--work-dir", wd}); rc != 0 {
		t.Fatal()
	}
	if rc := cmdAdd([]string{"project", "P2", "--slug", "p2", "--work-dir", wd}); rc != 0 {
		t.Fatal()
	}
	if rc := cmdUpdate([]string{"project", "p1", "--slug", "p2"}); rc != 1 {
		t.Errorf("rc=%d, want 1 for taken slug", rc)
	}
	db := openFlowDB(t)
	if _, err := flowdb.GetProject(db, "p1"); err != nil {
		t.Errorf("p1 should still exist after failed rename: %v", err)
	}
}

// TestCmdUpdateProjectInvalidSlug pins slug validation: empty and
// path-separator-containing slugs are rejected.
func TestCmdUpdateProjectInvalidSlug(t *testing.T) {
	setupFlowRoot(t)
	wd := t.TempDir()
	if rc := cmdAdd([]string{"project", "P", "--slug", "valid", "--work-dir", wd}); rc != 0 {
		t.Fatal()
	}
	if rc := cmdUpdate([]string{"project", "valid", "--slug", "  "}); rc != 2 {
		t.Errorf("empty slug rc=%d, want 2", rc)
	}
	if rc := cmdUpdate([]string{"project", "valid", "--slug", "../escape"}); rc != 2 {
		t.Errorf("path-separator slug rc=%d, want 2", rc)
	}
}

// TestCmdUpdateTaskRenameSlugCascades verifies that renaming a task's
// slug cascades to tags and the ~/.flow/tasks/<slug>/ dir moves.
func TestCmdUpdateTaskRenameSlugCascades(t *testing.T) {
	root := setupFlowRoot(t)
	seedTask(t, "ut-rename-old")
	if rc := cmdUpdate([]string{"task", "ut-rename-old", "--tag", "alpha"}); rc != 0 {
		t.Fatal()
	}

	if rc := cmdUpdate([]string{"task", "ut-rename-old", "--slug", "ut-rename-new", "--name", "renamed"}); rc != 0 {
		t.Fatalf("rename rc=%d", rc)
	}

	db := openFlowDB(t)
	task, err := flowdb.GetTask(db, "ut-rename-new")
	if err != nil {
		t.Fatalf("GetTask(new): %v", err)
	}
	if task.Name != "renamed" {
		t.Errorf("name = %q, want %q", task.Name, "renamed")
	}
	if _, err := flowdb.GetTask(db, "ut-rename-old"); err == nil {
		t.Errorf("old task slug still resolvable")
	}
	tags, _ := flowdb.GetTaskTags(db, "ut-rename-new")
	if len(tags) != 1 || tags[0] != "alpha" {
		t.Errorf("tags not carried to new slug: %v", tags)
	}

	if _, err := os.Stat(filepath.Join(root, "tasks", "ut-rename-new", "brief.md")); err != nil {
		t.Errorf("new task dir missing brief.md: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "tasks", "ut-rename-old")); !os.IsNotExist(err) {
		t.Errorf("old task dir should be gone, got err=%v", err)
	}
}

// TestCmdUpdateTaskRenameSlugTaken pins that renaming to an already-taken
// task slug fails.
func TestCmdUpdateTaskRenameSlugTaken(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "first")
	seedTask(t, "second")
	if rc := cmdUpdate([]string{"task", "first", "--slug", "second"}); rc != 1 {
		t.Errorf("rc=%d, want 1 for taken slug", rc)
	}
}

// TestCmdUpdatePlaybookRenameSlugCascades verifies that renaming a
// playbook's slug cascades to tasks.playbook_slug (the
// kind=playbook_run rows) and moves the FS dir.
func TestCmdUpdatePlaybookRenameSlugCascades(t *testing.T) {
	root := setupFlowRoot(t)
	wd := t.TempDir()
	if rc := cmdAdd([]string{"playbook", "Old PB", "--slug", "old-pb", "--work-dir", wd}); rc != 0 {
		t.Fatalf("seed rc=%d", rc)
	}
	// Seed a fake playbook-run task referencing the playbook by slug.
	db := openFlowDB(t)
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, kind, playbook_slug, priority, work_dir, session_provider, session_id, status_changed_at, created_at, updated_at)
		 VALUES (?, ?, 'in-progress', 'playbook_run', ?, 'medium', ?, 'claude', ?, ?, ?, ?)`,
		"old-pb-run-1", "Run 1", "old-pb", wd, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", now, now, now,
	); err != nil {
		t.Fatalf("seed run task: %v", err)
	}

	if rc := cmdUpdate([]string{"playbook", "old-pb", "--slug", "new-pb", "--name", "New PB"}); rc != 0 {
		t.Fatalf("rename rc=%d", rc)
	}

	pb, err := flowdb.GetPlaybook(db, "new-pb")
	if err != nil {
		t.Fatalf("GetPlaybook(new-pb): %v", err)
	}
	if pb.Name != "New PB" {
		t.Errorf("name = %q, want %q", pb.Name, "New PB")
	}
	runTask, _ := flowdb.GetTask(db, "old-pb-run-1")
	if !runTask.PlaybookSlug.Valid || runTask.PlaybookSlug.String != "new-pb" {
		t.Errorf("run task playbook_slug not cascaded: %+v", runTask.PlaybookSlug)
	}
	if _, err := os.Stat(filepath.Join(root, "playbooks", "new-pb", "brief.md")); err != nil {
		t.Errorf("new playbook dir missing brief.md: %v", err)
	}
}

// TestCmdUpdateUnknownPlaybookTarget pins that `flow update playbook
// <unknown>` errors out cleanly.
func TestCmdUpdateUnknownPlaybookTarget(t *testing.T) {
	setupFlowRoot(t)
	if rc := cmdUpdate([]string{"playbook", "nope", "--name", "X"}); rc != 1 {
		t.Errorf("rc=%d, want 1", rc)
	}
}

// TestCmdUpdatePlaybookRequiresFlag pins that no field flag → rc=2.
func TestCmdUpdatePlaybookRequiresFlag(t *testing.T) {
	setupFlowRoot(t)
	wd := t.TempDir()
	if rc := cmdAdd([]string{"playbook", "P", "--slug", "p-flag", "--work-dir", wd}); rc != 0 {
		t.Fatal()
	}
	if rc := cmdUpdate([]string{"playbook", "p-flag"}); rc != 2 {
		t.Errorf("rc=%d, want 2 for no fields", rc)
	}
}
