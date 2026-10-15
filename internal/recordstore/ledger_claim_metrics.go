package recordstore

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ClaimMetrics holds the claim responder's two veto instruments on the
// "varroa-recorder" meter. recorder_fence_veto_total counts only the
// vetoes of the placement fence; recorder_claims_vetoed_total counts
// every veto the responder answers, INVALID_KEY included.
// recorder_claim_veto_marked_total counts the vetoed runs it marks
// TRIGGER_FAILED. recorder_start_marks_total counts the start marks it
// answers, labelled by decision.
type ClaimMetrics struct {
	vetoed     metric.Int64Counter
	vetoMarked metric.Int64Counter
	startMarks metric.Int64Counter
}

// NewClaimMetrics registers recorder_claims_vetoed_total,
// recorder_claim_veto_marked_total, and recorder_start_marks_total.
func NewClaimMetrics(mp metric.MeterProvider) (*ClaimMetrics, error) {
	meter := mp.Meter("varroa-recorder")

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

	startMarks, err := meter.Int64Counter("recorder_start_marks_total",
		metric.WithDescription("Start marks the recorder answered, labelled by decision"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_start_marks_total: %w", err)
	}

	return &ClaimMetrics{vetoed: vetoed, vetoMarked: vetoMarked, startMarks: startMarks}, nil
}

// addStartMark counts one answered start mark, labelled by decision. A
// failure answer counts under the decision "error". A nil receiver is a
// no-op.
func (m *ClaimMetrics) addStartMark(ctx context.Context, decision string) {
	if m == nil || m.startMarks == nil {
		return
	}
	if decision == "" {
		decision = "error"
	}
	m.startMarks.Add(ctx, 1, metric.WithAttributes(attribute.String("decision", decision)))
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
