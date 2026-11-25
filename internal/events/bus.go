// Package events is a small in-process publish/subscribe bus. The server,
// job manager and community manager publish events; plugin event hooks and
// SSE clients (custom UIs, services) subscribe to them.
package events

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event is one published message.
type Event struct {
	ID     uint64         `json:"id"`
	Topic  string         `json:"topic"`
	Time   time.Time      `json:"time"`
	Data   map[string]any `json:"data,omitempty"`
	Source string         `json:"source,omitempty"` // plugin id that caused it, if any
	// Hops counts how many event-hook jobs led to this event; used to stop
	// hook → job → event → hook loops.
	Hops int `json:"hops,omitempty"`
}

// MaxHops is the longest chain of hook-triggered jobs an event may cause.
const MaxHops = 3

// Match reports whether topic matches pattern: an exact topic, "*" for
// everything, or a prefix ending in ".*" ("job.*" matches "job.done").
func Match(pattern, topic string) bool {
	switch {
	case pattern == "" || pattern == "*":
		return true
	case strings.HasSuffix(pattern, ".*"):
		return strings.HasPrefix(topic, pattern[:len(pattern)-1])
	default:
		return pattern == topic
	}
}

type sub struct {
	pattern string
	ch      chan Event
}

type Bus struct {
	seq  atomic.Uint64
	mu   sync.RWMutex
	subs map[*sub]struct{}
	// handlers run synchronously in publish order (used for hooks, which
	// must not be dropped like a slow SSE client's events).
	handlers []func(Event)
}

func New() *Bus { return &Bus{subs: map[*sub]struct{}{}} }

// Publish delivers an event to all subscribers. A nil bus is a no-op, so
// components can publish without checking whether events are enabled.
func (b *Bus) Publish(ev Event) {
	if b == nil {
		return
	}
	ev.ID = b.seq.Add(1)
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	b.mu.RLock()
	handlers := b.handlers
	for s := range b.subs {
		if Match(s.pattern, ev.Topic) {
			select {
			case s.ch <- ev:
			default: // slow subscriber: drop rather than block publishers
			}
		}
	}
	b.mu.RUnlock()
	for _, h := range handlers {
		h(ev)
	}
}

// Subscribe returns a channel of events matching pattern; call the returned
// func to unsubscribe.
func (b *Bus) Subscribe(pattern string) (<-chan Event, func()) {
	s := &sub{pattern: pattern, ch: make(chan Event, 128)}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s.ch, func() {
		b.mu.Lock()
		delete(b.subs, s)
		b.mu.Unlock()
	}
}

// Handle registers a function called for every event. It must not block.
func (b *Bus) Handle(f func(Event)) {
	b.mu.Lock()
	b.handlers = append(b.handlers, f)
	b.mu.Unlock()
}
