package app

import (
	"bytes"
	"encoding/json"
	"flow/internal/agenthooks"
	"flow/internal/flowdb"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// cmdHook dispatches `flow hook <subcommand>`. Main subcommands:
//
//   - session-start: wired as a Claude Code SessionStart hook so that
//     every session start (fresh spawn AND resume) re-injects the
//     "load your task context" instruction. Without it, resumed
//     sessions never re-read briefs and updates that may have been
//     edited since the previous session.
//
//   - user-prompt-submit: kept as a permanent no-op for forward
//     compatibility with stale settings.json entries.
//
//   - codex-run: internal wrapper used by `flow do --agent codex` so the
//     spawned shell can run Codex and capture the Codex-generated session id.
//
//   - agent-event: side-effect hook sink for Claude/Codex lifecycle
//     events. It forwards hook JSON to the local UI API without
//     printing stdout or blocking the agent when the UI is unavailable.
func cmdHook(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: hook requires a subcommand (session-start|user-prompt-submit|agent-event)")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "session-start":
		return cmdHookSessionStart(rest)
	case "user-prompt-submit":
		return cmdHookUserPromptSubmit(rest)
	case "codex-run":
		return cmdHookCodexRun(rest)
	case "agent-event":
		return cmdHookAgentEvent(rest)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown hook subcommand %q\n", sub)
		return 2
	}
}

// cmdHookSessionStart emits a Claude Code SessionStart hook response.
// Wired via ~/.claude/settings.json with a matcher of "startup|resume"
// so it fires for both fresh spawns and `claude --resume`.
//
// Two modes, branching on whether this session is bound to a flow
// task. The binding is discovered via reverse-lookup on the
// $CLAUDE_CODE_SESSION_ID env var (Claude Code injects this into
// every session) against tasks.session_id:
//   - Bound (a task carries this session_id): emit the full
//     task-context reload instructions. On a fresh spawn this is
//     redundant with the bootstrap prompt but harmless; on a resume
//     it's the only way to force the agent to re-read potentially-
//     updated briefs and updates.
//   - Unbound (no task carries it, or env var missing): emit the
//     ambient skill hint so Claude knows the flow skill is available.
func cmdHookSessionStart(args []string) int {
	fs := flagSet("hook session-start")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	slug := lookupBoundTaskSlug()
	if slug == "" {
		return emitAmbientSkillHint()
	}

	instructions := fmt.Sprintf(
		"You are running inside a flow execution session for task %q. "+
			"Before doing anything else in this turn, re-load your task context — "+
			"the brief and update files may have been edited since your previous "+
			"session. Do these in order: "+
			"(1) invoke the `flow` skill via the Skill tool. That skill is your "+
			"operating manual for this session: it defines the bootstrap contract, "+
			"the workflows for starting/saving/logging/archiving work, KB scoop "+
			"discipline, and the scope-creep detection that keeps unrelated work "+
			"from landing in the wrong task. "+
			"(2) run `flow show task` and use your Read tool on the file at the "+
			"`brief:` path AND every file listed under `updates:`; "+
			"(3) if a project is listed on the task, run `flow show project <that-slug>` "+
			"and Read its brief and updates too; "+
			"(4) Read `CLAUDE.md` in your work_dir and any nested CLAUDE.md under "+
			"subdirectories you plan to modify. "+
			"Only then proceed with the user's request. "+
			"If any brief section is blank or unclear, ASK — do not infer. "+
			"The `kb:` section of `flow show task` lists the knowledge-base files "+
			"(durable facts about the user, org, products, processes, business). "+
			"DO NOT read these eagerly on every turn — lazy-load only when the current "+
			"task requires that context (e.g. a brief that uses domain-specific terminology "+
			"you don't recognize, a question about who someone is, a request for org context). "+
			"Throughout the session, if the user shares a durable fact about themselves, "+
			"the org, products, processes, or business, append it to the matching kb "+
			"file on the fly — no permission needed — per the flow skill's §4.10.",
		slug,
	)

	inboxHint := appendInboxHint(slug)
	return emitSessionStartContext(inboxHint + instructions + appendStaleHookHint(slug) + appendStaleVersionHint())
}

// appendInboxHint prepends an INBOX-UPDATED notice when ~/.flow/tasks/<slug>/
// inbox.md has been modified since the task's last inbox_seen_at. After
// emitting the notice it bumps tasks.inbox_seen_at so the same message
// isn't re-flagged on every SessionStart for the rest of the session.
//
// Best-effort: any error (no DB, no file, mtime parse failure) returns ""
// silently. SessionStart latency is on the user's critical path; an
// errored hook would block the agent from starting.
func appendInboxHint(slug string) string {
	root, err := flowRoot()
	if err != nil {
		return ""
	}
	inboxPath := root + "/tasks/" + slug + "/inbox.md"
	info, err := os.Stat(inboxPath)
	if err != nil {
		return ""
	}
	mtime := info.ModTime().UTC().Format(time.RFC3339)

	dbPath, err := flowDBPath()
	if err != nil {
		return ""
	}
	db, err := flowdb.OpenDB(dbPath)
	if err != nil {
		return ""
	}
	defer db.Close()
	task, err := flowdb.GetTask(db, slug)
	if err != nil {
		return ""
	}
	if task.InboxSeenAt.Valid && task.InboxSeenAt.String >= mtime {
		return ""
	}

	// Bump inbox_seen_at so subsequent SessionStart hooks (e.g. claude
	// resume after a turn) don't re-fire the same notice. Best-effort.
	_, _ = db.Exec(`UPDATE tasks SET inbox_seen_at = ?, updated_at = ? WHERE slug = ?`,
		mtime, flowdb.NowISO(), slug)

	return fmt.Sprintf(
		"INBOX UPDATED: %s has new message(s) since your last session. "+
			"BEFORE doing anything else this turn, Read %s — these are "+
			"instructions from your parent task or the user that you "+
			"haven't acted on yet. Then proceed with your normal "+
			"context reload below.\n\n",
		slug, inboxPath,
	)
}

// appendStaleHookHint nudges the agent to refresh repo-local hooks when
// the installed --hook-version is below the current binary's expectation.
// It probes the task's workdir for .claude/settings.local.json (the
// repo-local hook file flow do installs) and parses the stamped version
// out of any flow hook agent-event command. Best-effort: returns "" if
// the file is missing, unparseable, or up to date. We must never block
// session start on a parse error.
func appendStaleHookHint(slug string) string {
	dbPath, err := flowDBPath()
	if err != nil {
		return ""
	}
	db, err := flowdb.OpenDB(dbPath)
	if err != nil {
		return ""
	}
	defer db.Close()
	task, err := flowdb.GetTask(db, slug)
	if err != nil || strings.TrimSpace(task.WorkDir) == "" {
		return ""
	}
	installed := minInstalledHookVersion(task.WorkDir)
	if installed == 0 || installed >= agenthooks.CurrentHookVersion {
		return ""
	}
	return fmt.Sprintf(
		" flow-hook-stale: repo-local agent hooks for this workdir are at version %d "+
			"but the running flow binary expects %d. Live status updates may miss "+
			"events the new hook surface emits. At a natural pause, suggest the user "+
			"refresh hooks (closing and re-opening this task with `flow do %s` will "+
			"reinstall them automatically).",
		installed, agenthooks.CurrentHookVersion, slug,
	)
}

// minInstalledHookVersion returns the smallest --hook-version stamped on
// any flow-installed agent-event hook entry in workDir. Returns 0 if no
// flow hook is present or all entries pre-date the version flag.
func minInstalledHookVersion(workDir string) int {
	min := 0
	for _, rel := range []string{".claude/settings.local.json", ".codex/hooks.json"} {
		v := scanHookFileVersion(workDir + "/" + rel)
		if v == 0 {
			continue
		}
		if min == 0 || v < min {
			min = v
		}
	}
	return min
}

func scanHookFileVersion(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return 0
	}
	hooks, _ := cfg["hooks"].(map[string]any)
	min := 0
	for _, groupRaw := range hooks {
		groups, _ := groupRaw.([]any)
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			inner, _ := gm["hooks"].([]any)
			for _, h := range inner {
				hm, _ := h.(map[string]any)
				cmd, _ := hm["command"].(string)
				if !strings.Contains(cmd, "hook agent-event") {
					continue
				}
				v := agenthooks.HookVersionFromCommand(cmd)
				if v > 0 && (min == 0 || v < min) {
					min = v
				}
			}
		}
	}
	return min
}

// appendStaleVersionHint returns a short suffix to add to SessionStart
// hint payloads when the local flow binary is older than the latest
// GitHub release. Returns "" when the check fails, the cache is fresh
// and matches, or the running binary is a dev build (no version
// embedded). The check is best-effort and silent on any error —
// session-start latency must not be impacted by a flaky network.
func appendStaleVersionHint() string {
	if Version == "" || Version == "dev" {
		return ""
	}
	latest := LatestRelease()
	if latest == "" || latest == Version {
		return ""
	}
	return fmt.Sprintf(
		" flow-version-stale: %s — the running flow binary is %s but a newer release is available. "+
			"When natural, offer the user an upgrade per skill §4.15 (Upgrade flow itself). "+
			"Do not interrupt active work to push this; surface it at a pause.",
		latest, Version,
	)
}

// lookupBoundTaskSlug returns the slug of the task whose session_id
// matches $CLAUDE_CODE_SESSION_ID, or "" if no such task exists, the
// env var is unset, or the DB lookup fails. Hook code must never
// fail loud — a hook error blocks the user's session — so all errors
// are swallowed and treated as "unbound".
func lookupBoundTaskSlug() string {
	sid := os.Getenv("CLAUDE_CODE_SESSION_ID")
	if sid == "" {
		return ""
	}
	dbPath, err := flowDBPath()
	if err != nil {
		return ""
	}
	db, err := flowdb.OpenDB(dbPath)
	if err != nil {
		return ""
	}
	defer db.Close()
	t, err := flowdb.TaskBySessionID(db, sid)
	if err != nil {
		return ""
	}
	return t.Slug
}

// emitAmbientSkillHint is the unbound-session branch of the
// SessionStart hook. The user has installed flow — a personal task/
// session manager and knowledge base — and benefits from having work
// flow through it. The hint frames that value prop and tells Claude
// to load the skill; it deliberately does NOT ask Claude to pre-judge
// whether the user's request is "substantive enough". That gate is
// the skill's job, not the hook's.
func emitAmbientSkillHint() int {
	// Substitute the real flow root so the hint points at the right
	// paths under FLOW_ROOT (default ~/.flow). Falling back to literal
	// "~/.flow" keeps the hint useful if home-dir lookup fails.
	root := "~/.flow"
	if r, err := flowRoot(); err == nil {
		root = r
	}
	hint := "This Claude session is not bound to a flow task — this hint IS the " +
		"binding answer. Do NOT re-probe binding with `flow show task` (no arg) " +
		"until you've actually bound this session via `flow do --here <slug>`; " +
		"until then it will error and waste a tool call. The user already tracks " +
		"their work and knowledge in flow — a personal task and session manager that " +
		"captures work as briefs, logs progress notes, resumes Claude sessions across " +
		"days, and maintains a central knowledge base of durable facts about them, " +
		"their org, and their work. Their tasks, projects, and KB already exist. " +
		"Invoke the `flow` skill via the Skill tool now and let it run alongside the " +
		"conversation. When the user starts any work in this session, before doing it, " +
		"confirm with them via AskUserQuestion: is this part of an existing flow task, " +
		"or should we create a new one? The user's work belongs in flow — don't " +
		"silently proceed in an unbound transcript. The skill knows how to list " +
		"candidates, run intake, and `flow do` the chosen task. " +
		"\n\n" +
		"If the user's message uses unfamiliar terminology — an internal codename, a " +
		"person, a customer, a product line, a tool you don't recognize — consult " +
		"flow's data before guessing or asking. The KB at " + root + "/kb/ holds durable " +
		"facts; the briefs under " + root + "/projects/<slug>/ and " + root + "/tasks/<slug>/ " +
		"hold project and task context. Names and context that are non-obvious from " +
		"this conversation alone are very likely already documented there — and not " +
		"only in active work: when the reference points at past work, also consult " +
		"done and archived tasks/projects (which need explicit `--status done` / " +
		"`--include-archived` flags on the list commands). The skill's §4.10 governs " +
		"how to lazy-load these without reading them eagerly every turn."
	return emitSessionStartContext(hint + appendStaleVersionHint())
}

// emitSessionStartContext is a thin wrapper around emitHookContext for
// the SessionStart event.
func emitSessionStartContext(ctx string) int {
	return emitHookContext("SessionStart", ctx)
}

// emitHookContext marshals a hookSpecificOutput payload for the given
// Claude Code hook event name. Used by both SessionStart and
// UserPromptSubmit hook handlers.
func emitHookContext(event, ctx string) int {
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     event,
			"additionalContext": ctx,
		},
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fmt.Fprintf(os.Stderr, "error: encode hook json: %v\n", err)
		return 1
	}
	return 0
}

// cmdHookUserPromptSubmit is a permanent no-op kept only for forward
// compatibility with stale `~/.claude/settings.json` entries.
// `flow skill install` (and the auto-upgrade path) now actively
// remove any UserPromptSubmit entry from settings.json, so this code
// path should not be hit on upgraded installs.
func cmdHookUserPromptSubmit(args []string) int {
	fs := flagSet("hook user-prompt-submit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return 0
}

func cmdHookAgentEvent(args []string) int {
	fs := flagSet("hook agent-event")
	provider := fs.String("provider", "", "agent provider (claude|codex)")
	apiURL := fs.String("url", "", "flow UI hook endpoint")
	timeout := fs.Duration("timeout", 1500*time.Millisecond, "forward timeout")
	hookVersion := fs.Int("hook-version", 0, "hook script version (set by installer)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		debugAgentHook("read stdin: %v", err)
		return 0
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	if !json.Valid(raw) {
		debugAgentHook("invalid hook JSON")
		return 0
	}

	// Inject side-band metadata into the payload before forwarding:
	//   - flow_seq: monotonic time.UnixNano() for causal ordering on the
	//     server (the agent harness only stamps RFC3339 timestamps, which
	//     collide on bursty events and lose to goroutine scheduling jitter)
	//   - flow_hook_owned: distinguishes flow-spawned sessions (FLOW_HOOK_OWNED=1
	//     set by flow do) from ambient agents running in flow-managed repos
	//   - flow_hook_version: installer-stamped version, lets the server detect
	//     outdated hook entries and surface an upgrade hint at SessionStart
	raw = injectHookMetadata(raw, *hookVersion)

	endpoint := agentHookEndpoint(*apiURL, *provider)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		debugAgentHook("build request: %v", err)
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: *timeout}
	resp, err := client.Do(req)
	if err != nil {
		debugAgentHook("post hook event: %v", err)
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		debugAgentHook("hook endpoint returned %s", resp.Status)
	}
	return 0
}

// injectHookMetadata stamps flow_seq, flow_hook_owned, and flow_hook_version
// onto the hook JSON. Returns raw unchanged if the payload isn't a JSON
// object — the server tolerates payloads without these fields and falls
// back to last_seen_at ordering. We never overwrite an already-present
// flow_seq so tests can inject deterministic values.
func injectHookMetadata(raw []byte, hookVersion int) []byte {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return raw
	}
	if _, present := obj["flow_seq"]; !present {
		obj["flow_seq"] = time.Now().UnixNano()
	}
	obj["flow_hook_owned"] = os.Getenv("FLOW_HOOK_OWNED") == "1"
	if hookVersion > 0 {
		obj["flow_hook_version"] = hookVersion
	}
	if out, err := json.Marshal(obj); err == nil {
		return out
	}
	return raw
}

func agentHookEndpoint(rawURL, provider string) string {
	endpoint := strings.TrimSpace(rawURL)
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv("FLOW_HOOK_URL"))
	}
	if endpoint == "" {
		base := strings.TrimRight(strings.TrimSpace(os.Getenv("FLOW_UI_URL")), "/")
		if base != "" {
			endpoint = base + "/api/hooks/agent"
		}
	}
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8787/api/hooks/agent"
	}
	if strings.TrimSpace(provider) == "" {
		return endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	q := u.Query()
	q.Set("provider", strings.TrimSpace(provider))
	u.RawQuery = q.Encode()
	return u.String()
}

func debugAgentHook(format string, args ...any) {
	if os.Getenv("FLOW_HOOK_DEBUG") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "flow hook agent-event: "+format+"\n", args...)
}
