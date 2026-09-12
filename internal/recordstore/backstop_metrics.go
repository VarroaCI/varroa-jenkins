package recordstore

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// BackstopMetrics holds the backstop cron scheduler's instruments
// (design D11) on the "varroa-recorder" meter: recorder_cron_fired_total
// and recorder_cron_missed_total.
type BackstopMetrics struct {
	fired  metric.Int64Counter
	missed metric.Int64Counter
}

// NewBackstopMetrics registers the backstop scheduler's counters.
func NewBackstopMetrics() (*BackstopMetrics, error) {
	meter := otel.Meter("varroa-recorder")

	fired, err := meter.Int64Counter("recorder_cron_fired_total",
		metric.WithDescription("Backstop triggers published"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_cron_fired_total: %w", err)
	}
	missed, err := meter.Int64Counter("recorder_cron_missed_total",
		metric.WithDescription("Exported occurrences that no fire covered"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_cron_missed_total: %w", err)
	}

	return &BackstopMetrics{fired: fired, missed: missed}, nil
}

// addFired counts one published backstop trigger. A nil receiver is a
// no-op.
func (m *BackstopMetrics) addFired(ctx context.Context) {
	if m == nil || m.fired == nil {
		return
	}
	m.fired.Add(ctx, 1)
}

// addMissed counts n occurrences no fire covered. A nil receiver is a
// no-op.
func (m *BackstopMetrics) addMissed(ctx context.Context, n int64) {
	if m == nil || m.missed == nil || n <= 0 {
		return
	}
	m.missed.Add(ctx, n)
}
