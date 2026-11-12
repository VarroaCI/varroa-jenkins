package activity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// AckEnv selects ack mode when it equals "1". The chart sets it only when the
// record store runs.
const AckEnv = "VARROA_ACTIVITY_ACK"

// CloseTimeout bounds how long a shutdown waits for the ack-mode queue.
const CloseTimeout = 5 * time.Second

// Drop reasons. They are the values of the "reason" attribute on the drop counter.
const (
	dropBudget    = "budget"
	dropQueueFull = "queue_full"
	dropClosed    = "closed"
	dropShutdown  = "shutdown"
)

// ackedConfig holds the bounds of ack mode. The budget must not exceed the
// stream's duplicate window, which is two minutes by server default. A retry
// after the window could store the same event twice.
type ackedConfig struct {
	queueSize                                        int
	attemptTimeout, budget, backoffStart, backoffMax time.Duration
	onDrop                                           func(m pendingEvent, reason string)
}

func defaultAckedConfig() ackedConfig {
	return ackedConfig{
		queueSize:      4096,
		attemptTimeout: 2 * time.Second,
		budget:         2 * time.Minute,
		backoffStart:   250 * time.Millisecond,
		backoffMax:     5 * time.Second,
	}
}

// AckedOptions configures an ack-mode or env-selected Publisher. A nil Meter
// registers no drop counter.
type AckedOptions struct {
	Logger    *slog.Logger
	Meter     metric.Meter
	Component string
}

// publishFunc sends one event and waits for the stream ack. It returns an
// error when no ack arrives.
type publishFunc func(ctx context.Context, subject string, data []byte, msgID string) error

type pendingEvent struct {
	subject, label, msgID string
	data                  []byte
	deadline              time.Time
}

// ackedSender owns one queue and one goroutine. The goroutine delivers events
// in order. Callers never wait on the network.
type ackedSender struct {
	cfg       ackedConfig
	publish   publishFunc
	logger    *slog.Logger
	dropped   metric.Int64Counter
	component string

	queue  chan pendingEvent
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu        sync.RWMutex
	closed    bool
	closeOnce sync.Once

	// healthy is read and written only by the run goroutine. It starts true.
	healthy bool
}

// NewAckedPublisher returns a Publisher in ack mode. It publishes through the
// JetStream stream of the bus connection.
func NewAckedPublisher(cluster string, conn *bus.Conn, opts AckedOptions) *Publisher {
	return newAckedPublisher(cluster, conn, opts, defaultAckedConfig(), jetStreamPublish(conn))
}

// NewPublisherFromEnv returns an ack-mode Publisher when getenv(AckEnv) is
// "1". Otherwise it returns a core-mode Publisher.
func NewPublisherFromEnv(getenv func(string) string, cluster string, conn *bus.Conn, opts AckedOptions) *Publisher {
	if getenv(AckEnv) == "1" {
		return NewAckedPublisher(cluster, conn, opts)
	}
	p := NewPublisher(cluster, conn)
	p.Logger = opts.Logger
	return p
}

func jetStreamPublish(conn *bus.Conn) publishFunc {
	return func(ctx context.Context, subject string, data []byte, msgID string) error {
		_, err := conn.PublishJetStreamMsgID(ctx, subject, data, msgID)
		return err
	}
}

func newAckedPublisher(cluster string, conn *bus.Conn, opts AckedOptions, cfg ackedConfig, publish publishFunc) *Publisher {
	p := &Publisher{cluster: cluster, conn: conn, Logger: opts.Logger}
	p.acked = newAckedSender(cfg, publish, p.log(), opts.Meter, opts.Component)
	return p
}

func newAckedSender(cfg ackedConfig, publish publishFunc, logger *slog.Logger, meter metric.Meter, component string) *ackedSender {
	s := &ackedSender{
		cfg:       cfg,
		publish:   publish,
		logger:    logger,
		component: component,
		queue:     make(chan pendingEvent, cfg.queueSize),
		done:      make(chan struct{}),
		healthy:   true,
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	if meter != nil {
		c, err := meter.Int64Counter("varroa.activity.publish.dropped",
			metric.WithDescription("Activity events dropped before the stream acknowledged them."),
			metric.WithUnit("{event}"))
		if err != nil {
			logger.Warn("activity drop counter unavailable", "error", err)
		} else {
			s.dropped = c
		}
	}
	go s.run()
	return s
}

// enqueue adds an event to the queue without blocking. A full queue or a
// closed sender drops the event.
func (s *ackedSender) enqueue(subject, label string, data []byte) {
	m := pendingEvent{
		subject:  subject,
		label:    label,
		msgID:    newMsgID(),
		data:     data,
		deadline: time.Now().Add(s.cfg.budget),
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		s.drop(m, dropClosed, nil)
		return
	}
	select {
	case s.queue <- m:
	default:
		s.drop(m, dropQueueFull, nil)
	}
}

func (s *ackedSender) run() {
	defer close(s.done)
	for m := range s.queue {
		if s.ctx.Err() != nil {
			s.drop(m, dropShutdown, nil)
			continue
		}
		s.deliver(m)
	}
}

// deliver tries one event until it is acknowledged, its budget ends, or the
// sender stops.
func (s *ackedSender) deliver(m pendingEvent) {
	backoff := s.cfg.backoffStart
	var lastErr error
	for {
		// While no ack arrives, an event past its budget is dropped without a try.
		if !s.healthy && !time.Now().Before(m.deadline) {
			s.drop(m, dropBudget, lastErr)
			return
		}
		ctx, cancel := context.WithTimeout(s.ctx, s.cfg.attemptTimeout)
		err := s.publish(ctx, m.subject, m.data, m.msgID)
		cancel()
		if err == nil {
			if !s.healthy {
				s.logger.Info("activity stream acknowledges events again")
				s.healthy = true
			}
			return
		}
		lastErr = err
		if s.healthy {
			s.logger.Warn("activity stream did not acknowledge an event; retrying",
				"subject", m.subject, "type", m.label, "error", err)
			s.healthy = false
		}
		if s.ctx.Err() != nil {
			s.drop(m, dropShutdown, err)
			return
		}
		remaining := time.Until(m.deadline)
		if remaining <= 0 {
			s.drop(m, dropBudget, err)
			return
		}
		timer := time.NewTimer(min(backoff, remaining))
		select {
		case <-timer.C:
		case <-s.ctx.Done():
			timer.Stop()
			s.drop(m, dropShutdown, err)
			return
		}
		backoff = min(backoff*2, s.cfg.backoffMax)
	}
}

func (s *ackedSender) drop(m pendingEvent, reason string, err error) {
	s.logger.Error("activity event dropped",
		"reason", reason, "subject", m.subject, "type", m.label,
		"error", err, "event", string(m.data))
	if s.dropped != nil {
		s.dropped.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("component", s.component),
			attribute.String("reason", reason)))
	}
	if s.cfg.onDrop != nil {
		s.cfg.onDrop(m, reason)
	}
}

// close stops new events, then waits for the queue to drain. When timeout
// ends first, it cancels the in-flight tries, and the remaining events drop
// with reason shutdown.
func (s *ackedSender) close(timeout time.Duration) {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.queue)
		s.mu.Unlock()
	})
	timer := time.AfterFunc(timeout, s.cancel)
	<-s.done
	timer.Stop()
	s.cancel()
}

// newMsgID returns a random Nats-Msg-Id for one event. Two events with equal
// subject and payload keep distinct IDs. Retries of one event reuse its ID, so
// a lost ack cannot store the event twice.
func newMsgID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(b[:])
}
