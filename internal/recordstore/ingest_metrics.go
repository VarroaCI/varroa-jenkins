package recordstore

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// Unknown-kind causes recorder_ingest_unknown_total labels its "cause"
// attribute with.
const (
	UnknownCauseDecode       = "decode"
	UnknownCauseKind         = "kind"
	UnknownCauseVersionBelow = "schema_version_below_minimum"
	UnknownCauseVersionAbove = "schema_version_above_maximum"
)

// IngestMetrics holds the recorder's ingest instruments on the
// "varroa-recorder" meter: the counters the disposition table drives,
// and two observable gauges reporting the consumer's backlog.
type IngestMetrics struct {
	unknown           metric.Int64Counter
	conflict          metric.Int64Counter
	orphan            metric.Int64Counter
	scheduleTruncated metric.Int64Counter
	scheduleStale     metric.Int64Counter
	inventoryAhead    metric.Int64Counter
	triggerFailed     metric.Int64Counter
	triggerAccepted   metric.Int64Counter
	commandRefused    metric.Int64Counter
	supersededStart   metric.Int64Counter
	numberMismatch    metric.Int64Counter
	backlogMsgs       metric.Int64ObservableGauge
	backlogBytes      metric.Int64ObservableGauge
}

// NewIngestMetrics registers the ingest instruments. conn reads the
// consumer's pending count and the stream's byte count for the two
// backlog gauges; a lookup failure during one collection skips that
// collection rather than erroring the whole callback.
func NewIngestMetrics(mp metric.MeterProvider, conn *bus.Conn, streamName, consumerName string) (*IngestMetrics, error) {
	meter := mp.Meter("varroa-recorder")
	m := &IngestMetrics{}

	var err error
	m.unknown, err = meter.Int64Counter("recorder_ingest_unknown_total",
		metric.WithDescription("Ingest events acknowledged or retried because their kind or schema version is unknown"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_ingest_unknown_total: %w", err)
	}

	m.conflict, err = meter.Int64Counter("recorder_ingest_conflict_total",
		metric.WithDescription("run.started writes that hit the build-number UNIQUE constraint"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_ingest_conflict_total: %w", err)
	}

	m.supersededStart, err = meter.Int64Counter("recorder_ingest_superseded_start_total",
		metric.WithDescription("run.started events acknowledged because they reported a build number below the intent's stored number from an instance other than the intent's owner"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_ingest_superseded_start_total: %w", err)
	}

	m.numberMismatch, err = meter.Int64Counter("recorder_ingest_number_mismatch_total",
		metric.WithDescription("run.started events whose build number differs from the grant the run applied"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_ingest_number_mismatch_total: %w", err)
	}

	m.orphan, err = meter.Int64Counter("recorder_ingest_orphan_total",
		metric.WithDescription("Ingest events acknowledged because their intent expired"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_ingest_orphan_total: %w", err)
	}

	m.scheduleTruncated, err = meter.Int64Counter("recorder_schedules_truncated_total",
		metric.WithDescription("job.schedules envelopes stored without deleting any row, because the reported set was truncated"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_schedules_truncated_total: %w", err)
	}

	m.scheduleStale, err = meter.Int64Counter("recorder_schedules_stale_total",
		metric.WithDescription("job.schedules envelopes acknowledged with no change, because their time was not newer than the cell's stored watermark"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_schedules_stale_total: %w", err)
	}

	m.inventoryAhead, err = meter.Int64Counter("recorder_job_inventory_ahead_total",
		metric.WithDescription("job.schedules and job.branches envelopes refused because their taken time was more than five minutes ahead of the recorder clock"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_job_inventory_ahead_total: %w", err)
	}

	m.triggerFailed, err = meter.Int64Counter("recorder_ingest_trigger_failed_total",
		metric.WithDescription("run.trigger_failed envelopes the recorder stored, or skipped because a row already exists"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_ingest_trigger_failed_total: %w", err)
	}

	m.triggerAccepted, err = meter.Int64Counter("recorder_ingest_trigger_accepted_total",
		metric.WithDescription("run.trigger_accepted envelopes the recorder stored, or skipped because a row already exists"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_ingest_trigger_accepted_total: %w", err)
	}

	m.commandRefused, err = meter.Int64Counter("recorder_ingest_command_refused_total",
		metric.WithDescription("run.command_refused envelopes the recorder stored, or skipped because no run row matched or the run holds a notice that is not older"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_ingest_command_refused_total: %w", err)
	}

	m.backlogMsgs, err = meter.Int64ObservableGauge("recorder_ingest_backlog_msgs",
		metric.WithDescription("Pending message count on the recorder consumer"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			// A lookup failure skips this collection instead of erroring
			// it, so one slow response never breaks metrics scraping.
			if info, infoErr := conn.JetStream().ConsumerInfo(streamName, consumerName); infoErr == nil {
				obs.Observe(int64(info.NumPending))
			}
			return nil
		}))
	if err != nil {
		return nil, fmt.Errorf("register recorder_ingest_backlog_msgs: %w", err)
	}

	m.backlogBytes, err = meter.Int64ObservableGauge("recorder_ingest_backlog_bytes",
		metric.WithDescription("Byte size of the varroa_builds stream"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			if info, infoErr := conn.JetStream().StreamInfo(streamName); infoErr == nil {
				obs.Observe(int64(info.State.Bytes))
			}
			return nil
		}))
	if err != nil {
		return nil, fmt.Errorf("register recorder_ingest_backlog_bytes: %w", err)
	}

	return m, nil
}

// addUnknown counts one recorder_ingest_unknown_total event, labelled by
// cause.
func (m *IngestMetrics) addUnknown(ctx context.Context, cause string) {
	if m == nil || m.unknown == nil {
		return
	}
	m.unknown.Add(ctx, 1, metric.WithAttributes(attribute.String("cause", cause)))
}

// addConflict counts one recorder_ingest_conflict_total event.
func (m *IngestMetrics) addConflict(ctx context.Context) {
	if m == nil || m.conflict == nil {
		return
	}
	m.conflict.Add(ctx, 1)
}

// addOrphan counts one recorder_ingest_orphan_total event.
func (m *IngestMetrics) addOrphan(ctx context.Context) {
	if m == nil || m.orphan == nil {
		return
	}
	m.orphan.Add(ctx, 1)
}

// addScheduleTruncated counts one recorder_schedules_truncated_total
// event.
func (m *IngestMetrics) addScheduleTruncated(ctx context.Context) {
	if m == nil || m.scheduleTruncated == nil {
		return
	}
	m.scheduleTruncated.Add(ctx, 1)
}

// addTriggerFailed counts one recorder_ingest_trigger_failed_total
// event, labelled by reason code.
func (m *IngestMetrics) addTriggerFailed(ctx context.Context, reasonCode string) {
	if m == nil || m.triggerFailed == nil {
		return
	}
	m.triggerFailed.Add(ctx, 1, metric.WithAttributes(attribute.String("reason_code", reasonCode)))
}

// addScheduleStale counts one recorder_schedules_stale_total event.
func (m *IngestMetrics) addScheduleStale(ctx context.Context) {
	if m == nil || m.scheduleStale == nil {
		return
	}
	m.scheduleStale.Add(ctx, 1)
}

// addInventoryAhead counts one recorder_job_inventory_ahead_total event,
// labelled by envelope kind.
func (m *IngestMetrics) addInventoryAhead(ctx context.Context, kind string) {
	if m == nil || m.inventoryAhead == nil {
		return
	}
	m.inventoryAhead.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind)))
}

// addNumberMismatch counts one recorder_ingest_number_mismatch_total
// event.
func (m *IngestMetrics) addNumberMismatch(ctx context.Context) {
	if m == nil || m.numberMismatch == nil {
		return
	}
	m.numberMismatch.Add(ctx, 1)
}

// addSupersededStart counts one recorder_ingest_superseded_start_total
// event.
func (m *IngestMetrics) addSupersededStart(ctx context.Context) {
	if m == nil || m.supersededStart == nil {
		return
	}
	m.supersededStart.Add(ctx, 1)
}

// addTriggerAccepted counts one recorder_ingest_trigger_accepted_total
// event, labelled by whether the insert stored a row.
func (m *IngestMetrics) addTriggerAccepted(ctx context.Context, stored bool) {
	if m == nil || m.triggerAccepted == nil {
		return
	}
	m.triggerAccepted.Add(ctx, 1, metric.WithAttributes(attribute.Bool("stored", stored)))
}

// addCommandRefused counts one recorder_ingest_command_refused_total
// event, labelled by the refused command and by whether the write stored
// the notice.
func (m *IngestMetrics) addCommandRefused(ctx context.Context, command string, stored bool) {
	if m == nil || m.commandRefused == nil {
		return
	}
	m.commandRefused.Add(ctx, 1, metric.WithAttributes(
		attribute.String("command", command), attribute.Bool("stored", stored)))
}
