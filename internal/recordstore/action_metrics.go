package recordstore

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ActionMetrics holds recorder_actions_published_total, the counter
// design D15 defines for every action this section publishes on
// varroa.ledger.actions.<cluster>: the concurrency cancel, the
// interrupted-run cancel and automatic retry, and the backstop wake and
// retry. It is a separate instrument from the per-capability counters
// (recorder_concurrency_cancel_total, recorder_cron_fired_total, and so
// on), because it counts every kind under one name, labelled by kind.
type ActionMetrics struct {
	published      metric.Int64Counter
	jobHomeChanged metric.Int64Counter
}

// Causes of a job home change, the label values of
// recorder_job_home_changed_total.
const (
	// jobHomeCauseScale names a path the active slot count moved.
	jobHomeCauseScale = "scale"
	// jobHomeCauseTakeover names a path the member set moved.
	jobHomeCauseTakeover = "takeover"
	// jobHomeCauseNewJob names a path the recorder homes for the first
	// time.
	jobHomeCauseNewJob = "new-job"
)

// NewActionMetrics registers recorder_actions_published_total on the
// "varroa-recorder" meter.
func NewActionMetrics(mp metric.MeterProvider) (*ActionMetrics, error) {
	meter := mp.Meter("varroa-recorder")
	published, err := meter.Int64Counter("recorder_actions_published_total",
		metric.WithDescription("Ledger actions published on the action subject, labelled by kind"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_actions_published_total: %w", err)
	}
	jobHomeChanged, err := meter.Int64Counter("recorder_job_home_changed_total",
		metric.WithDescription("Job home slot changes the recorder stored, labelled by cause"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_job_home_changed_total: %w", err)
	}
	return &ActionMetrics{published: published, jobHomeChanged: jobHomeChanged}, nil
}

// addJobHomeChanged counts one moved job path, labelled by cause. A nil
// receiver is a no-op, so an unwired caller counts nothing.
func (m *ActionMetrics) addJobHomeChanged(ctx context.Context, cause string) {
	if m == nil || m.jobHomeChanged == nil {
		return
	}
	m.jobHomeChanged.Add(ctx, 1, metric.WithAttributes(attribute.String("cause", cause)))
}

// addPublished counts one published action, labelled by kind. A nil
// receiver is a no-op, so an unwired caller counts nothing.
func (m *ActionMetrics) addPublished(ctx context.Context, kind string) {
	if m == nil || m.published == nil {
		return
	}
	m.published.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind)))
}
