package recordstore

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ClaimMetrics holds the claim responder's two veto instruments on the
// "varroa-recorder" meter. recorder_fence_veto_total counts only the
// vetoes of the placement fence; recorder_claims_vetoed_total counts
// every veto the responder answers, INVALID_KEY included.
// recorder_claim_veto_marked_total counts the vetoed runs it marks
// TRIGGER_FAILED.
type ClaimMetrics struct {
	vetoed     metric.Int64Counter
	vetoMarked metric.Int64Counter
}

// NewClaimMetrics registers recorder_claims_vetoed_total and
// recorder_claim_veto_marked_total.
func NewClaimMetrics() (*ClaimMetrics, error) {
	meter := otel.Meter("varroa-recorder")

	vetoed, err := meter.Int64Counter("recorder_claims_vetoed_total",
		metric.WithDescription("Claims the recorder vetoed, labelled by reason"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_claims_vetoed_total: %w", err)
	}
	vetoMarked, err := meter.Int64Counter("recorder_claim_veto_marked_total",
		metric.WithDescription("Vetoed runs the recorder marked TRIGGER_FAILED, labelled by reason"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_claim_veto_marked_total: %w", err)
	}

	return &ClaimMetrics{vetoed: vetoed, vetoMarked: vetoMarked}, nil
}

// addVetoMarked counts one vetoed run the responder marked, labelled by
// reason. A nil receiver is a no-op.
func (m *ClaimMetrics) addVetoMarked(ctx context.Context, reason string) {
	if m == nil || m.vetoMarked == nil {
		return
	}
	m.vetoMarked.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// addVeto counts one veto, labelled by reason. A nil receiver is a
// no-op.
func (m *ClaimMetrics) addVeto(ctx context.Context, reason string) {
	if m == nil || m.vetoed == nil {
		return
	}
	m.vetoed.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}
