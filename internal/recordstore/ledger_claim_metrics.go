package recordstore

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ClaimMetrics holds the claim responder's veto instrument on the
// "varroa-recorder" meter. recorder_fence_veto_total counts only the
// vetoes of the placement fence; this counter counts every veto the
// responder answers, INVALID_KEY included.
type ClaimMetrics struct {
	vetoed metric.Int64Counter
}

// NewClaimMetrics registers recorder_claims_vetoed_total.
func NewClaimMetrics() (*ClaimMetrics, error) {
	meter := otel.Meter("varroa-recorder")

	vetoed, err := meter.Int64Counter("recorder_claims_vetoed_total",
		metric.WithDescription("Claims the recorder vetoed, labelled by reason"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_claims_vetoed_total: %w", err)
	}

	return &ClaimMetrics{vetoed: vetoed}, nil
}

// addVeto counts one veto, labelled by reason. A nil receiver is a
// no-op.
func (m *ClaimMetrics) addVeto(ctx context.Context, reason string) {
	if m == nil || m.vetoed == nil {
		return
	}
	m.vetoed.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}
