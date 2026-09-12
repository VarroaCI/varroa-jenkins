package recordstore

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
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
	published metric.Int64Counter
}

// NewActionMetrics registers recorder_actions_published_total on the
// "varroa-recorder" meter.
func NewActionMetrics() (*ActionMetrics, error) {
	meter := otel.Meter("varroa-recorder")
	published, err := meter.Int64Counter("recorder_actions_published_total",
		metric.WithDescription("Ledger actions published on the action subject, labelled by kind"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_actions_published_total: %w", err)
	}
	return &ActionMetrics{published: published}, nil
}

// addPublished counts one published action, labelled by kind. A nil
// receiver is a no-op, so an unwired caller counts nothing.
func (m *ActionMetrics) addPublished(ctx context.Context, kind string) {
	if m == nil || m.published == nil {
		return
	}
	m.published.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind)))
}
