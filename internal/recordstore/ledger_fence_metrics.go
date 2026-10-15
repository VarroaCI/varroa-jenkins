package recordstore

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// FenceMetrics holds the recorder's fence-veto instrument on the
// "varroa-recorder" meter.
type FenceMetrics struct {
	veto metric.Int64Counter
}

// NewFenceMetrics registers recorder_fence_veto_total.
func NewFenceMetrics(mp metric.MeterProvider) (*FenceMetrics, error) {
	meter := mp.Meter("varroa-recorder")

	veto, err := meter.Int64Counter("recorder_fence_veto_total",
		metric.WithDescription("Claims the placement fence vetoed, labelled by reason"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_fence_veto_total: %w", err)
	}

	return &FenceMetrics{veto: veto}, nil
}

// addVeto counts one veto, labelled by reason. A nil receiver, or a
// FenceMetrics built with no counter, is a no-op, so a PlacementFence
// built with no metrics stays silent.
func (m *FenceMetrics) addVeto(ctx context.Context, reason string) {
	if m == nil || m.veto == nil {
		return
	}
	m.veto.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}
