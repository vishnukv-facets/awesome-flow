package monitor

import (
	"context"
	"strings"
	"sync"
	"time"
)

// slackNameCacheTTL bounds how long a resolved user/channel name is trusted
// before a re-lookup. Names change rarely, so an hour keeps the inbox UI
// responsive without hammering the rate-limited users.info /
// conversations.info endpoints.
const slackNameCacheTTL = time.Hour

type nameCacheEntry struct {
	name string
	at   time.Time
}

// SlackNameResolver resolves Slack user and channel IDs to human-readable
// names, caching results in-memory with a TTL. It exists so the Inbox UI can
// render display names instead of raw IDs (U…/C…) without making a Slack API
// call per row on every load — the cache absorbs repeat lookups across
// requests.
//
// Every method is nil-safe: a nil *SlackNameResolver (no Slack token
// configured) resolves to "", so callers MUST supply their own non-ID
// fallback. Negative lookups (id not found / API error) are cached as "" too,
// so a missing user doesn't trigger a fresh API call on every render.
//
// Safe for concurrent use.
type SlackNameResolver struct {
	client SlackTitleClient
	ttl    time.Duration

	mu    sync.Mutex
	users map[string]nameCacheEntry
	chans map[string]nameCacheEntry
}

// NewSlackNameResolver builds a resolver backed by the Slack API using the
// configured bot token. Returns nil when no token is available; callers treat
// a nil resolver as "cannot resolve".
func NewSlackNameResolver() *SlackNameResolver {
	client := newSlackTitleAPIClient()
	if client == nil {
		return nil
	}
	return NewSlackNameResolverWithClient(client)
}

// NewSlackNameResolverWithClient builds a resolver over any SlackTitleClient.
// Production wiring uses NewSlackNameResolver; this constructor lets callers
// (and tests) supply a custom or fake client so resolution and cache
// behaviour can be exercised without a real Slack token.
func NewSlackNameResolverWithClient(client SlackTitleClient) *SlackNameResolver {
	if client == nil {
		return nil
	}
	return &SlackNameResolver{
		client: client,
		ttl:    slackNameCacheTTL,
		users:  map[string]nameCacheEntry{},
		chans:  map[string]nameCacheEntry{},
	}
}

// UserName resolves a Slack user ID to a display name
// (DisplayName → RealName → Name). Returns "" when the resolver is nil, the id
// is blank, or the lookup fails — never the raw ID.
func (r *SlackNameResolver) UserName(ctx context.Context, userID string) string {
	userID = strings.TrimSpace(userID)
	if r == nil || userID == "" {
		return ""
	}
	if name, ok := r.lookup(r.users, userID); ok {
		return name
	}
	name := ""
	if user, err := r.client.UserInfo(ctx, userID); err == nil {
		name = firstNonEmpty(user.DisplayName, user.RealName, user.Name)
	}
	r.store(r.users, userID, name)
	return name
}

// ChannelName resolves a Slack channel ID to a channel name, "#"-prefixed for
// public/private channels. Returns "" when the resolver is nil, the id is
// blank, or the lookup fails — never the raw ID.
func (r *SlackNameResolver) ChannelName(ctx context.Context, channelID string) string {
	channelID = strings.TrimSpace(channelID)
	if r == nil || channelID == "" {
		return ""
	}
	if name, ok := r.lookup(r.chans, channelID); ok {
		return name
	}
	name := ""
	if conv, err := r.client.ConversationInfo(ctx, channelID); err == nil {
		name = strings.TrimSpace(conv.Name)
		if name != "" && (conv.IsChannel || conv.IsGroup) && !strings.HasPrefix(name, "#") {
			name = "#" + name
		}
	}
	r.store(r.chans, channelID, name)
	return name
}

// CleanText rewrites Slack message markup so a body never surfaces raw IDs:
//   - <@U123|label>  → @label
//   - <@U123>        → @<resolved name>  (or @user when unresolved)
//   - <url|label>    → label
//   - <url>          → url
//
// Unlike cleanSlackTitleText, it preserves newlines and does not truncate —
// it is meant for full message bodies, not titles. Non-Slack text (e.g.
// GitHub comment bodies) passes through unchanged since the markup patterns
// simply don't match.
func (r *SlackNameResolver) CleanText(ctx context.Context, text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = slackMentionRe.ReplaceAllStringFunc(text, func(match string) string {
		// Prefer an inline label (<@U123|name>) when Slack provided one.
		if _, label, ok := strings.Cut(match, "|"); ok {
			if label = strings.TrimSpace(strings.TrimRight(label, ">")); label != "" {
				return "@" + label
			}
		}
		sub := slackMentionRe.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		if name := r.UserName(ctx, sub[1]); name != "" {
			return "@" + name
		}
		return "@user"
	})
	text = slackLinkRe.ReplaceAllString(text, "$2")
	text = slackBareLinkRe.ReplaceAllString(text, "$1")
	return strings.TrimSpace(text)
}

// lookup returns a cached name when present and not past its TTL. The bool is
// false on a miss so the caller knows to hit the API; a cached negative ("")
// still counts as a hit.
func (r *SlackNameResolver) lookup(cache map[string]nameCacheEntry, id string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := cache[id]
	if !ok {
		return "", false
	}
	if r.ttl > 0 && time.Since(entry.at) > r.ttl {
		delete(cache, id)
		return "", false
	}
	return entry.name, true
}

func (r *SlackNameResolver) store(cache map[string]nameCacheEntry, id, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cache[id] = nameCacheEntry{name: name, at: time.Now()}
}
