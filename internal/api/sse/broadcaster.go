package sse

import (
	"log/slog"
	"sync"
)

// Record is a single SSE event delivered to subscribers.
type Record struct {
	Event string      // SSE event type, e.g. "heartbeat", "snapshot", "connected"
	Data  interface{} // JSON-marshaled payload
}

// ActivityNotifier receives connect/disconnect events for the activity feed.
type ActivityNotifier interface {
	NotifyConnect(name, namespace, version string)
	NotifyDisconnect(name, namespace string)
}

// EventSource is the interface satisfied by both Broadcaster (in-process) and
// BusFanout (bus-backed). Handlers use this interface so the BFF can wire
// in a BusFanout without the monolith's in-process Broadcaster.
type EventSource interface {
	Subscribe(key string) <-chan Record
	SubscribeAll() <-chan Record
	Unsubscribe(key string, ch <-chan Record)
}

// Broadcaster fans out mite events to SSE subscribers.
type Broadcaster struct {
	mu          sync.RWMutex
	subscribers map[string]map[chan Record]struct{}
	activity    ActivityNotifier
	Logger      *slog.Logger
}

// NewBroadcaster creates a new Broadcaster.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		subscribers: make(map[string]map[chan Record]struct{}),
	}
}

// SetActivityNotifier sets the activity store for connect/disconnect events.
func (b *Broadcaster) SetActivityNotifier(a ActivityNotifier) {
	b.activity = a
}

// Subscribe returns a buffered channel that receives events for the given key.
// Key format is "namespace/controllerName". Use "*" for brood-wide events.
// The caller must call Unsubscribe when done.
func (b *Broadcaster) Subscribe(key string) <-chan Record {
	ch := make(chan Record, 64)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subscribers[key] == nil {
		b.subscribers[key] = make(map[chan Record]struct{})
	}
	b.subscribers[key][ch] = struct{}{}
	return ch
}

// SubscribeAll returns a buffered channel that receives all brood-wide events.
func (b *Broadcaster) SubscribeAll() <-chan Record {
	return b.Subscribe("*")
}

// Unsubscribe removes a subscription. Safe to call multiple times.
func (b *Broadcaster) Unsubscribe(key string, ch <-chan Record) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// We must convert to chan Record for map lookup; the underlying type is the same.
	subs := b.subscribers[key]
	if subs == nil {
		return
	}
	// Find the channel by iterating (channels are comparable by identity).
	for k := range subs {
		if k == ch {
			delete(subs, k)
			close(k)
			break
		}
	}
	if len(subs) == 0 {
		delete(b.subscribers, key)
	}
}

// Notify sends a record to all subscribers for the given key AND brood-wide
// subscribers. Non-blocking: drops records if a subscriber's buffer is full.
func (b *Broadcaster) Notify(key string, record Record) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	b.deliver(key, record)
	if key != "*" {
		b.deliver("*", record)
	}

	// Forward connect/disconnect to activity store.
	if b.activity != nil {
		switch record.Event {
		case "connected":
			if data, ok := record.Data.(map[string]interface{}); ok {
				name, _ := data["name"].(string)
				ns, _ := data["namespace"].(string)
				version, _ := data["version"].(string)
				b.activity.NotifyConnect(name, ns, version)
			}
		case "disconnected":
			if data, ok := record.Data.(map[string]interface{}); ok {
				name, _ := data["name"].(string)
				ns, _ := data["namespace"].(string)
				b.activity.NotifyDisconnect(name, ns)
			}
		}
	}
}

func (b *Broadcaster) deliver(key string, record Record) {
	for ch := range b.subscribers[key] {
		select {
		case ch <- record:
		default:
			logger := b.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Warn("dropping event, buffer full", "event", record.Event, "key", key)
		}
	}
}
