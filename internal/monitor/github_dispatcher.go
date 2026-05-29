package monitor

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"flow/internal/flowdb"
	"flow/internal/ghref"
	"flow/internal/workdirreg"
)

// resolveProjectForRepo returns the slug of the single active (non-archived)
// project whose work_dir git origin remote points at the given owner/repo,
// and ok=true. When zero OR more than one project matches, ok=false and the
// caller falls back to the brief's manual project picker rather than guessing
// — an ambiguous auto-attach is worse than asking. Package-level var so tests
// can inject matches without standing up real git checkouts.
var resolveProjectForRepo = func(db *sql.DB, repoKey string) (string, bool) {
	repoKey = strings.ToLower(strings.TrimSpace(repoKey))
	if db == nil || repoKey == "" {
		return "", false
	}
	projects, err := flowdb.ListProjects(db, flowdb.ProjectFilter{IncludeArchived: false})
	if err != nil {
		return "", false
	}
	matched := ""
	count := 0
	for _, p := range projects {
		if p == nil || strings.TrimSpace(p.WorkDir) == "" {
			continue
		}
		slug, ok := ghref.RepoFromRemote(workdirreg.DetectGitRemote(p.WorkDir))
		if !ok || slug != repoKey {
			continue
		}
		matched = p.Slug
		count++
	}
	if count == 1 {
		return matched, true
	}
	return "", false
}

// GitHubDispatcher routes normalized GitHub events into flow tasks. It
// mirrors Dispatcher for Slack but keeps GitHub-specific tags, briefs, and
// idempotency isolated.
type GitHubDispatcher struct {
	DB     *sql.DB
	Opener TaskOpener
}

func NewGitHubDispatcher(db *sql.DB, opener TaskOpener) *GitHubDispatcher {
	return &GitHubDispatcher{DB: db, Opener: opener}
}

func (d *GitHubDispatcher) Dispatch(ctx context.Context, ev GitHubEvent) error {
	if d == nil || d.DB == nil {
		return nil
	}
	if ev.LinkTag() == "" {
		return nil
	}
	if key := ev.EventKeyValue(); key != "" {
		seen, err := flowdb.HasGitHubEvent(d.DB, key)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}
	}

	switch ev.Kind {
	case GitHubEventPRAssigned, GitHubEventPRReviewRequested, GitHubEventIssueAssigned:
		return d.dispatchGitHubItem(ctx, ev)
	case GitHubEventPRReviewComment, GitHubEventPRReviewChangesRequested, GitHubEventPRReviewApproved,
		GitHubEventPRComment, GitHubEventIssueComment:
		return d.dispatchGitHubReview(ctx, ev)
	case GitHubEventPRHeadUpdated:
		return d.dispatchGitHubHeadUpdated(ev)
	case GitHubEventPRMerged:
		return d.dispatchGitHubMerged(ev)
	default:
		return nil
	}
}

func (d *GitHubDispatcher) dispatchGitHubItem(ctx context.Context, ev GitHubEvent) error {
	slug, found, err := d.findTaskByGitHubTag(ev.LinkTag())
	if err != nil {
		return fmt.Errorf("github monitor: lookup task by tag: %w", err)
	}
	if !found {
		slug, err = d.createGitHubTask(ctx, ev)
		if err != nil {
			return fmt.Errorf("github monitor: create task: %w", err)
		}
	}
	if err := AppendInboxEvent(slug, gitHubEventToInboxEvent(ev)); err != nil {
		return fmt.Errorf("github monitor: append inbox: %w", err)
	}
	if err := d.recordEvent(ev, slug); err != nil {
		return err
	}
	if !found && ev.IsPR() && ev.HeadSHA != "" {
		if err := d.recordHeadSHASeen(ev, slug); err != nil {
			return err
		}
	}
	if !found && githubAutoOpenEnabled() {
		if d.Opener != nil {
			if err := d.Opener.OpenInUI(slug); err != nil {
				fmt.Fprintf(os.Stderr, "github monitor: open in UI: %v\n", err)
			}
		} else {
			_ = openSlackReplyTask(slug)
		}
	}
	return nil
}

func (d *GitHubDispatcher) dispatchGitHubReview(ctx context.Context, ev GitHubEvent) error {
	slug, found, err := d.findTaskByGitHubTag(ev.LinkTag())
	if err != nil {
		return fmt.Errorf("github monitor: lookup task by tag: %w", err)
	}
	if !found {
		slug, err = d.createGitHubTask(ctx, ev)
		if err != nil {
			return fmt.Errorf("github monitor: create task for comment: %w", err)
		}
	}
	if err := AppendInboxEvent(slug, gitHubEventToInboxEvent(ev)); err != nil {
		return fmt.Errorf("github monitor: append inbox: %w", err)
	}
	if ev.Kind == GitHubEventPRReviewChangesRequested {
		if err := d.reopenTaskForGitHubReview(slug); err != nil {
			return err
		}
	}
	return d.recordEvent(ev, slug)
}

func (d *GitHubDispatcher) dispatchGitHubHeadUpdated(ev GitHubEvent) error {
	slug, found, err := d.findTaskByGitHubTag(ev.LinkTag())
	if err != nil {
		return fmt.Errorf("github monitor: lookup task by tag: %w", err)
	}
	if !found {
		return d.recordEvent(ev, "")
	}
	if err := AppendInboxEvent(slug, gitHubEventToInboxEvent(ev)); err != nil {
		return fmt.Errorf("github monitor: append inbox: %w", err)
	}
	if err := d.reopenTaskForGitHubReview(slug); err != nil {
		return err
	}
	return d.recordEvent(ev, slug)
}

func (d *GitHubDispatcher) dispatchGitHubMerged(ev GitHubEvent) error {
	slug, found, err := d.findTaskByGitHubTag(ev.LinkTag())
	if err != nil {
		return fmt.Errorf("github monitor: lookup task by tag: %w", err)
	}
	if !found {
		return d.recordEvent(ev, "")
	}
	if err := AppendInboxEvent(slug, gitHubEventToInboxEvent(ev)); err != nil {
		return fmt.Errorf("github monitor: append inbox: %w", err)
	}
	if err := d.markTaskDoneFromGitHubMerge(slug); err != nil {
		return err
	}
	return d.recordEvent(ev, slug)
}

func (d *GitHubDispatcher) findTaskByGitHubTag(tag string) (string, bool, error) {
	tag = flowdb.NormalizeTag(tag)
	if tag == "" {
		return "", false, nil
	}
	tasks, err := flowdb.ListTasks(d.DB, flowdb.TaskFilter{Tag: tag})
	if err != nil {
		return "", false, err
	}
	if len(tasks) == 0 {
		return "", false, nil
	}
	for _, task := range tasks {
		if task != nil && task.Status != "done" {
			return task.Slug, true, nil
		}
	}
	return tasks[0].Slug, true, nil
}

func (d *GitHubDispatcher) createGitHubTask(ctx context.Context, ev GitHubEvent) (string, error) {
	slug := GitHubSlugForEvent(ev)
	if slug == "" {
		return "", fmt.Errorf("cannot derive github task slug")
	}
	// The GitHub event names the repo, so try to attach the task to the
	// matching flow project up front — then it inherits the project's real
	// work_dir instead of a throwaway workspace (project-workdir-bug). On an
	// ambiguous or absent match we fall back to the brief's manual picker.
	attached, _ := resolveProjectForRepo(d.DB, ev.RepoKey())
	projects, _ := listProjectChoices(d.DB)
	brief := githubTaskBrief(ev, slug, projects, attached)
	if err := spawnFlowTask(ctx, githubTaskName(ev), slug, brief, ProviderForGitHubLabels(ev.Labels), attached); err != nil {
		return "", err
	}
	for _, tag := range []string{"github", ev.LinkTag()} {
		if err := tagFlowTask(ctx, slug, tag); err != nil {
			return slug, err
		}
	}
	return slug, nil
}

func (d *GitHubDispatcher) recordEvent(ev GitHubEvent, slug string) error {
	key := ev.EventKeyValue()
	if key == "" {
		return nil
	}
	taskSlug := strings.TrimSpace(slug)
	if taskSlug != "" {
		if _, err := flowdb.GetTask(d.DB, taskSlug); err != nil {
			taskSlug = ""
		}
	}
	_, err := flowdb.RecordGitHubEvent(d.DB, flowdb.GitHubEventLogEntry{
		EventKey:  key,
		EventKind: string(ev.Kind),
		TaskSlug:  taskSlug,
		RawJSON:   ev.RawJSON,
	})
	return err
}

func (d *GitHubDispatcher) recordHeadSHASeen(ev GitHubEvent, slug string) error {
	sha := strings.TrimSpace(ev.HeadSHA)
	if sha == "" || ev.LinkTag() == "" {
		return nil
	}
	seed := ev
	seed.Kind = GitHubEventPRHeadUpdated
	seed.EventKey = gitHubPRHeadEventKey(ev.Owner, ev.Repo, ev.Number, sha)
	return d.recordEvent(seed, slug)
}

func (d *GitHubDispatcher) reopenTaskForGitHubReview(slug string) error {
	now := flowdb.NowISO()
	res, err := d.DB.Exec(
		`UPDATE tasks
		 SET status = CASE WHEN status = 'done' THEN 'backlog' ELSE status END,
		     status_changed_at = CASE WHEN status = 'done' THEN ? ELSE status_changed_at END,
		     updated_at = ?
		 WHERE slug = ?`,
		now, now, slug,
	)
	if err != nil {
		return fmt.Errorf("github monitor: reopen task for review: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return fmt.Errorf("github monitor: task %q not updated for review", slug)
	}
	return nil
}

func (d *GitHubDispatcher) markTaskDoneFromGitHubMerge(slug string) error {
	now := flowdb.NowISO()
	res, err := d.DB.Exec(
		`UPDATE tasks
		 SET status = 'done',
		     status_changed_at = CASE WHEN status != 'done' THEN ? ELSE status_changed_at END,
		     updated_at = ?
		 WHERE slug = ?`,
		now, now, slug,
	)
	if err != nil {
		return fmt.Errorf("github monitor: mark merged task done: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return fmt.Errorf("github monitor: task %q not updated after merge", slug)
	}
	return nil
}

func githubTaskName(ev GitHubEvent) string {
	title := strings.TrimSpace(ev.Title)
	if title == "" {
		title = strings.TrimSpace(ev.URL)
	}
	if title == "" {
		title = ev.LinkTag()
	}
	prefix := "PR"
	if ev.IsIssue() {
		prefix = "Issue"
	}
	return fmt.Sprintf("%s %s#%d: %s", prefix, ev.RepoKey(), ev.Number, title)
}

func githubTaskBrief(ev GitHubEvent, slug string, projects []projectChoice, attached string) string {
	var b strings.Builder
	title := strings.TrimSpace(ev.Title)
	if title == "" {
		title = githubTaskName(ev)
	}
	b.WriteString("# " + title + "\n\n")
	if strings.TrimSpace(attached) != "" {
		// Repo matched a flow project by git origin, so the task is already
		// attached and runs in that project's real checkout — no manual pick.
		fmt.Fprintf(&b, "## Project\nThis task is attached to project `%s` and runs in that project's "+
			"repository — `flow show task` shows the work_dir and worktree. No project pick is needed; "+
			"make any code changes in the project repo, not a throwaway workspace.\n", attached)
	} else {
		b.WriteString("## First step — pick a project\n")
		b.WriteString(renderGitHubProjectPicker(slug, projects))
	}
	b.WriteString("\n## What\n")
	if ev.IsIssue() {
		fmt.Fprintf(&b, "Issue: %s#%d\n", ev.RepoKey(), ev.Number)
	} else {
		fmt.Fprintf(&b, "Pull request: %s#%d\n", ev.RepoKey(), ev.Number)
	}
	if ev.URL != "" {
		fmt.Fprintf(&b, "url: %s\n", ev.URL)
	}
	if ev.Author != "" {
		fmt.Fprintf(&b, "author: %s\n", ev.Author)
	}
	if ev.BaseRef != "" || ev.HeadRef != "" {
		fmt.Fprintf(&b, "base: %s\n", nonEmptyOr(ev.BaseRef, "?"))
		fmt.Fprintf(&b, "head: %s\n", nonEmptyOr(ev.HeadRef, "?"))
	}
	if ev.Milestone != "" {
		fmt.Fprintf(&b, "milestone: %s\n", ev.Milestone)
	}
	if len(ev.Labels) > 0 {
		fmt.Fprintf(&b, "labels: %s\n", strings.Join(ev.Labels, ", "))
	}
	if ev.Body != "" {
		b.WriteString("\n## GitHub body\n")
		b.WriteString(strings.TrimSpace(ev.Body) + "\n")
	}
	b.WriteString("\n## Inbox\n")
	dir := TaskDir(slug)
	if dir == "" {
		dir = "~/.flow/tasks/" + slug
	}
	fmt.Fprintf(&b, "GitHub events for this item are appended to:\n  %s/inbox.jsonl\n", dir)
	b.WriteString("\n## Done when\n")
	b.WriteString("- The GitHub item has been reviewed, answered, or otherwise handled.\n")
	b.WriteString("- Relevant comments or decisions have been captured in flow before closing.\n")
	b.WriteString("\n## Tags\n")
	fmt.Fprintf(&b, "github, %s\n", ev.LinkTag())
	b.WriteString("\n---\n*GitHub-origin task. The GitHub listener inside flow ui serve writes incoming events to inbox.jsonl as they arrive.*\n")
	return b.String()
}

func renderGitHubProjectPicker(slug string, projects []projectChoice) string {
	var b strings.Builder
	b.WriteString("**Before doing anything else**, decide which flow project this GitHub task should belong to.\n\n")
	b.WriteString("1. Read the GitHub context and inbox events.\n")
	b.WriteString("2. From the list below, pick the 2–3 projects that look most relevant.\n")
	b.WriteString("3. Ask the operator in this session which one to use, offering an `adhoc` option if none fit.\n")
	b.WriteString("4. Once the operator answers, run exactly one of:\n\n")
	b.WriteString("   ```bash\n")
	b.WriteString("   flow update task " + slug + " --project <chosen-slug>\n")
	b.WriteString("   # or, if they pick adhoc / none:\n")
	b.WriteString("   flow update task " + slug + " --clear-project\n")
	b.WriteString("   ```\n\n")
	if len(projects) == 0 {
		b.WriteString("_No active projects found in flowdb. Ask the operator whether to leave this task as adhoc._\n")
		return b.String()
	}
	b.WriteString("**Available projects** (active, non-archived):\n\n")
	for _, p := range projects {
		line := "- `" + p.Slug + "`"
		if strings.TrimSpace(p.Name) != "" && p.Name != p.Slug {
			line += " — " + p.Name
		}
		if strings.TrimSpace(p.Priority) != "" {
			line += " · priority " + p.Priority
		}
		if strings.TrimSpace(p.UpdatedAt) != "" {
			line += " · last activity " + p.UpdatedAt
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func githubAutoOpenEnabled() bool {
	return envBoolDefault("FLOW_GH_AUTOOPEN", true)
}
