package monitor

import (
	"os"
	"strings"
)

// DefaultTriggerEmoji is the Slack reaction shortname (no colons) that
// signals "Claude should handle this thread." Override at runtime via
// FLOW_SLACK_TRIGGER_EMOJI. The default matches the convention the user
// settled on during design — a custom workspace emoji combining the
// flow + claude brand.
const DefaultTriggerEmoji = "claude"

// TriggerEmoji resolves the first configured trigger emoji shortname. It
// exists for callers that only care about one canonical value (logging,
// brief text). Most production code should use TriggerEmojis() instead so
// multi-provider triggers ("claude" → Claude, "codex" → Codex) work.
func TriggerEmoji() string {
	emojis := TriggerEmojis()
	return emojis[0]
}

// TriggerEmojis resolves the full set of configured trigger emoji
// shortnames. The env var accepts a comma- or whitespace-separated list
// (with optional surrounding colons), so all of these are equivalent:
//
//	FLOW_SLACK_TRIGGER_EMOJI=claude
//	FLOW_SLACK_TRIGGER_EMOJI=":claude:,:codex:"
//	FLOW_SLACK_TRIGGER_EMOJI="claude codex"
//
// Empty / whitespace env values fall through to [DefaultTriggerEmoji].
// Duplicates are de-duped (case-insensitive). Order is preserved so the
// first entry stays stable as the "primary" emoji.
func TriggerEmojis() []string {
	raw := strings.TrimSpace(os.Getenv("FLOW_SLACK_TRIGGER_EMOJI"))
	if raw == "" {
		return []string{DefaultTriggerEmoji}
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		e := strings.Trim(strings.TrimSpace(p), ":")
		if e == "" {
			continue
		}
		k := strings.ToLower(e)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, e)
	}
	if len(out) == 0 {
		return []string{DefaultTriggerEmoji}
	}
	return out
}

// ProviderForEmoji maps a Slack trigger emoji shortname to the agent
// provider that should service it. The emoji name is taken literally:
// ":codex:" → "codex", ":claude:" → "claude". Anything else (legacy
// custom workspace emojis like ":flow-claude:" or ":robot:") falls back
// to "claude" so existing installations keep working.
func ProviderForEmoji(emoji string) string {
	if strings.EqualFold(strings.TrimSpace(emoji), "codex") {
		return "codex"
	}
	return "claude"
}

// SelfUserIDs returns the Slack user IDs that count as "the user" for
// reaction-consent purposes. Only reactions added by one of these IDs
// are treated as trigger signals — reactions from coworkers are noise.
//
// Configured via FLOW_SLACK_SELF_USER_IDS (comma/space-separated for
// multi-workspace setups) with fallbacks to the single-id env vars from
// the existing Slack code. Returns an empty slice when unset, in which
// case DecideReaction will refuse to trigger — better to require explicit
// configuration than to fan out replies for every workspace member's
// reaction.
func SelfUserIDs() []string {
	raw := firstNonEmpty(
		os.Getenv("FLOW_SLACK_SELF_USER_IDS"),
		os.Getenv("FLOW_SLACK_SELF_USER_ID"),
		os.Getenv("FLOW_SLACK_USER_ID"),
		os.Getenv("SLACK_USER_ID"),
	)
	if raw == "" {
		return nil
	}
	out := []string{}
	seen := map[string]bool{}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	}) {
		id := strings.TrimSpace(part)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// ThreadKey returns the partition key flow uses to find or create a task
// for a Slack thread. We include the channel ID because two different
// channels can technically have messages with the same ts (Slack ts is
// only unique-per-channel), so "thread_ts" alone could collide.
//
// The corresponding flow task tag is "slack-thread:<key>" — see the
// integration layer for the tag-vs-lookup helpers.
func ThreadKey(channel, threadTS string) string {
	channel = strings.TrimSpace(channel)
	threadTS = strings.TrimSpace(threadTS)
	if channel == "" || threadTS == "" {
		return ""
	}
	return channel + ":" + threadTS
}

// ReactionDecision is the output of DecideReaction: a small struct the
// integration layer turns into side effects (flow add task, inbox append,
// reaction.add for status, etc.). Trigger=false means the event is not
// a consenting trigger from "us" — drop it.
type ReactionDecision struct {
	Trigger   bool
	ThreadKey string
	Channel   string
	ThreadTS  string
	ItemTS    string
	Reactor   string
	Reaction  string
	Event     InboundEvent
}

// DecideReaction classifies an InboundEvent. Returns Trigger=false unless
// ALL of these hold:
//
//   - Kind == "reaction_added"
//   - Reaction (case-insensitive) matches one of triggerEmojis
//   - Reactor (UserID) is in selfUserIDs
//   - Channel + ThreadTS are present (so ThreadKey is meaningful)
//
// The integration layer then uses ThreadKey to look up an existing task
// or create a new one, and appends the event to that task's inbox. The
// matched emoji is echoed back via [ReactionDecision.Reaction] so the
// caller can pick an agent provider from it (see [ProviderForEmoji]).
//
// Pass an empty selfUserIDs to short-circuit all reactions to non-trigger
// — useful for tests and as a safety net when SelfUserIDs() resolves to
// empty (operator hasn't configured their Slack user id).
func DecideReaction(ev InboundEvent, triggerEmojis []string, selfUserIDs []string) ReactionDecision {
	if ev.Kind != "reaction_added" {
		return ReactionDecision{}
	}
	want := strings.TrimSpace(ev.Reaction)
	matched := false
	for _, e := range triggerEmojis {
		if strings.EqualFold(want, strings.TrimSpace(e)) {
			matched = true
			break
		}
	}
	if !matched {
		return ReactionDecision{}
	}
	if !containsUserID(selfUserIDs, ev.UserID) {
		return ReactionDecision{}
	}
	key := ThreadKey(ev.Channel, ev.ThreadTS)
	if key == "" {
		return ReactionDecision{}
	}
	return ReactionDecision{
		Trigger:   true,
		ThreadKey: key,
		Channel:   ev.Channel,
		ThreadTS:  ev.ThreadTS,
		ItemTS:    ev.ItemTS,
		Reactor:   ev.UserID,
		Reaction:  ev.Reaction,
		Event:     ev,
	}
}

func containsUserID(haystack []string, needle string) bool {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return false
	}
	for _, h := range haystack {
		if strings.TrimSpace(h) == needle {
			return true
		}
	}
	return false
}
