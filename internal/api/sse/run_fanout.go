package sse

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// RunEventSource is the live-run subscription seam a stream handler reads.
// RunFanout is its one implementation.
type RunEventSource interface {
	Subscribe(keys []string, filter func(recordstore.RunNotification) bool) <-chan Record
	Unsubscribe(keys []string, ch <-chan Record)
}

// runSubscriber pairs a delivery channel with an optional filter. It
// records every key it registered under, so an unsubscribe with a partial
// key list still leaves no registration behind.
type runSubscriber struct {
	ch     chan Record
	filter func(recordstore.RunNotification) bool
	keys   []string
}

// RunFanout delivers live run notifications to local SSE subscribers. It
// routes a notification to one key, which is the notification's own
// cluster and tenant, so it holds no wildcard key.
type RunFanout struct {
	conn *bus.Conn

	mu          sync.RWMutex
	subscribers map[string]map[*runSubscriber]struct{}

	unmatched metric.Int64Counter

	Logger *slog.Logger
}

// RunUnmatchedMetric names the counter of notifications with no local
// subscriber. A reader on the poll path can read silence as one number.
const RunUnmatchedMetric = "bff.run_notify.unmatched"

// NewRunFanout creates a RunFanout over conn. A nil meter selects the
// process meter, which is a no-op meter in a test. Call Start to receive.
func NewRunFanout(conn *bus.Conn, meter metric.Meter) *RunFanout {
	if meter == nil {
		meter = otel.Meter("varroa-bff")
	}
	counter, _ := meter.Int64Counter(RunUnmatchedMetric,
		metric.WithDescription("Live run notifications whose key matched no local subscriber."),
	)
	return &RunFanout{
		conn:        conn,
		subscribers: make(map[string]map[*runSubscriber]struct{}),
		unmatched:   counter,
	}
}

// Start subscribes the run notification subject and fans each message out
// to local subscribers. One replica holds one subscription. Start blocks
// until ctx is cancelled.
func (rf *RunFanout) Start(ctx context.Context) error {
	sub, err := rf.conn.SubscribeData(bus.RunNotifyWildcard, rf.deliverPayload)
	if err != nil {
		return err
	}
	defer func() { _ = sub.Unsubscribe() }()

	logger := rf.logger()
	logger.Info("run fanout started", "subject", bus.RunNotifyWildcard)
	<-ctx.Done()
	logger.Info("run fanout stopped")
	return nil
}

// Subscribe registers one subscriber under every key and returns its
// channel. The channel buffers 64 records, and a full buffer drops a
// record rather than blocking the bus callback. The subscriber remembers
// its own keys, so no caller can leave a registration behind.
func (rf *RunFanout) Subscribe(keys []string, filter func(recordstore.RunNotification) bool) <-chan Record {
	ch := make(chan Record, 64)
	s := &runSubscriber{ch: ch, filter: filter, keys: append([]string(nil), keys...)}
	rf.mu.Lock()
	defer rf.mu.Unlock()
	for _, key := range keys {
		if rf.subscribers[key] == nil {
			rf.subscribers[key] = make(map[*runSubscriber]struct{})
		}
		rf.subscribers[key][s] = struct{}{}
	}
	return ch
}

// Unsubscribe removes the subscriber from every key it recorded and closes
// its channel once. The keys argument locates the subscriber; the recorded
// keys decide what it leaves, so a partial list still removes every
// registration. A second call is safe, because it finds no subscriber.
func (rf *RunFanout) Unsubscribe(keys []string, ch <-chan Record) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	var found *runSubscriber
	for _, key := range keys {
		for s := range rf.subscribers[key] {
			if s.ch == ch {
				found = s
				break
			}
		}
		if found != nil {
			break
		}
	}
	if found == nil {
		return
	}

	for _, key := range found.keys {
		subs, registered := rf.subscribers[key]
		if !registered {
			continue
		}
		delete(subs, found)
		if len(subs) == 0 {
			delete(rf.subscribers, key)
		}
	}
	close(found.ch)
}

// deliverPayload decodes one bus message and routes it. It drops a message
// the fan-out cannot decode, and one whose key is incomplete, because the
// key needs both a cluster and a tenant.
func (rf *RunFanout) deliverPayload(data []byte) {
	var n recordstore.RunNotification
	if err := json.Unmarshal(data, &n); err != nil {
		rf.logger().Warn("undecodable run notification", "error", err)
		return
	}
	if n.Cluster == "" || n.Tenant == "" {
		rf.logger().Warn("run notification with an incomplete key",
			"cluster", n.Cluster, "tenant", n.Tenant, "kind", n.Kind)
		return
	}
	rf.deliver(n)
}

// deliver writes n to the subscribers of its own key. It raises the
// unmatched counter when no subscriber holds that key.
func (rf *RunFanout) deliver(n recordstore.RunNotification) {
	key := n.Cluster + "/" + n.Tenant

	rf.mu.RLock()
	defer rf.mu.RUnlock()

	subs := rf.subscribers[key]
	if len(subs) == 0 {
		if rf.unmatched != nil {
			rf.unmatched.Add(context.Background(), 1)
		}
		return
	}

	record := Record{Event: n.Kind, Data: n}
	for s := range subs {
		if s.filter != nil && !s.filter(n) {
			continue
		}
		select {
		case s.ch <- record:
		default:
			rf.logger().Warn("dropping run notification, buffer full", "key", key, "kind", n.Kind)
		}
	}
}

// logger answers the fan-out's logger, or the process logger when unset.
func (rf *RunFanout) logger() *slog.Logger {
	if rf.Logger != nil {
		return rf.Logger
	}
	return slog.Default()
}

// compile-time check: a RunFanout satisfies the seam a handler reads.
var _ RunEventSource = (*RunFanout)(nil)
