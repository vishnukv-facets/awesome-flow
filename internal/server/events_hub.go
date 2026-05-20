package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"flow/internal/flowdb"
)

// eventHub is the in-process pub/sub fan-out for live events that the UI
// (or any external subscriber) wants to react to in real time. Subscribers
// connect via the /ws/events WebSocket; the hub pushes ndjson-shaped
// envelopes onto each subscriber's buffered channel and drops events for
// any subscriber that's too slow to drain.
//
// This intentionally lives in-process and uses Go channels rather than a
// real broker. flow already has exactly one server per host; there's no
// horizontal scale to plan for, and an external broker would just add an
// operational dependency without buying anything.
type eventHub struct {
	mu          sync.RWMutex
	subscribers map[*eventSubscriber]struct{}
}

type eventSubscriber struct {
	id        string
	filter    eventFilter
	send      chan eventEnvelope
	done      chan struct{}
	closeOnce sync.Once
}

// eventFilter narrows what each subscriber sees. Empty fields match
// everything. Both filters are AND'd: a subscriber that requests
// session="X" AND types=[hook_event] only sees hook events for that
// session.
type eventFilter struct {
	SessionID string
	TaskSlug  string
	Types     []string // event envelope Type values to include
}

// eventEnvelope is the wire shape pushed to subscribers. Type is the
// stable enum the UI switches on; SessionID/TaskSlug let clients filter
// efficiently without parsing Data. Data is whatever payload the
// publisher chose to attach (typically the hook response or a
// liveness/reconciler snapshot).
type eventEnvelope struct {
	Type        string            `json:"type"`
	Timestamp   string            `json:"timestamp"`
	SessionID   string            `json:"session_id,omitempty"`
	TaskSlug    string            `json:"task_slug,omitempty"`
	Seq         int64             `json:"seq,omitempty"`
	Data        json.RawMessage   `json:"data,omitempty"`
	HookEvent   *eventHookData    `json:"hook,omitempty"`
	Liveness    *eventLiveness    `json:"liveness,omitempty"`
	Runtime     *eventRuntime     `json:"runtime,omitempty"`
	HookHealth  *uiHookHealth     `json:"hook_health,omitempty"`
}

type eventHookData struct {
	Provider     string `json:"provider,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Event        string `json:"event,omitempty"`
	SubagentID   string `json:"subagent_id,omitempty"`
	HookVersion  int    `json:"hook_version,omitempty"`
	HookOwned    bool   `json:"hook_owned,omitempty"`
	HookOutdated bool   `json:"hook_outdated,omitempty"`
}

type eventLiveness struct {
	Provider string `json:"provider"`
	Slug     string `json:"slug,omitempty"`
	Status   string `json:"status"` // "alive" | "dead"
	Reason   string `json:"reason,omitempty"`
}

type eventRuntime struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`
	Kind     string `json:"kind,omitempty"`
}

func newEventHub() *eventHub {
	return &eventHub{
		subscribers: map[*eventSubscriber]struct{}{},
	}
}

func (h *eventHub) subscribe(filter eventFilter) *eventSubscriber {
	sub := &eventSubscriber{
		id:     subscriberID(),
		filter: filter,
		// Buffered channel deliberately small: live events are bursty and
		// we'd rather drop than block the publisher. 64 covers a typical
		// PreToolUse → PostToolUse storm without grief.
		send: make(chan eventEnvelope, 64),
		done: make(chan struct{}),
	}
	h.mu.Lock()
	h.subscribers[sub] = struct{}{}
	h.mu.Unlock()
	return sub
}

func (h *eventHub) unsubscribe(sub *eventSubscriber) {
	sub.closeOnce.Do(func() {
		close(sub.done)
		close(sub.send)
	})
	h.mu.Lock()
	delete(h.subscribers, sub)
	h.mu.Unlock()
}

func (h *eventHub) publish(env eventEnvelope) {
	if env.Timestamp == "" {
		env.Timestamp = flowdb.NowISO()
	}
	h.mu.RLock()
	subs := make([]*eventSubscriber, 0, len(h.subscribers))
	for s := range h.subscribers {
		subs = append(subs, s)
	}
	h.mu.RUnlock()
	for _, sub := range subs {
		if !sub.filter.matches(env) {
			continue
		}
		select {
		case <-sub.done:
		case sub.send <- env:
		default:
			// Subscriber is back-pressured. Drop rather than block so
			// that one slow client can't stall ingestAgentHook for
			// everyone else. The UI should reconnect and re-sync from
			// the DB if it notices a gap.
		}
	}
}

func (f eventFilter) matches(env eventEnvelope) bool {
	if f.SessionID != "" && env.SessionID != f.SessionID {
		return false
	}
	if f.TaskSlug != "" && env.TaskSlug != f.TaskSlug {
		return false
	}
	if len(f.Types) == 0 {
		return true
	}
	for _, t := range f.Types {
		if t == env.Type {
			return true
		}
	}
	return false
}

// publishHookEvent fans out a hook-ingest response to the event hub so
// subscribers see the same lifecycle the DB just recorded. It is safe to
// call before the hub exists (no-op). The payload parameter is reserved
// for future extension (per-event extras like tool_name) — accepted now
// so callers don't churn when extras land.
func (s *Server) publishHookEvent(resp agentHookIngestResponse, _ map[string]any) {
	if s.events == nil {
		return
	}
	s.events.publish(eventEnvelope{
		Type:      "agent_hook",
		SessionID: resp.SessionID,
		TaskSlug:  resp.Task,
		Seq:       resp.Seq,
		HookEvent: &eventHookData{
			Provider:     resp.Provider,
			Kind:         resp.Kind,
			Event:        resp.Event,
			SubagentID:   resp.SubagentID,
			HookVersion:  resp.HookVersion,
			HookOwned:    resp.HookOwned,
			HookOutdated: resp.HookOutdated,
		},
	})
}


// publishLiveness fans out a liveness reconciler observation. Slug is
// empty when no task is bound to the session.
func (s *Server) publishLiveness(provider, sessionID, slug, status, reason string) {
	if s.events == nil {
		return
	}
	s.events.publish(eventEnvelope{
		Type:      "liveness",
		SessionID: sessionID,
		TaskSlug:  slug,
		Liveness: &eventLiveness{
			Provider: provider,
			Slug:     slug,
			Status:   status,
			Reason:   reason,
		},
	})
}

// handleEventWebSocket upgrades the connection, accepts an optional
// subscribe message that narrows the filter, then pumps envelopes until
// either side closes. Newline-delimited JSON over the WS frame stream
// keeps the wire format trivial to parse with browser EventSource-style
// clients.
func (s *Server) handleEventWebSocket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.events == nil {
		http.Error(w, "events hub unavailable", http.StatusServiceUnavailable)
		return
	}
	filter := eventFilterFromQuery(r)
	conn, err := terminalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	sub := s.events.subscribe(filter)
	defer s.events.unsubscribe(sub)

	// Send an ack so the client knows the subscription took effect and
	// has the assigned subscriber id (useful in logs when debugging).
	_ = conn.WriteJSON(map[string]any{
		"type":           "subscribed",
		"subscriber_id":  sub.id,
		"server_version": s.cfg.Version,
		"hook_version":   CurrentHookVersion,
	})

	// Send-loop: drain envelopes; bail when the channel closes or the
	// peer goes away.
	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	readCh := make(chan error, 1)
	go func() {
		// Drain any incoming messages so the connection's read buffer
		// stays unblocked; we don't currently accept subscription
		// updates over the wire, but ignoring reads would let TCP
		// back-pressure block the WS heartbeat.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				readCh <- err
				return
			}
		}
	}()

	for {
		select {
		case env, ok := <-sub.send:
			if !ok {
				return
			}
			if err := conn.WriteJSON(env); err != nil {
				return
			}
		case <-pingTicker.C:
			if err := conn.WriteJSON(map[string]any{"type": "ping", "ts": flowdb.NowISO()}); err != nil {
				return
			}
		case <-readCh:
			return
		case <-sub.done:
			return
		}
	}
}

func eventFilterFromQuery(r *http.Request) eventFilter {
	q := r.URL.Query()
	filter := eventFilter{
		SessionID: strings.TrimSpace(q.Get("session_id")),
		TaskSlug:  strings.TrimSpace(q.Get("task_slug")),
	}
	if types := strings.TrimSpace(q.Get("types")); types != "" {
		for _, t := range strings.Split(types, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				filter.Types = append(filter.Types, t)
			}
		}
	}
	return filter
}

var subscriberSeq int64

func subscriberID() string {
	subscriberMu.Lock()
	subscriberSeq++
	id := subscriberSeq
	subscriberMu.Unlock()
	return "sub-" + strconvFormatInt(id)
}

var subscriberMu sync.Mutex

// strconvFormatInt is a tiny shim to avoid a strconv import bloat in this
// file; the caller's allocation pattern matters less than keeping the
// hot ingest path lean.
func strconvFormatInt(i int64) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
