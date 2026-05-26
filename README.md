<p align="center">
  <img src="assets/flow-logo-v2.svg" alt="flow" width="480">
</p>

<p align="center">
  <img src="https://img.shields.io/badge/status-alpha-orange" alt="Status">
  <img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="License">
</p>

> A complete task manager for **Claude Code and Codex** — with
> first-class **Slack** triggers, a browser-based **Mission Control**,
> and a working memory layer that turns every session from a brilliant
> new hire into the engineer on your team.

This README captures the current state. flow ships new features
weekly — what's documented below is the floor, not the ceiling. The
[changelog](CHANGELOG.md) carries every release.

## See it in action

A four-act demo of how flow compounds context across days and tasks.
The work is silly on purpose — Star Trek bridge starships — so the
mechanic is what you watch, not the code.

**Act 1 — Capture the work.** Just talk. flow interviews you for
what / why / where / done-when, drafts a structured brief, and opens
a dedicated Claude session for the task in a new tab.

![Act 1 — task intake](docs/demo/01-task-intake.gif)

**Act 2 — Work, then park.** The session has the brief, the project
context, and the knowledge base loaded. You build until you hit a
blocker — here, "Kirk needs to review this" — and tell Claude to
park it. Status flips to `waiting`. Tab can close.

![Act 2 — execution and park](docs/demo/02-task-execution.gif)

**Act 3 — Resume and close.** A day later you say "Kirk signed off."
Same session resumes with full memory of where it left off. `flow
done` flips status and triggers the sweep — Claude re-reads the
whole transcript and writes durable facts (Kirk approved the design,
the ship class, the conventions used) into the knowledge base.

![Act 3 — approval and close](docs/demo/03-approval-and-close.gif)

**Act 4 — Months later, a new captain.** New task: "Picard's the new
boss, he wants the starship as an SVG." Brand new session, but it
already knows the ship, knows the design choices Kirk approved,
knows the project conventions — because the KB carried it. Claude
just gets to work.

![Act 4 — Picard's SVG upgrade](docs/demo/04-picard-svg-upgrade.gif)

That fourth session is what flow is really about. Not the first
session — the fiftieth.

## Why flow

If you use Claude Code daily, you've felt the ceiling: every session
is a new hire. Brilliant, capable, ready to help — but with no memory
of yesterday's decisions, last week's migrations, or the half-finished
threads in your other tabs. You spend the first ten minutes of every
session catching it up.

flow changes the relationship. It's a complete task manager —
projects, tasks, structured briefs, progress notes, playbooks for
recurring work — *and* a working memory layer that injects all of it
into every Claude session automatically. Capture once, work with
Claude on it forever.

The first session feels normal. By session ten, Claude knows your
codebase quirks, your team, the customer you keep mentioning, and
the migration you're three steps into. By session fifty, it's the
engineer on your team — not a new hire you re-explain yourself to
every morning.

Built for power users who want Claude to *work with them*, not just
*help them*.

## How context compounds

Every task feeds the same knowledge base. Every closed task makes
the next one smarter.

```
                                       ┌────────────────────────┐
                                       │   ~/.flow/kb/          │
                                       │   user · org · products│
                                       │   processes · business │
                                       └─────▲──────────▲───────┘
                                             │          │
                  flow do <task>             │ scoop    │ sweep
   ┌────────┐  ─────────────────▶  ┌─────────┴──────────┴─────┐
   │  Task  │                      │      Claude session      │
   │  brief │  ◀──── updates ───── │  loads brief + kb +      │
   │ +notes │                      │  notes + repo conventions│
   └────────┘  ─── flow done ───▶  └──────────────────────────┘
                                       (auto-sweep transcript
                                        into kb on done)
```

- **Scoop (live):** during a session the flow skill listens for
  durable facts you mention — your role, a teammate's name, a
  product convention — and appends them to the matching kb file
  on the fly.
- **Sweep (on `flow done`):** when you close a task, flow spawns
  a headless Claude pass that re-reads the entire transcript and
  pulls anything kb-worthy that the live scoop missed. The status
  flip is the contract; the sweep is best-effort.
- **Cross-reference:** `flow transcript <sibling-task>` lets a
  current session read what was decided in a related one — useful
  when the brief alone doesn't carry enough context.

Net effect: the longer you use flow, the more your knowledge base
grows, the less you re-explain yourself.

## Playbooks for the work you do on cadence

Some work repeats. Weekly reviews. Daily PR triage. On-call rotations.
Customer-meeting prep.

A **playbook** is a reusable run definition — a markdown brief that
describes what a run does. `flow run playbook weekly-review` snapshots
that brief into a fresh task and spawns a new Claude session against
it. Every run is reproducible (it executes against a frozen snapshot,
so editing the playbook later doesn't rewrite history) and contributes
back to the knowledge base on `flow done` like any other task.

```
┌──────────┐  flow run playbook weekly-review
│ Playbook │ ────────▶ snapshot ─────▶ new task ─────▶ new session
│  brief   │           (frozen for                     (executes
└──────────┘            reproducibility)                against snapshot)
```

Same compounding mechanic — your weekly review session two months from
now will know everything every prior weekly review surfaced.

## Mission Control — flow in the browser

`flow ui serve` boots a local web app at `127.0.0.1:8787`. Same
SQLite, same markdown briefs, same skill — just a richer surface for
the things terminals don't do well: side-by-side task lists, inline
brief editing, live agent status, and a browser-attached terminal that
streams the Claude or Codex session over WebSocket.

![Mission Control overview](docs/ui/01-mission-control.png)

Mission Control is a peer to the CLI, not a replacement. It reads and
writes the same `~/.flow/flow.db`, so the browser, your terminal
sessions, and the bundled skill always see one consistent view.

### Cmd+K everything

A global palette with FTS5 over briefs, updates, memories, tasks, and
commands. Type a few letters and jump. Press Enter and you're either
on the task detail page or directly attached to its live terminal.

![Cmd+K palette filtering to "kling"](docs/ui/cmdk.gif)

### Task detail with inline editors

Every task has a single page that shows status, priority, due date,
tags, agent provider (Claude or Codex), permission mode (default /
auto / bypass), session id, brief, and append-only updates. Priority
and permission mode are segmented controls — one click to change. New
task sessions default to `auto`; choose `default` when you want the
provider's prompt-on-request behavior.

![Task detail with priority and permission editors](docs/ui/02-task-detail.png)

### Projects, playbooks, and a tasks table

Every entity has a list and a detail page. The tasks table filters
on status and priority; project pages roll up task breakdowns;
playbook pages show the frozen brief that each run snapshots from.

<table>
<tr>
<td width="33%"><a href="docs/ui/06-tasks-list.png"><img src="docs/ui/06-tasks-list.png" alt="Tasks list with status and priority filters"></a><br><em>Tasks — filterable list</em></td>
<td width="33%"><a href="docs/ui/07-project-detail.png"><img src="docs/ui/07-project-detail.png" alt="Project detail with task breakdown"></a><br><em>Project detail</em></td>
<td width="33%"><a href="docs/ui/05-playbooks.png"><img src="docs/ui/05-playbooks.png" alt="Playbooks list"></a><br><em>Playbooks</em></td>
</tr>
</table>

### Knowledge base browser

Five markdown buckets — user, org, products, processes, business —
rendered as a two-pane reader. Scoop appends to these during
sessions; sweep adds more on `flow done`.

![Knowledge base browser](docs/ui/08-kb.png)

### Browser-attached terminal

The biggest reason to leave the CLI: when an agent is mid-tool-call
or waiting for input, the browser shows it. Click *Open session* on
any task and an xterm.js terminal streams the live Claude or Codex
session — same scrollback, same input. Reload the tab and the
snapshot syncs back.

### Same-session inbox monitor

Every monitored source writes normalized events to the task's
`~/.flow/tasks/<slug>/inbox.jsonl`. When that task has a live Flow-owned
terminal session, flow starts a task-local monitor that watches the
inbox and sends a short wake prompt into the same Claude or Codex
session. The monitor is generic: Slack, GitHub, and future sources use
the same append-to-inbox contract, and the agent continues the work in
place instead of spawning a separate solver.

Provider capability note: Claude Code's native background sessions are separate from Flow's monitor.
Codex currently exposes experimental app-server/remote-control building blocks, not a guaranteed Claude-style background scheduler that Flow can depend on.
Therefore Flow's supported path for both providers is the task-local inbox monitor + Flow-owned terminal wake; any native provider backend should be an opt-in integration behind this contract.

### What it doesn't do

No auth, no TLS, loopback only. Mission Control is a *local* tool —
if you want it on a public network, put your own auth in front. The
binary refuses to bind to non-loopback hosts without an explicit
`--host` flag for exactly this reason.

## Slack integration — react to triage

flow listens to a Slack workspace over Socket Mode and turns *your*
reactions into tasks. React to a thread with `:claude:` and a Claude
session spins up bound to that thread; react with `:codex:` and you
get a Codex session instead. Same task model, same KB, same UI —
just with Slack as one more input channel.

```
                        Slack Socket Mode
                              │
                              ▼
   ┌─────────────────────────────────────────────────┐
   │  monitor.SlackListener   (parses reaction_added)│
   └────────────────┬────────────────────────────────┘
                    │  is reactor in FLOW_SLACK_SELF_USER_IDS?
                    │  is emoji in trigger set?
                    ▼
   ┌─────────────────────────────────────────────────┐
   │  DecideReaction   → (channel, thread_ts, emoji) │
   └────────────────┬────────────────────────────────┘
                    │
                    ▼
   ┌─────────────────────────────────────────────────┐
   │  Dispatcher                                     │
   │   • find task by slack-thread:<channel>:<ts>    │
   │   • create one if absent                        │
   │   • pick provider: :claude: → claude            │
   │                    :codex:  → codex             │
   │   • append to inbox · auto-open in Mission Ctrl │
   └─────────────────────────────────────────────────┘
```

**Why reactions, not slash commands.** Slash commands let anyone in the
channel trigger you. Reactions are explicit consent from *your*
account, so a coworker's `:claude:` is harmless noise — only the IDs
you list in `FLOW_SLACK_SELF_USER_IDS` count.

**Per-emoji provider routing.** A `:claude:` reaction routes to a Claude
session; a `:codex:` reaction routes to Codex. Both pre-existing custom
emojis (e.g., `:flow-claude:`) and the literal `claude` / `codex`
shortnames are supported. Set extras via `FLOW_SLACK_TRIGGER_EMOJI`
(comma- or whitespace-separated, with or without colons).

**One thread, one task, forever.** flow tags each task with
`slack-thread:<channel>:<thread_ts>`. A second reaction on the same
thread won't create a duplicate — it appends to the existing task's
inbox. The task title is built from the message author's display
name and the first line of the message; if Slack's `channels:read`
scope is missing, flow falls back to the author name alone rather
than erroring.

**Configuration.**

| Env var                       | Purpose                                                        |
| ----------------------------- | -------------------------------------------------------------- |
| `FLOW_SLACK_APP_TOKEN`        | App-level token (`xapp-…`) — required for Socket Mode          |
| `SLACK_BOT_TOKEN`             | Bot/user token (`xoxb-…` / `xoxp-…`) for Web API calls         |
| `FLOW_SLACK_SELF_USER_IDS`    | Comma-separated Slack user IDs whose reactions count as you    |
| `FLOW_SLACK_TRIGGER_EMOJI`    | Trigger emoji shortnames (default: `claude`)                   |
| `FLOW_SLACK_SOCKET_MODE`      | `0` to disable Socket Mode while leaving tokens configured     |
| `FLOW_SLACK_OPEN_TARGET`      | `ui` (default, browser terminal) or `iterm` (legacy iTerm tab) |

The listener starts automatically when `flow ui serve` runs with the
above tokens set. Without tokens, the rest of flow works unchanged —
Slack is opt-in.

## GitHub integration — assigned work and review threads

flow can also poll GitHub through the authenticated `gh` CLI and turn
assigned issues, assigned/review-requested pull requests, review
comments, and top-level PR reviews on tracked pull requests into flow
work.

```
                           gh api polling
                                │
                                ▼
   ┌─────────────────────────────────────────────────┐
   │  monitor.GitHubListener                         │
   │   • search assigned issues / PRs                 │
   │   • search PRs requesting your review            │
   │   • track head changes / merge state for PRs      │
   │   • fetch review comments / reviews for PR tasks │
   └────────────────┬────────────────────────────────┘
                    │  is FLOW_GH_ENABLED=1?
                    │  is login in FLOW_GH_SELF_LOGINS?
                    ▼
   ┌─────────────────────────────────────────────────┐
   │  GitHubDispatcher                               │
   │   • find task by gh-pr:<owner>/<repo>#<n>        │
   │     or gh-issue:<owner>/<repo>#<n>               │
   │   • create one if absent                        │
   │   • pick provider from labels: flow:claude       │
   │                              or flow:codex       │
   │   • append to inbox · suppress duplicate events  │
   └─────────────────────────────────────────────────┘
```

**All repos by default.** Once enabled, GitHub search is not limited to
the current checkout. Set `FLOW_GH_REPOS=owner/repo,owner/repo2` when
you want a smaller allowlist.

**One GitHub item, one task.** PR tasks are tagged
`gh-pr:<owner>/<repo>#<number>` and issue tasks are tagged
`gh-issue:<owner>/<repo>#<number>`. A later assignment,
review-request, review comment, top-level review, commit, or merge for
the same PR/issue appends to the existing task's `inbox.jsonl` instead
of creating a duplicate. GitHub event keys are recorded in SQLite so
repeated polling does not double-append the same event. `flow done`
also tries to link the current branch PR automatically by adding the
`gh-pr:` tag.

**PR review lifecycle.** GitHub search only creates work for open PRs.
For already-tracked PR tasks, flow also polls the PR detail and review
endpoints: a `CHANGES_REQUESTED` review or new head SHA appends a
"review again" event and reopens the flow task if it was already done,
while a merged PR appends a merge event and marks the associated flow
task done. Approved reviews are recorded in the inbox but do not reopen
the task. flow does not blindly approve PRs; approval remains part of
the review task after the reviewer/agent has verified the latest diff.

**Provider routing via labels.** Add `flow:codex` to route a new
GitHub-origin task to Codex, or `flow:claude` to route it to Claude.
Without either label, flow defaults to Claude.

**Configuration.**

| Env var                 | Purpose                                                                    |
| ----------------------- | -------------------------------------------------------------------------- |
| `FLOW_GH_ENABLED`       | `1` to enable GitHub polling; default is off                               |
| `FLOW_GH_SELF_LOGINS`   | Comma-separated GitHub logins that count as you                            |
| `FLOW_GH_REPOS`         | Optional repo allowlist; unset means search all repos visible to `gh`      |
| `FLOW_GH_POLL_INTERVAL` | Poll interval such as `60s`, `2m`, or `120`; default is `1m`               |
| `FLOW_GH_AUTOOPEN`      | `0` to create tasks without opening a Mission Control terminal immediately |

Run `gh auth login` first, or provide the usual `GH_TOKEN` /
`GITHUB_TOKEN` environment that `gh api` supports. Without
`FLOW_GH_ENABLED=1`, the rest of flow works unchanged — GitHub is
opt-in.

## Install

In any Claude Code session, paste this:

> Install flow from https://github.com/Facets-cloud/flow

Claude reads the repo, downloads the binary, and runs `flow init` —
which installs the flow skill into `~/.claude/skills/flow/SKILL.md`
and registers a SessionStart hook so every future Claude session
loads the skill automatically. Then say **"let's get to work"** and
follow along.

<details>
<summary>Manual install (curl + chmod + flow init)</summary>

```bash
# 1. Download the binary for your Mac.
ARCH=arm64        # Apple Silicon (M1/M2/M3/M4) — use amd64 for Intel.

curl -fsSL -o /usr/local/bin/flow \
  "https://github.com/Facets-cloud/flow/releases/latest/download/flow-darwin-${ARCH}"
chmod +x /usr/local/bin/flow
xattr -d com.apple.quarantine /usr/local/bin/flow 2>/dev/null || true

# 2. Initialize. This is required — it creates ~/.flow/, the SQLite
#    index, the knowledge base, AND installs the Claude skill +
#    SessionStart hook. Without this step, Claude can't talk to flow.
flow init
```

`flow init` is the step that wires flow into Claude Code. It:

- Creates `~/.flow/` (database, kb, projects, tasks, playbooks)
- Writes the flow skill to `~/.claude/skills/flow/SKILL.md`
- Adds a SessionStart hook to `~/.claude/settings.json` so every new
  Claude Code session auto-loads the skill

The `xattr` step removes Gatekeeper's quarantine attribute so macOS
doesn't refuse to run the unsigned binary.

</details>

### For those who use Claude's agents view

`flow do` and `flow run` don't pass `--bg` today — the aliases below
will, so flow's sessions (and your own direct invocations) land in
the agents view. Add to your shell rc (`~/.zshrc` or `~/.bashrc`) and
`source` it.

```bash
# Bare `claude` now drops into the agents view. Add
# `--dangerously-skip-permissions` if you'd also like dispatched
# sessions to skip per-tool permission prompts (optional flavor).
alias claude='claude --bg'

# Since `claude` is now aliased, any `claude <sub>` invocation would
# also pick up `--bg`. For each subcommand you use, add an alias that
# routes through `command claude` to bypass the outer alias. The one
# below is for the agents subcommand — the same shape works for
# `mcp`, `doctor`, etc.
alias ca='command claude agents'
```

## Upgrade

In any Claude Code session:

> Upgrade flow from https://github.com/Facets-cloud/flow

Claude fetches the latest release binary and runs `flow skill
update` to refresh the skill and re-wire the SessionStart and
UserPromptSubmit hooks. Check the running version with
`flow --version`.

## Quickstart

Just open Claude and say **"let's get to work"**. The skill
handles the rest.

## What you get

- **One task, one agent session, one tab.** `flow do <task>`
  spawns a dedicated tab in iTerm2, Warp, stock macOS Terminal,
  kitty (requires `allow_remote_control yes` in `kitty.conf`), or
  your current zellij session (requires zellij ≥ 0.40) — flow
  picks whichever you launched it from. Override with
  `FLOW_TERM=warp|iterm|terminal|zellij|kitty` when you're on a
  non-standard host. Tomorrow's `flow do <task>` resumes the same
  conversation.
- **Claude or Codex, your call.** Default is Claude. Pass
  `--agent codex` (or `--codex`) on `flow add task`, `flow do`,
  or `flow run playbook` to bootstrap a Codex session instead.
  Provider is per-task — switching is a per-task decision, not a
  global one. The knowledge base, briefs, and close-out sweep
  work the same way either way.
- **Worktrees by default.** `flow do` creates a per-task git
  worktree at `<repo>/.<agent>/worktrees/<slug>` on branch
  `flow/<slug>`, so two parallel tasks on the same repo never
  step on each other's working tree. `flow do --here` binds the
  current Claude or Codex session and never relocates.
- **Auto-PR on done.** `flow done` pushes the worktree branch and
  runs `gh pr create` against the detected base branch with the
  task brief as the PR body. The PR URL is recorded against the
  task. After explicit user approval, pass `--merge` to merge the
  opened or existing PR with `gh pr merge --merge --delete-branch`.
  `--no-pr` opts out; push, PR, or merge failures warn and
  continue, never block the status flip.
- **Mission Control, in your browser.** `flow ui serve` boots a
  local web app at `127.0.0.1:8787` with task / project / playbook
  views, inline brief editing, a Cmd+K palette, and a browser-attached
  terminal that streams Claude or Codex sessions live. See the
  [Mission Control section](#mission-control--flow-in-the-browser).
- **Slack triggers.** React to a thread with `:claude:` or `:codex:`
  and flow spins up a task bound to that thread — same KB, same UI,
  Slack as one more input channel. See the
  [Slack section](#slack-integration--react-to-triage).
- **Full-text search over flow memory.** `flow search "<query>"`
  searches brief, update, Flow KB, Codex memory, and Claude memory
  markdown through SQLite FTS5. Add `--in transcripts` when you
  explicitly want session transcript search; transcripts stay out of
  the default scope because they are much larger. Use `--in all` to
  include every supported corpus.
- **Interview-driven task capture.** No forms. flow asks
  what / why / where / done-when, then writes a structured brief.
- **A knowledge base that grows.** Five markdown buckets for
  durable facts about you, your team, products, processes, and
  customers. Live-appended during sessions; auto-swept from
  transcripts on `flow done`.
- **Per-task progress notes.** Append-only logs. Pick up where
  you left off, even after a week away.
- **Playbooks for cadence work.** Weekly reviews, daily triage,
  on-call rotations — define once, run on demand.
- **Soft delete, then restore.** `flow delete` hides a task,
  project, or playbook from normal lists and the UI without
  touching its markdown. `flow restore` brings it back. Use
  `--include-deleted` (or `--deleted`) on `flow list` to see
  what's hidden.
- **A skill that speaks plain English.** "What should I work
  on", "resume auth", "save a note" — the bundled Claude skill
  turns intent into flow commands.

## How it works under the hood

`flow do <task>` resolves the task's provider (`claude` by
default, `codex` when the task was created with `--agent codex`),
pre-allocates or captures a session id, writes it to the task
row, and spawns a tab in zellij (when `$ZELLIJ` is set), kitty
(when `$KITTY_WINDOW_ID` is set or `$TERM=xterm-kitty`), the
backend named in `$FLOW_TERM` (when set), or Warp / iTerm2 / stock
Terminal.app (auto-detected from `$TERM_PROGRAM`) — chosen in
that priority order, with iTerm as the historical fallback —
running `claude --session-id <uuid>` (or the equivalent
`codex resume <uuid>`) with `FLOW_PROJECT` inlined. For Claude
the jsonl file lands at the deterministic path
`~/.claude/projects/<encoded-cwd>/<uuid>.jsonl`; for Codex it's
captured back from Codex's own session store. Either way, future
`flow do` calls resume the same conversation. A SessionStart
hook re-injects the task brief, updates, and CLAUDE.md context on
every resume.

### Worktrees, branches, and the close-out PR

By default `flow do` ensures a per-task git worktree at
`<repo>/.<agent>/worktrees/<slug>` on branch `flow/<slug>`,
forked from `origin/HEAD` (detected at task start). The agent
session is launched inside that worktree, so multiple tasks
against the same repo never collide. `tasks.worktree_path`
remembers the path; `flow show task` surfaces it. Worktree setup
errors stop launch instead of falling back to the shared checkout.

When you `flow done <task>`, flow snapshots the worktree's diff
against its starting HEAD, runs the close-out sweep, then pushes
the branch and runs `gh pr create --base <detected> --head
flow/<slug>` with the task brief as the PR body. The PR URL is
stored in `task_pr_links`. Pass `--merge` only after the user has
approved shipping; it merges the opened or already-recorded PR and
marks the stored PR link as merged. Pass `--no-pr` to skip; push,
PR, or merge failures warn and keep going (the status flip is the
contract).

### Focus instead of spawn for live sessions

When `flow do <task>` is run for a task whose session is already
live in another tab, flow focuses that tab instead of spawning a
duplicate. The source tab prints "Already open: `<slug>` — switched
to existing tab" as an audit line.

The first `flow do` from stock Terminal.app needs macOS Accessibility
permission for the **app hosting your shell** — not the `flow` binary
itself. Terminal.app's AppleScript dictionary has no "make new tab"
verb, so flow drives cmd-T through System Events, and System Events
checks Accessibility against the responsible parent app. Until that's
granted, `flow do` errors out with a multi-line explanation pointing at
System Settings → Privacy & Security → Accessibility (enable the
toggle for "Terminal" if you launched flow from Terminal.app, "iTerm"
from iTerm2, "Claude" if Claude Code is the host, etc.; add it via the
+ button if it's not listed). After the grant the spawn is silent.
iTerm2 doesn't need this — it has a native `create tab` verb.

### One-shot instructions with `--with`

`flow do <task> --with "<instruction>"` resumes (or starts) the
task's session and injects the instruction as the first user
message — prefixed with `[via flow do --with]` so the model can
tell injected input from typed input.

`--with-file <path>` is the same idea for longer instructions:
instead of embedding the file contents, flow injects `read
instructions at <absolute path>` and the session uses its Read
tool to load the file. No size limits. The flags are mutually
exclusive, and cannot be combined with `--here` (there's no
spawned session to inject into).

```bash
# Nudge a parked task without opening the tab.
flow do auth --with "check if upstream PR merged and update the brief if so"

# --with on a done task auto-rolls it back to in-progress, so playbooks
# can fire on previously-closed work.
flow do auth --with "are we still blocked on the security review?"

# Hand the session a longer brief to follow.
flow do auth --with-file ~/playbooks/triage-checklist.md
```

This is the lane scheduled playbooks use to fire instructions at
existing tasks without manual intervention. `flow run playbook
<slug>` accepts the same flags for ad-hoc per-run instructions.

### Agent hooks — what the UI knows about your sessions

The browser shows "agent idle / task in progress / waiting on you /
needs attention" because each running Claude or Codex session is
emitting lifecycle events through a repo-local
[agent-hooks](internal/agenthooks/) shim. `flow ui serve` installs
these into every known workdir automatically. Codex hooks are gated
to flow-owned terminals (`FLOW_HOOK_OWNED=1`) so ordinary Codex
sessions opened in the same repo never forward events into Mission
Control.

`flow ui serve` also accepts `--host`, `--port`, and `--bg`. Default
is `127.0.0.1:8787`. The Go HTTP server is a single binary — no node
runtime, no build step, no package install. The static UI ships
inside the binary.

## Your data — local, portable, yours

Everything flow stores lives under `~/.flow/` (override with
`$FLOW_ROOT`). No server, no cloud, no telemetry. Plain markdown
beside a SQLite index — readable in any editor, versionable in git.

```
~/.flow/
  flow.db                          # SQLite — projects, tasks, playbooks index
  kb/
    user.md  org.md  products.md
    processes.md  business.md      # 5 markdown buckets, append-only
  projects/<slug>/
    brief.md
    updates/YYYY-MM-DD-*.md
  tasks/<slug>/
    brief.md
    updates/YYYY-MM-DD-*.md
  playbooks/<slug>/
    brief.md
    updates/YYYY-MM-DD-*.md
```

The SQLite database is an *index*, not the source of truth — every
task and project has its real content in the markdown files next to
it. You could delete `flow.db` and rebuild it from the markdown if
you had to.

### Backup & sync

Pick whichever fits your workflow:

- **Git (recommended for single-user history).**
  ```bash
  cd ~/.flow && git init && git add . && git commit -m "initial"
  ```
  Commit periodically. The SQLite file is binary, so diffs aren't
  useful, but each commit is a clean snapshot. **If you push to a
  shared remote**, add `kb/` to `.gitignore` first — kb files often
  contain personal or org-sensitive notes you don't want public.

- **Time Machine / system backup.** Just works, no setup.

- **iCloud Drive / Dropbox / Google Drive.** Symlink `~/.flow` into
  the synced folder:
  ```bash
  mv ~/.flow ~/Library/Mobile\ Documents/com~apple~CloudDocs/flow
  ln -s ~/Library/Mobile\ Documents/com~apple~CloudDocs/flow ~/.flow
  ```
  ⚠️ **Don't run flow on two machines simultaneously** through a
  synced folder — SQLite doesn't tolerate concurrent writes from
  separate hosts and you can corrupt `flow.db`. Use this for backup
  + occasional second-machine access, not active multi-machine use.

- **Manual rsync.** `rsync -a ~/.flow/ /path/to/backup/flow/` on a
  schedule. Same caveat about concurrent writes.

To move flow to a new machine: copy `~/.flow/` over, install the
binary, and run `flow init` once — it'll pick up the existing data
and reinstall the skill + hook.

## Where flow runs (and where we'd love help)

Today flow runs on **macOS (iTerm2, Warp, stock Terminal.app,
kitty, or zellij) + Claude Code or Codex**. That's the stack we
use, and that's what the session-spawn layer was built and
tested against. zellij and kitty work on Linux too as a side
effect — both are cross-platform and flow's zellij / kitty
backends don't depend on any macOS APIs. Kitty needs
`allow_remote_control yes` (or `socket-only`) in `kitty.conf` so
flow can drive `kitty @ launch` from inside the running kitty
instance.

The architecture is portable — session spawning is one small
package, agent providers are pluggable via `internal/agents/` —
but other harnesses (Cursor, Aider, plain shell) and other
terminals (Linux + tmux/wezterm, Windows Terminal) need
contributors who run those stacks daily and care enough to wire
them in. If that's you, [a PR is very welcome](CONTRIBUTING.md).

## What's next on the roadmap

flow ships weekly and the surface area keeps growing. A few things
queued up that we use internally and want to land in the open
release:

- **Deeper GitHub workflows** — outbound replies/comments, webhook
  mode for low-latency installs, and richer GitHub Projects sync.
- **More providers, more terminals.** Cursor, Aider, plain shell,
  and Linux + tmux/wezterm are wired-but-not-blessed today.
  Contributors who run those stacks daily can graduate them to
  first-class — the session-spawn layer is intentionally small.
- **Sharper Mission Control.** Browser-side editing of more entity
  fields, richer agent-hook visualizations, and a built-in inbox
  for the things flow nags you about.

If any of these would unblock you, [open an
issue](https://github.com/Facets-cloud/flow/issues) — interest moves
things up the queue.

## Where flow came from

flow started as an internal tool at Facets. We use Claude Code every
day, and the context-loss problem was eating into how much of the
tool's capability we could actually use. flow fixed that for us — to
the point where we couldn't imagine working without it. We're
open-sourcing it as-is because it might do the same for you.

This is not a Facets product. There's no signup, no cloud, no upsell.
Just the tool we built for ourselves.

## Docs & contributing

- [Contributing](CONTRIBUTING.md) — bug reports, PRs, dev setup
- [Changelog](CHANGELOG.md)
- [Security](SECURITY.md) — how to report issues
- [Code of Conduct](CODE_OF_CONDUCT.md)

## License

[MIT](LICENSE) — © 2026 Facets Cloud
