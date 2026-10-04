package recordstore

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// CrossTenantMetrics holds the cross-tenant responder's two instruments
// on the "varroa-recorder" meter, design.md D15.
type CrossTenantMetrics struct {
	requests    metric.Int64Counter
	grantDenied metric.Int64Counter
}

// NewCrossTenantMetrics registers recorder_crosstenant_requests_total
// and recorder_crosstenant_grant_denied_total.
func NewCrossTenantMetrics(mp metric.MeterProvider) (*CrossTenantMetrics, error) {
	meter := mp.Meter("varroa-recorder")

	requests, err := meter.Int64Counter("recorder_crosstenant_requests_total",
		metric.WithDescription("Requests on the cross-tenant subject, labelled by kind and outcome"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_crosstenant_requests_total: %w", err)
	}

	grantDenied, err := meter.Int64Counter("recorder_crosstenant_grant_denied_total",
		metric.WithDescription("Requests and lock calls the grant evaluator vetoed, labelled by reason"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_crosstenant_grant_denied_total: %w", err)
	}

	return &CrossTenantMetrics{requests: requests, grantDenied: grantDenied}, nil
}

// The four recorder_crosstenant_requests_total outcome values.
const (
	crossTenantOutcomeAllowed  = "allowed"
	crossTenantOutcomeDenied   = "denied"
	crossTenantOutcomeError    = "error"
	crossTenantOutcomeNotFound = "not_found"
)

// addRequest counts one request, labelled by kind and outcome. A nil
// receiver is a no-op.
func (m *CrossTenantMetrics) addRequest(ctx context.Context, kind, outcome string) {
	if m == nil || m.requests == nil {
		return
	}
	m.requests.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind), attribute.String("outcome", outcome)))
}

// addGrantDenied counts one veto, labelled by reason. A nil receiver is
// a no-op.
func (m *CrossTenantMetrics) addGrantDenied(ctx context.Context, reason string) {
	if m == nil || m.grantDenied == nil {
		return
	}
	m.grantDenied.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// The seven recorder_crosstenant_grant_denied_total reason values,
// design.md D5's grant-message cause table.
const (
	denialReasonNotPlaced       = "not_placed"
	denialReasonWrongScheme     = "wrong_intent_scheme"
	denialReasonNoTarget        = "no_target"
	denialReasonAmbiguousTarget = "ambiguous_target"
	denialReasonNoJobExport     = "no_job_export"
	denialReasonNoLockExport    = "no_lock_export"
	denialReasonCallerNotListed = "caller_not_listed"
	denialReasonOther           = "other"
)

// crossTenantGrantDenialReason maps a CROSS_TENANT veto's message to one
// of the grant-message cause table's seven rows. GrantResult carries no
// machine-readable cause of its own: its shape stays fixed to Allow,
// ReasonCode, and Message, because ledger_fence_placement.go converts a
// GrantResult straight to a FenceResult with an identical field list. The
// message's added clause is itself a frozen contract this same table
// fixes, so matching on it costs no new coupling.
func crossTenantGrantDenialReason(message string) string {
	switch {
	case strings.Contains(message, "is not placed. A cell role is required."):
		return denialReasonNotPlaced
	case strings.Contains(message, "Only the Varroa path may cross a tenant boundary."):
		return denialReasonWrongScheme
	case strings.Contains(message, "Name one cluster."):
		return denialReasonAmbiguousTarget
	case strings.Contains(message, "does not exist."):
		return denialReasonNoTarget
	case strings.Contains(message, "exports no job matching"):
		return denialReasonNoJobExport
	case strings.Contains(message, "exports no lock matching"):
		return denialReasonNoLockExport
	case strings.Contains(message, "does not grant"):
		return denialReasonCallerNotListed
	default:
		return denialReasonOther
	}
}

// crossTenantRequestOutcome classifies resp for the requests_total
// outcome label: not_found when Code is NOT_FOUND, error when a server
// fault set any other Code, denied when a placement or grant veto set
// ReasonCode, allowed otherwise. A missing record is a caller mistake,
// so it stays out of the error rate.
func crossTenantRequestOutcome(resp CrossTenantResponse) string {
	switch {
	case resp.Code == CodeNotFound:
		return crossTenantOutcomeNotFound
	case resp.Code != "":
		return crossTenantOutcomeError
	case resp.ReasonCode != "":
		return crossTenantOutcomeDenied
	default:
		return crossTenantOutcomeAllowed
	}
}
