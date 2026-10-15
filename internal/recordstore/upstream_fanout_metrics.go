package recordstore

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// UpstreamFanoutMetrics holds the upstream fan-out's two instruments on
// the "varroa-recorder" meter, design.md D15.
type UpstreamFanoutMetrics struct {
	published metric.Int64Counter
	skipped   metric.Int64Counter
}

// NewUpstreamFanoutMetrics registers recorder_upstream_fanout_total and
// recorder_upstream_fanout_skipped_total.
func NewUpstreamFanoutMetrics(mp metric.MeterProvider) (*UpstreamFanoutMetrics, error) {
	meter := mp.Meter("varroa-recorder")

	published, err := meter.Int64Counter("recorder_upstream_fanout_total",
		metric.WithDescription("trigger_run actions the upstream fan-out published"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_upstream_fanout_total: %w", err)
	}

	skipped, err := meter.Int64Counter("recorder_upstream_fanout_skipped_total",
		metric.WithDescription("Subscribers the upstream fan-out did not build, labelled by reason"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_upstream_fanout_skipped_total: %w", err)
	}

	return &UpstreamFanoutMetrics{published: published, skipped: skipped}, nil
}

// addPublished counts one published trigger_run action. A nil receiver
// is a no-op.
func (m *UpstreamFanoutMetrics) addPublished(ctx context.Context) {
	if m == nil || m.published == nil {
		return
	}
	m.published.Add(ctx, 1)
}

// addSkipped counts one skipped subscriber, labelled by reason. A nil
// receiver is a no-op.
func (m *UpstreamFanoutMetrics) addSkipped(ctx context.Context, reason string) {
	if m == nil || m.skipped == nil {
		return
	}
	m.skipped.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// addSkippedN counts n skipped subscribers at once, labelled by reason,
// for the over_cap reason design.md D9 counts in bulk. A nil receiver,
// or a non-positive n, is a no-op.
func (m *UpstreamFanoutMetrics) addSkippedN(ctx context.Context, reason string, n int) {
	if m == nil || m.skipped == nil || n <= 0 {
		return
	}
	m.skipped.Add(ctx, int64(n), metric.WithAttributes(attribute.String("reason", reason)))
}
