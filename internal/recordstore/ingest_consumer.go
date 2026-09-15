package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// RecorderConsumerName is the durable pull consumer name every recorder
// replica binds to, sharing one queue over the varroa_builds stream.
const RecorderConsumerName = "recorder"

// ackWait bounds how long the recorder has to decide one message's
// disposition before JetStream redelivers it.
const ackWait = 30 * time.Second

// backoffFloor and backoffCeiling bound the NakWithDelay backoff a retryable
// failure uses, doubling from the minimum toward the maximum.
const (
	backoffFloor   = time.Second
	backoffCeiling = 60 * time.Second
)

// EnsureIngestConsumer creates or updates the durable "recorder" pull
// consumer on stream, with explicit acknowledgement, AckWait 30s, and
// MaxDeliver -1. Every replica calls this with the same durable name,
// which is JetStream's queue-group equivalent for pull consumers.
func EnsureIngestConsumer(conn *bus.Conn, stream string) error {
	return conn.EnsureConsumer(stream, RecorderConsumerName, &nats.ConsumerConfig{
		Durable:    RecorderConsumerName,
		AckPolicy:  nats.AckExplicitPolicy,
		AckWait:    ackWait,
		MaxDeliver: -1,
	})
}

// RunIngestConsumer drains the varroa_builds stream through the durable
// "recorder" consumer until ctx is cancelled, applying the D4
// disposition table to every message. retentionRuns is the store's
// configured run retention: an event whose intent is gone and whose
// occurred_at predates it is terminally acknowledged rather than
// retried forever.
func RunIngestConsumer(ctx context.Context, conn *bus.Conn, ingester *Ingester, metrics *IngestMetrics, retentionRuns time.Duration, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if err := EnsureIngestConsumer(conn, bus.BuildsStreamName); err != nil {
		return err
	}
	sub, err := conn.PullSubscribe(bus.BuildsWildcard, bus.BuildsStreamName, RecorderConsumerName)
	if err != nil {
		return err
	}
	defer func() { _ = sub.Unsubscribe() }()

	for {
		if ctx.Err() != nil {
			return nil
		}
		msgs, err := conn.DrainConsumer(sub, 32)
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
			handleIngestMessage(ctx, ingester, metrics, retentionRuns, logger, msg)
		}
	}
}

// handleIngestMessage applies the D4 disposition table to one delivered
// message.
func handleIngestMessage(ctx context.Context, ingester *Ingester, metrics *IngestMetrics, retentionRuns time.Duration, logger *slog.Logger, msg *nats.Msg) {
	var env IngestEnvelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		metrics.addUnknown(ctx, UnknownCauseDecode)
		logger.Warn("undecodable build event acknowledged", "error", err)
		_ = msg.Ack()
		return
	}

	if !IsKnownKind(env.Kind) {
		metrics.addUnknown(ctx, UnknownCauseKind)
		logger.Warn("unknown build event kind acknowledged", "kind", env.Kind, "runID", env.RunID)
		_ = msg.Ack()
		return
	}

	switch {
	case env.SchemaVersion < minSchemaVersion:
		metrics.addUnknown(ctx, UnknownCauseVersionBelow)
		logger.Warn("build event schema version below minimum acknowledged",
			"schemaVersion", env.SchemaVersion, "runID", env.RunID)
		_ = msg.Ack()
		return
	case env.SchemaVersion > maxSchemaVersion:
		metrics.addUnknown(ctx, UnknownCauseVersionAbove)
		logger.Warn("build event schema version above maximum retried",
			"schemaVersion", env.SchemaVersion, "runID", env.RunID)
		_ = msg.NakWithDelay(backoffCeiling)
		return
	}

	result, err := ingester.Apply(ctx, env)
	switch {
	case err == nil:
		if result.Conflict {
			metrics.addConflict(ctx)
			logger.Warn("run.started build number conflict acknowledged",
				"runID", env.RunID, "kind", env.Kind)
		}
		if result.SupersededStart {
			metrics.addSupersededStart(ctx)
			logger.Warn("run.started for a superseded grant acknowledged without a write",
				"runID", env.RunID, "reported", result.SupersededReported, "stored", result.SupersededStored,
				"instance", env.CellInstance, "owner", result.SupersededOwner)
		}
		if result.Truncated {
			metrics.addScheduleTruncated(ctx)
			logger.Warn("job schedule envelope truncated; stored every carried row and deleted none",
				"cluster", env.Cluster, "namespace", env.Namespace, "cell", env.Cell)
		}
		if result.TriggerFailedReason != "" {
			metrics.addTriggerFailed(ctx, result.TriggerFailedReason)
			if result.TriggerFailedStored {
				logger.Info("failed build trigger recorded",
					"runID", env.RunID, "reasonCode", result.TriggerFailedReason)
			} else {
				logger.Info("failed build trigger skipped; a run already holds that run id",
					"runID", env.RunID, "reasonCode", result.TriggerFailedReason)
			}
		}
		if result.ScheduleStale {
			metrics.addScheduleStale(ctx)
			logger.Info("stale job schedule envelope acknowledged with no change",
				"cluster", env.Cluster, "namespace", env.Namespace, "cell", env.Cell)
		}
		if env.Kind == KindRunTriggerAccepted {
			metrics.addTriggerAccepted(ctx, result.TriggerAcceptedStored)
			if result.TriggerAcceptedStored {
				logger.Info("accepted build trigger recorded", "runID", env.RunID)
			} else {
				logger.Info("accepted build trigger skipped; a run already holds that run id",
					"runID", env.RunID)
			}
		}
		_ = msg.Ack()
	case errors.Is(err, ErrIntentMissing):
		if time.Since(env.OccurredAt) > retentionRuns {
			metrics.addOrphan(ctx)
			logger.Warn("build event orphaned by expired intent acknowledged",
				"runID", env.RunID, "kind", env.Kind, "occurredAt", env.OccurredAt)
			_ = msg.Ack()
			return
		}
		_ = msg.NakWithDelay(backoffDelay(msg))
	case errors.Is(err, ErrUnknownKind):
		metrics.addUnknown(ctx, UnknownCauseKind)
		logger.Warn("unknown ingest kind acknowledged", "runID", env.RunID, "kind", env.Kind, "error", err)
		_ = msg.Ack()
	case errors.Is(err, ErrMalformedEnvelope):
		metrics.addUnknown(ctx, UnknownCauseDecode)
		logger.Warn("malformed ingest envelope acknowledged", "runID", env.RunID, "kind", env.Kind, "error", err)
		_ = msg.Ack()
	default:
		logger.Warn("retryable ingest failure", "runID", env.RunID, "kind", env.Kind, "error", err)
		_ = msg.NakWithDelay(backoffDelay(msg))
	}
}

// backoffDelay computes the NakWithDelay backoff for msg, doubling from
// backoffFloor toward backoffCeiling by delivery attempt.
func backoffDelay(msg *nats.Msg) time.Duration {
	meta, err := msg.Metadata()
	if err != nil || meta == nil || meta.NumDelivered == 0 {
		return backoffFloor
	}
	delay := backoffFloor
	for i := uint64(1); i < meta.NumDelivered && delay < backoffCeiling; i++ {
		delay *= 2
	}
	if delay > backoffCeiling {
		delay = backoffCeiling
	}
	return delay
}
