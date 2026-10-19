package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// ActivityRecorderConsumerName is the durable pull consumer name every
// recorder replica shares over the varroa_activity stream.
const ActivityRecorderConsumerName = "activity-recorder"

// activityFetchBatch is the pull consumer's fetch batch size. C1 states
// no batch size for its own "recorder" consumer; this change chooses
// 256, because an activity row is far smaller than a build event.
const activityFetchBatch = 256

// activityRecorderConsumerConfig returns D3's consumer shape: explicit
// acknowledgement, a 30s ack wait, unlimited delivery, and DeliverAll,
// so a fresh install imports the stream's full retained history.
func activityRecorderConsumerConfig() *nats.ConsumerConfig {
	return &nats.ConsumerConfig{
		Durable:       ActivityRecorderConsumerName,
		DeliverPolicy: nats.DeliverAllPolicy,
		AckPolicy:     nats.AckExplicitPolicy,
		AckWait:       ackWait,
		MaxDeliver:    -1,
	}
}

// RunActivityIngestConsumer drains the varroa_activity stream through
// the durable "activity-recorder" consumer until ctx is cancelled,
// applying D3's disposition table to every message.
func RunActivityIngestConsumer(ctx context.Context, conn *bus.Conn, ingester *ActivityIngester, metrics *ActivityMetrics, retentionActivity time.Duration, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if err := conn.EnsureConsumer(bus.ActivityStreamName, ActivityRecorderConsumerName, activityRecorderConsumerConfig()); err != nil {
		return err
	}
	sub, err := conn.PullSubscribe(bus.ActivityWildcard, bus.ActivityStreamName, ActivityRecorderConsumerName)
	if err != nil {
		return err
	}
	defer func() { _ = sub.Unsubscribe() }()

	for {
		if ctx.Err() != nil {
			return nil
		}
		msgs, err := conn.DrainConsumer(sub, activityFetchBatch)
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) {
				continue
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
				continue
			}
		}
		for _, msg := range msgs {
			handleActivityMessage(ctx, ingester, metrics, retentionActivity, logger, msg)
		}
	}
}

// handleActivityMessage applies D3's disposition table to one delivered
// message. It never calls a bare Nak and never terminates a decodable
// event.
func handleActivityMessage(ctx context.Context, ingester *ActivityIngester, metrics *ActivityMetrics, retentionActivity time.Duration, logger *slog.Logger, msg *nats.Msg) {
	var e activity.Event
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		metrics.addUnknown(ctx)
		logger.Warn("undecodable activity event acknowledged", "error", err)
		_ = msg.Ack()
		return
	}

	// The recorder decodes every message from its payload and never
	// from its subject, so a legacy pre-cutover message
	// (activity.<namespace>.<controller> or activity._global) is stored
	// with whatever cluster its payload carries, empty or not, and is
	// never re-attributed to the reading cluster.
	if e.Timestamp.IsZero() {
		// The payload carries no timestamp, or an unparseable one that
		// decoded to the zero value. The JetStream message timestamp is
		// closer to the truth than the recorder's own clock and is
		// stable across a redelivery, so the computed key is stable too.
		if meta, merr := msg.Metadata(); merr == nil && meta != nil {
			e.Timestamp = meta.Timestamp
		}
	}
	e.Normalize()

	if time.Since(e.Timestamp) > retentionActivity {
		metrics.addExpired(ctx)
		_ = msg.Ack()
		return
	}

	key := activity.EventKey(e)
	inserted, err := ingester.Insert(ctx, e, key)
	switch {
	case err == nil:
		if !inserted {
			metrics.addDuplicate(ctx)
		}
		_ = msg.Ack()
	case isNoActivityPartition(err):
		if perr := ensureActivityPartitionForTime(ctx, ingester.pool, e.Timestamp); perr != nil {
			logger.Warn("create activity partition failed; retrying", "error", perr)
			_ = msg.NakWithDelay(backoffDelay(msg))
			return
		}
		inserted, err = ingester.Insert(ctx, e, key)
		if err != nil {
			logger.Warn("retryable activity ingest failure after partition create", "error", err)
			_ = msg.NakWithDelay(backoffDelay(msg))
			return
		}
		if !inserted {
			metrics.addDuplicate(ctx)
		}
		_ = msg.Ack()
	default:
		logger.Warn("retryable activity ingest failure", "error", err)
		_ = msg.NakWithDelay(backoffDelay(msg))
	}
}

// ensureActivityPartitionForTime creates the "activity" partition
// covering ts's month, using the same CREATE TABLE IF NOT EXISTS form
// the maintenance round uses, which makes on-demand creation safe under
// concurrency.
func ensureActivityPartitionForTime(ctx context.Context, db maintDB, ts time.Time) error {
	from := time.Date(ts.UTC().Year(), ts.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	suffix := from.Format("200601")
	return createPartition(ctx, db, "activity", suffix, from, to)
}
