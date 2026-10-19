package triggers

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/loop"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// retryDelay is how long a message waits before a redelivery when the worker
// pool is full or the store is not ready.
const retryDelay = 60 * time.Second

// Message is the acknowledgement surface of one consumed activity row.
type Message interface {
	Ack() error
	NakWithDelay(delay time.Duration) error
}

// Starter reserves a worker slot and runs a session on it.
type Starter interface {
	TryReserve() bool
	Release()
	Start(sess agent.Session, trigger loop.Trigger)
}

// SessionCreator writes the session row of one match.
type SessionCreator interface {
	Create(ctx context.Context, m Match) (agent.Session, CreateOutcome, error)
}

// Handoff takes one activity row from the consumer to a session. It acks a row
// that concerns no Team agent, and naks a row that cannot start yet, so the
// broker redelivers it after retryDelay.
type Handoff struct {
	Matcher *Matcher
	Creator SessionCreator
	Pool    Starter
	// Publish sends a content-free session notice on the bus.
	Publish func(subject string, data []byte) error
	Cluster string
	Now     func() time.Time
	// OnOutcome counts each trigger by its outcome. It may be nil.
	OnOutcome func(trigger, outcome string)
	Logger    *slog.Logger
}

// Handle processes one message.
func (h *Handoff) Handle(ctx context.Context, msg Message, data []byte) {
	logger := h.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var e activity.Event
	if err := json.Unmarshal(data, &e); err != nil {
		_ = msg.Ack()
		return
	}
	m, outcome := h.Matcher.Match(e, h.Now())
	if outcome == OutcomeAck {
		_ = msg.Ack()
		return
	}
	if !h.Pool.TryReserve() {
		h.count(m.Trigger, "pool_full")
		_ = msg.NakWithDelay(retryDelay)
		return
	}
	sess, result, err := h.Creator.Create(ctx, m)
	if err != nil {
		h.Pool.Release()
		h.count(m.Trigger, "error")
		logger.Warn("team agent create", "trigger", m.Trigger, "error", err.Error())
		_ = msg.NakWithDelay(retryDelay)
		return
	}
	h.count(m.Trigger, string(result))
	if result == Capped {
		h.Pool.Release()
		_ = msg.NakWithDelay(retryDelay)
		return
	}
	_ = msg.Ack()
	if result == Created || result == Skipped {
		h.notify(sess)
	}
	if result == Created {
		h.Pool.Start(sess, loop.TriggerStart)
		return
	}
	h.Pool.Release()
}

// notify publishes the content-free notice of a new row.
func (h *Handoff) notify(s agent.Session) {
	if h.Publish == nil {
		return
	}
	body, err := json.Marshal(map[string]any{
		"session_id": s.ID.String(),
		"kind":       string(s.Kind),
		"state":      string(s.State),
		"seq":        0,
	})
	if err != nil {
		return
	}
	_ = h.Publish(bus.AgentSessionSubject(h.Cluster, s.Team), body)
}

func (h *Handoff) count(trigger, outcome string) {
	if h.OnOutcome != nil {
		h.OnOutcome(trigger, outcome)
	}
}
