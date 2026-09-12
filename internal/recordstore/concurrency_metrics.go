package recordstore

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// ConcurrencyMetrics holds the concurrency-cancel instruments (design
// D10) on the "varroa-recorder" meter: recorder_concurrency_cancel_total
// and recorder_concurrency_cancel_skipped_total.
type ConcurrencyMetrics struct {
	cancel  metric.Int64Counter
	skipped metric.Int64Counter
}

// NewConcurrencyMetrics registers the concurrency-cancel counters.
func NewConcurrencyMetrics() (*ConcurrencyMetrics, error) {
	meter := otel.Meter("varroa-recorder")

	cancel, err := meter.Int64Counter("recorder_concurrency_cancel_total",
		metric.WithDescription("cancel_run actions published for a cancelInProgress tenant's older runs"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_concurrency_cancel_total: %w", err)
	}
	skipped, err := meter.Int64Counter("recorder_concurrency_cancel_skipped_total",
		metric.WithDescription("Runs above the eight-run concurrency-cancel limit"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_concurrency_cancel_skipped_total: %w", err)
	}

	return &ConcurrencyMetrics{cancel: cancel, skipped: skipped}, nil
}

// addCancel counts one published cancel_run action. A nil receiver is a
// no-op.
func (m *ConcurrencyMetrics) addCancel(ctx context.Context) {
	if m == nil || m.cancel == nil {
		return
	}
	m.cancel.Add(ctx, 1)
}

// addSkipped counts n runs above the eight-run limit. A nil receiver is
// a no-op.
func (m *ConcurrencyMetrics) addSkipped(ctx context.Context, n int64) {
	if m == nil || m.skipped == nil || n <= 0 {
		return
	}
	m.skipped.Add(ctx, n)
}
