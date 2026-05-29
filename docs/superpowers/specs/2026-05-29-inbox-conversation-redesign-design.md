# Inbox redesign — conversation view with terminal/spawn action

**Date:** 2026-05-29
**Status:** Approved (brainstorming) → ready for implementation planning

## Problem

The current Inbox page (`/inbox`) is a flat, time-bucketed feed of individual
inbox events. Every Slack message and GitHub PR event becomes its own row, so a
single Slack thread (e.g. `SLACK-C0B3L0D8QG1-…`) shows up as 5+ near-identical
rows that all read "Slack message" — because the list renders `body_snippet`,
which trims to the title line. The actual message text, sender, and source are
not shown, even though the data is already fetched.

Two problems to solve:

1. **No conversation view.** Users can't read the actual back-and-forth (Slack
   thread / PR comments) routed into a task without leaving the inbox.
2. **Raw IDs leak.** Senders render as `U09KX234ZRS`, channels as
   `C0B3L0D8QG1`, and slugs as `SLACK-…`. The UI must show human-readable
   names everywhere — **no raw IDs in the UI**.

And one capability to add: a button per conversation that **opens the live
terminal if a session is running, else spawns an agent**.

## Goals

- Two-pane master–detail "conversation" layout on `/inbox`.
- Left pane: one row **per conversation** (grouped by task), no raw IDs.
- Right pane: the selected conversation's messages, rendered as a readable
  thread with resolved names, source styling, and permalinks.
- A live-aware action button: live → open terminal; backlog → spawn agent.
- Resolve Slack user IDs → display names and channel IDs → channel names,
  lazily and cached. No raw IDs anywhere in the rendered UI.

## Non-goals (YAGNI)

- No reply/compose box. The action is "open terminal / spawn agent"; the agent
  handles replies.
- No new persistent storage or schema migration. Resolution is read-time and
  cached; `inbox.jsonl` is unchanged.
- No change to how events are ingested by the monitor listeners.

## Decisions (from brainstorming)

| Decision | Choice |
| --- | --- |
| Left list granularity | **One row per conversation** (grouped by `task_slug`, which is the thread identity in this setup) |
| Where the terminal opens | **Navigate to the existing `session/<slug>` page** (reuse current behavior; no terminal re-wiring) |
| Right-pane data richness | **Enrich** with source, kind, permalink, and resolved names |
| Name resolution | **Read-time, lazy per opened conversation, cached** (avoids rate-limited bulk `users.info` on every load) |
| Left-row preview | task name + source icon + message count + latest time + unread accent (no per-sender preview, to keep resolution lazy) |

## Architecture

### Existing pieces reused

- **Feed handler:** `internal/server/inbox_md.go` `handleInbox()` (lines
  200–282) walks every non-archived task's `inbox.jsonl` via
  `monitor.ReadInboxEntries(slug)` and flattens each `monitor.InboundEvent`
  into an `InboxFeedEntry`.
- **Event shape:** `monitor.InboundEvent` (`internal/monitor/inbound_event.go:29`)
  carries `Kind`, `Channel`, `ChannelType` (`slack`/`github`/…), `TS`,
  `ThreadTS`, `UserID`, `Text`, `URL`, `Reaction`, `ItemAuthor`.
- **Name resolution:** `internal/monitor/slack_title.go` —
  `slackUserDisplayName()` (line 230) resolves user ID → display name;
  `ConversationInfo()` (line 65) resolves channel ID → name;
  `cleanSlackTitleText()` (line 248) rewrites in-text `<@U…>` mentions and
  `<url|label>` links into readable text. Token via `SlackBotToken()`.
  **No cache today** — every call hits the Slack API.
- **GitHub permalinks:** `internal/monitor/github_client.go:536` already sets
  `URL: r.HTMLURL`; GitHub senders are human logins. No resolution needed.
- **Liveness:** `internal/server/ui_data.go` `uiAgent()` (line 486) sets `Live`
  by checking the task's `SessionID` against `cachedLiveAgentSessions()`.
- **Action button pattern:** `c906f42d…js:4334` `sessionButton` already
  implements live→"Resume terminal" (`action('attach')`), backlog→"Spawn
  session" (`action('spawn')`), done→"View transcript", else→"Open session".
  Both `attach` and `spawn` end with `goto('session/<slug>')`
  (`index.html:2140`, `2285`).

### New pieces

#### 1. Cached Slack name resolver (`internal/monitor`)

A small exported, concurrency-safe, TTL-cached resolver wrapping the existing
`slackTitleAPIClient`:

```go
// SlackNameResolver resolves Slack user/channel IDs to display names,
// caching results in-memory with a TTL to avoid rate-limited repeat calls.
type SlackNameResolver struct { /* client, mu, userCache, chanCache, ttl */ }

func NewSlackNameResolver() *SlackNameResolver        // nil-safe if no token
func (r *SlackNameResolver) UserName(ctx, id) string    // "" if unresolved
func (r *SlackNameResolver) ChannelName(ctx, id) string // "" if unresolved
func (r *SlackNameResolver) CleanText(text) string      // reuse cleanSlackTitleText
```

- Cache entries: `map[string]cacheEntry{ name string; at time.Time }`, guarded
  by a mutex, TTL ~1h.
- When the resolver is `nil` (no token) or a lookup errors, callers fall back to
  a neutral label — never the raw ID.
- Lives in `monitor` because it owns the Slack client + token.

#### 2. Feed enrichment (`internal/server`)

Add fields to `InboxFeedEntry` (`internal/server/types.go:154`):

```go
Source string `json:"source"` // "slack" | "github" | "" (from ChannelType/Kind)
Live   bool   `json:"live"`    // task session currently running
```

`handleInbox()` (`inbox_md.go`) sets `Source` from `ev.ChannelType`/`ev.Kind`
(cheap, no API) and `Live` by cross-referencing the same
`cachedLiveAgentSessions()` set `uiAgent` uses. This powers the left list's
source icons and the button state without any Slack API calls.

#### 3. Conversation endpoint (`internal/server`)

New `GET /api/inbox/conversation?slug=<task>` returning one task's full ordered
thread, with names resolved (lazy, only for this conversation's unique users):

```go
type InboxConversation struct {
    Slug        string                    `json:"slug"`
    Name        string                    `json:"name"`         // human task name
    ProjectSlug *string                   `json:"project_slug,omitempty"`
    Status      string                    `json:"status"`
    Live        bool                      `json:"live"`
    ChannelName string                    `json:"channel_name,omitempty"` // resolved
    Source      string                    `json:"source"`       // slack|github|mixed
    Messages    []InboxConversationMessage `json:"messages"`     // chronological
}

type InboxConversationMessage struct {
    Source     string `json:"source"`      // slack | github
    Kind       string `json:"kind"`        // message, pr_review_comment, …
    SenderName string `json:"sender_name"` // resolved; never a raw ID
    Timestamp  string `json:"timestamp"`
    Title      string `json:"title"`       // humanised kind, e.g. "PR review requested"
    Body       string `json:"body"`        // mentions/links cleaned via CleanText
    Permalink  string `json:"permalink,omitempty"` // ev.URL (github) or built slack archive URL
    Reaction   string `json:"reaction,omitempty"`
}
```

Handler:
1. Resolve task by slug; 404 if archived/unknown.
2. `monitor.ReadInboxEntries(slug)` → chronological events.
3. For each event, build a message:
   - `SenderName`: GitHub → `ItemAuthor`/`UserID` (already a login). Slack →
     `resolver.UserName(UserID)`; fall back to `"unknown"` if unresolved.
   - `Body`: `resolver.CleanText(ev.Text)` so in-text `<@U…>` mentions become
     readable; GitHub text passes through.
   - `Permalink`: GitHub → `ev.URL`. Slack → `ev.URL` if present, else build the
     archive URL from channel + ts when possible, else omit.
   - `Source`/`Kind` from `ChannelType`/`Kind`.
4. `ChannelName`: `resolver.ChannelName(channel)` for Slack; omit for GitHub.
5. `Live`/`Status`: same liveness cross-reference as the feed.

Register in `internal/server/routes.go` next to `/api/inbox`.

#### 4. Frontend (`c906f42d…js` + `index.html` styles)

Rewrite `InboxView` (`c906f42d…js:1071`) as a two-pane master–detail:

- **Left pane (conversation list):** group `feed.entries` by `task_slug`
  client-side. Each conversation row = task **name** (human), source icon
  (Slack/GitHub from `entry.source`), message count, latest time, unread accent.
  Keep the existing filters/search (project/task/received/unread/search),
  applied to the grouped list. **No `task_slug` / IDs shown as primary text**
  (slug only in a `title=` tooltip if at all).
- **Selection state:** `selectedSlug`; clicking a left row fetches
  `/api/inbox/conversation?slug=` and renders the right pane. Default-select the
  first conversation on load (desktop).
- **Right pane (thread):** header with task name, channel name + source, and the
  **action button** (see below). Body = chronological messages: sender name,
  relative time, source badge, optional "↗ open in slack / open PR #N"
  permalink, and cleaned body text. Per-source accent styling.
- **Action button:** reuse the `sessionButton` logic verbatim, driven by the
  conversation's `live`/`status`:
  - live → `Open terminal` → `action('attach', { slug, provider, hasAgent: true })`
  - backlog → `Spawn agent` → `action('spawn', { slug, provider })`
  - in-progress not live → `Open session` (`goto('session/'+slug)`)
  - done → `View transcript`
- **Responsive:** below a width breakpoint, show list-only; selecting pushes the
  detail view with a back button.

## Data flow

```
Left list:   GET /api/inbox  ──▶ group by task_slug ──▶ conversation rows
                              (source + live enriched; no API calls, cheap)

Open convo:  click row ──▶ GET /api/inbox/conversation?slug=<task>
                          ──▶ server resolves names (cached) for this thread
                          ──▶ right pane renders messages (no raw IDs)

Action:      Open terminal / Spawn ──▶ action('attach'|'spawn')
                          ──▶ goto('session/<slug>')  (existing full terminal)
```

## "No raw IDs" guarantee

- Left rows use the already-resolved human **task name**, never the slug.
- Right-pane senders/channels go through the cached resolver.
- Message bodies are passed through `CleanText` so `<@U…>` mentions and bare
  link IDs are rewritten.
- On resolution failure / no token, render `unknown` (user) or omit the channel
  — never the raw ID.
- Raw slugs/IDs may appear only in `title=` tooltips, never as visible text.

## Error handling

- Conversation endpoint: 400 on missing/invalid slug, 404 on unknown/archived
  task, 200 with `messages: []` when the task has no `inbox.jsonl`.
- Resolver: nil-safe with no token; per-lookup errors degrade to neutral labels.
- Frontend: per-pane loading + error states; left list keeps the existing 15s
  refresh backstop; right pane refetches when `selectedSlug` changes.

## Testing

- **Go (`internal/server`, `internal/monitor`):**
  - Conversation endpoint: grouping, source detection, mention cleaning,
    permalink selection (github vs slack), fallback when no token, 400/404.
  - Resolver cache: miss → API call, hit → cached, TTL expiry, nil client.
    Reuse the existing fake `SlackTitleClient`.
  - Feed enrichment: `source` + `live` populated correctly.
- **Frontend:** manual — `make build`, open `/inbox`, verify grouping, thread
  rendering, name resolution (no IDs), and the live/backlog button paths. The
  bundle is hand-maintained; there is no JS test harness.

## Files to touch

- `internal/monitor/slack_title.go` (or new `slack_name_resolver.go`) — exported
  cached resolver.
- `internal/server/types.go` — `Source`/`Live` on `InboxFeedEntry`;
  `InboxConversation` + `InboxConversationMessage`.
- `internal/server/inbox_md.go` — enrich feed; new `handleInboxConversation`.
- `internal/server/routes.go` — register `/api/inbox/conversation`.
- `internal/server/static/assets/c906f42d-…js` — rewrite `InboxView`.
- `internal/server/static/index.html` — two-pane styles.
- Tests alongside the above.
- `internal/app/skill/SKILL.md` + `README.md` — note the new inbox behavior if
  the skill describes the inbox.
