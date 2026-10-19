// Package triggers turns the audit feed into Team agent sessions. The consumer
// reads varroa_activity, the matcher decides which Team agent a row concerns,
// and the creator writes the session row under a lock before any model call.
package triggers

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// ConsumerName names the durable consumer on varroa_activity.
const ConsumerName = "agent-triggers"

// StreamName is the activity buffer that the recorder owns.
const StreamName = "varroa_activity"

// ErrStreamAbsent reports that varroa_activity does not exist yet.
var ErrStreamAbsent = errors.New("activity stream is absent")

// ConsumerConfig returns the durable consumer config. It delivers only new
// messages on creation, so a fresh install does not replay the buffer.
func ConsumerConfig() *nats.ConsumerConfig {
	return &nats.ConsumerConfig{
		Durable:       ConsumerName,
		FilterSubject: "activity.>",
		DeliverPolicy: nats.DeliverNewPolicy,
		AckPolicy:     nats.AckExplicitPolicy,
		AckWait:       60 * time.Second,
		MaxDeliver:    -1,
		MaxAckPending: 256,
	}
}

// Ensure creates or updates the durable consumer. The deliver policy stays
// the same on update, so the stored position is kept.
func Ensure(conn *bus.Conn) error {
	if _, err := conn.StreamInfo(StreamName); err != nil {
		return ErrStreamAbsent
	}
	return conn.EnsureConsumer(StreamName, ConsumerName, ConsumerConfig())
}

// Run pulls batches from the consumer and passes each message to handle. It
// retries Ensure every 30 seconds while the stream is absent. onBound(true) runs
// when the consumer is bound, and onBound(false) when it is lost, so a caller's
// readiness follows the binding both ways. Run returns when ctx ends.
func Run(ctx context.Context, conn *bus.Conn, logger *slog.Logger, onBound func(bound bool), handle func(ctx context.Context, msg *nats.Msg)) error {
	if logger == nil {
		logger = slog.Default()
	}
	var sub *nats.Subscription
	for {
		if sub == nil {
			if err := Ensure(conn); err != nil {
				onBound(false)
				logger.Warn("trigger consumer: waiting for the activity stream", "error", err.Error())
				if !sleep(ctx, 30*time.Second) {
					return nil
				}
				continue
			}
			s, err := conn.PullSubscribe("activity.>", StreamName, ConsumerName)
			if err != nil {
				logger.Warn("trigger consumer: subscribe", "error", err.Error())
				if !sleep(ctx, 30*time.Second) {
					return nil
				}
				continue
			}
			sub = s
			onBound(true)
		}
		if ctx.Err() != nil {
			return nil //nolint:nilerr // the context ended; the loop is done
		}
		msgs, err := sub.Fetch(16, nats.MaxWait(5*time.Second))
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) {
				continue
			}
			logger.Warn("trigger consumer: fetch", "error", err.Error())
			onBound(false)
			sub = nil
			continue
		}
		for _, m := range msgs {
			handle(ctx, m)
		}
	}
}

// sleep waits d or until ctx ends. It reports false when ctx ended.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
