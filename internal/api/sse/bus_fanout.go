package sse

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// subscriber pairs a delivery channel with an optional per-connection filter.
// A nil filter means the subscriber receives all events for its key (back-compat).
type subscriber struct {
	ch     chan Record
	filter func(Record) bool
}

// BusFanout replaces the in-process Broadcaster with a bus-backed fan-out
// for the BFF tier. It subscribes to brood and activity subjects on the
// bus and delivers events to local SSE subscribers. Unlike Broadcaster,
// there is no local Notify — all events arrive via the bus.
type BusFanout struct {
	conn *bus.Conn

	mu          sync.RWMutex
	subscribers map[string]map[*subscriber]struct{}

	Logger *slog.Logger
}

// NewBusFanout creates a BusFanout. Call Start to begin receiving events.
func NewBusFanout(conn *bus.Conn) *BusFanout {
	return &BusFanout{
		conn:        conn,
		subscribers: make(map[string]map[*subscriber]struct{}),
	}
}

// Start subscribes to the bus subjects and begins fanning out events to
// local subscribers. It blocks until ctx is cancelled.
func (bf *BusFanout) Start(ctx context.Context) error {
	// Subscribe to brood events.
	broodSub, err := bf.conn.SubscribeData(bus.BroodWildcard, func(data []byte) {
		bf.deliverBusEvent("brood", data)
	})
	if err != nil {
		return err
	}
	defer func() { _ = broodSub.Unsubscribe() }()

	// Subscribe to activity events on the hierarchical subject family.
	// Use Subscribe (with *nats.Msg) so we can read the subject tokens
	// to derive the routing key, rather than parsing from the payload.
	activitySub, err := bf.conn.Subscribe(bus.ActivityWildcard, func(msg *nats.Msg) {
		bf.deliverActivityEvent(msg.Subject, msg.Data)
	})
	if err != nil {
		return err
	}
	defer func() { _ = activitySub.Unsubscribe() }()

	logger := bf.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("bus fanout started", "subjects", "events.brood.> + activity.>")
	<-ctx.Done()
	logger.Info("bus fanout stopped")
	return nil
}

// SubscribeFiltered returns a buffered channel that receives events for the
// given key, but only those for which the filter function returns true.
// A nil filter receives all events (identical to Subscribe).
//
// add-activity-persistence wires this with a func(Record) bool closure built
// from Authorizer.CanReadActivityEvent, extracting namespace and controller
// from each Record. Activity events routed onto bus.EventsActivity must expose
// namespace + controller on each Record and must deliver global (empty-controller)
// events to a global subscriber set without being dropped by deliverBusEvent.
func (bf *BusFanout) SubscribeFiltered(key string, filter func(Record) bool) <-chan Record {
	ch := make(chan Record, 64)
	s := &subscriber{ch: ch, filter: filter}
	bf.mu.Lock()
	defer bf.mu.Unlock()
	if bf.subscribers[key] == nil {
		bf.subscribers[key] = make(map[*subscriber]struct{})
	}
	bf.subscribers[key][s] = struct{}{}
	return ch
}

// Subscribe returns a buffered channel that receives events for the given key.
// Key format is "namespace/controllerName". Use "*" for brood-wide events.
func (bf *BusFanout) Subscribe(key string) <-chan Record {
	return bf.SubscribeFiltered(key, nil)
}

// SubscribeAll returns a channel for brood-wide events.
func (bf *BusFanout) SubscribeAll() <-chan Record {
	return bf.SubscribeFiltered("*", nil)
}

// Unsubscribe removes a subscription. Safe to call multiple times.
// The ch argument is compared by channel identity (the underlying chan Record).
func (bf *BusFanout) Unsubscribe(key string, ch <-chan Record) {
	bf.mu.Lock()
	defer bf.mu.Unlock()
	subs := bf.subscribers[key]
	if subs == nil {
		return
	}
	for s := range subs {
		if s.ch == ch {
			delete(subs, s)
			close(s.ch)
			break
		}
	}
	if len(subs) == 0 {
		delete(bf.subscribers, key)
	}
}

// deliverBusEvent parses a brood-format bus event and delivers it to subscribers.
// Routing key format: cluster/ns/name (3-token).
func (bf *BusFanout) deliverBusEvent(source string, data []byte) {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		logger := bf.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("unparsable bus event", "source", source, "error", err)
		return
	}

	event, _ := raw["event"].(string)
	name, _ := raw["name"].(string)
	namespace, _ := raw["namespace"].(string)
	cluster, _ := raw["cluster"].(string)

	if namespace == "" || name == "" {
		return
	}
	if cluster == "" {
		// Every publisher stamps cluster (greenfield, no legacy shape). A
		// missing field is a malformed event — drop it rather than guess a
		// cluster and misroute per-controller subscribers.
		logger := bf.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("bus event missing cluster field, dropping", "source", source, "event", event)
		return
	}

	record := Record{
		Event: event,
		Data:  raw,
	}

	key := cluster + "/" + namespace + "/" + name

	bf.mu.RLock()
	defer bf.mu.RUnlock()

	bf.deliver(key, record)
	if key != "*" {
		bf.deliver("*", record)
	}
}

// deliverActivityEvent parses a full Event JSON from an activity.* subject
// and derives the routing key from the subject tokens.
// Subject format: activity.<namespace>.<controller> or activity._global.
func (bf *BusFanout) deliverActivityEvent(subject string, data []byte) {
	// Parse as full Event JSON.
	var e activity.Event
	if err := json.Unmarshal(data, &e); err != nil {
		logger := bf.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("unparsable activity event", "subject", subject, "error", err)
		return
	}

	// Build the record from the parsed event.
	record := Record{
		Event: e.Type,
		Data:  e,
	}

	// Derive routing key from subject tokens: activity.<cluster>.<ns>.<ctrl>
	// or activity.<cluster>._global (global key).
	key := subjectToKey(subject)
	if key == "" {
		return // malformed/legacy subject: drop, do not fan out
	}

	bf.mu.RLock()
	defer bf.mu.RUnlock()

	bf.deliver(key, record)
	if key != "*" {
		bf.deliver("*", record)
	}
}

// subjectToKey converts an activity subject to a routing key.
// activity.<cluster>.<ns>.<ctrl> → "cluster/ns/ctrl"
// activity.<cluster>._global     → "_global"
func subjectToKey(subject string) string {
	// Strip the "activity." prefix.
	rest := strings.TrimPrefix(subject, "activity.")
	if rest == "" {
		return ""
	}
	parts := strings.SplitN(rest, ".", 3) // [cluster, ns|_global, ctrl]
	if len(parts) >= 2 && parts[1] == "_global" {
		return "_global"
	}
	if len(parts) != 3 {
		return "" // malformed → dropped by deliver (no subscribers on "")
	}
	return parts[0] + "/" + parts[1] + "/" + parts[2]
}

func (bf *BusFanout) deliver(key string, record Record) {
	for s := range bf.subscribers[key] {
		if s.filter != nil && !s.filter(record) {
			continue
		}
		select {
		case s.ch <- record:
		default:
			logger := bf.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Warn("dropping event, buffer full", "event", record.Event, "key", key)
		}
	}
}
