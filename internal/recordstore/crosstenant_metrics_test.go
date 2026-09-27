package recordstore

import (
	"context"
	"testing"
)

// TestCrossTenantGrantDenialReason covers task 4.5's seven-cause mapping,
// design.md D5's own grant-message cause table.
func TestCrossTenantGrantDenialReason(t *testing.T) {
	tests := []struct {
		message string
		want    string
	}{
		{crossTenantMessage + " Tenant beta is not placed. A cell role is required.", denialReasonNotPlaced},
		{crossTenantMessage + " Only the Varroa path may cross a tenant boundary.", denialReasonWrongScheme},
		{crossTenantMessage + " Tenant ghost does not exist.", denialReasonNoTarget},
		{crossTenantMessage + " Tenant beta exists in clusters core, other. Name one cluster.", denialReasonAmbiguousTarget},
		{crossTenantMessage + " Tenant beta exports no job matching alpha/x.", denialReasonNoJobExport},
		{crossTenantMessage + " Tenant beta exports no lock matching deploy-prod.", denialReasonNoLockExport},
		{crossTenantMessage + " Tenant beta does not grant job alpha/x to gamma.", denialReasonCallerNotListed},
		{"an unrecognized message", denialReasonOther},
	}
	for _, tc := range tests {
		if got := crossTenantGrantDenialReason(tc.message); got != tc.want {
			t.Errorf("crossTenantGrantDenialReason(%q) = %q, want %q", tc.message, got, tc.want)
		}
	}
}

// TestCrossTenantRequestOutcome covers the four outcome values.
func TestCrossTenantRequestOutcome(t *testing.T) {
	tests := []struct {
		resp CrossTenantResponse
		want string
	}{
		{CrossTenantResponse{}, crossTenantOutcomeAllowed},
		{CrossTenantResponse{ReasonCode: ReasonCrossTenant}, crossTenantOutcomeDenied},
		{CrossTenantResponse{Code: CodeInternal}, crossTenantOutcomeError},
		{CrossTenantResponse{Code: CodeNotFound}, crossTenantOutcomeNotFound},
	}
	for _, tc := range tests {
		if got := crossTenantRequestOutcome(tc.resp); got != tc.want {
			t.Errorf("crossTenantRequestOutcome(%+v) = %q, want %q", tc.resp, got, tc.want)
		}
	}
}

// TestCrossTenantMetrics_RequestsAndGrantDeniedCountThroughDispatch
// asserts task 4.5's own acceptance: recorder_crosstenant_requests_total
// increments per request, labelled by kind and outcome, and
// recorder_crosstenant_grant_denied_total increments on a CROSS_TENANT
// veto, labelled by reason.
func TestCrossTenantMetrics_RequestsAndGrantDeniedCountThroughDispatch(t *testing.T) {
	deps, db := lockTestDeps(t)
	reader := fanoutTestMeter(t)
	metrics, err := NewCrossTenantMetrics()
	if err != nil {
		t.Fatalf("new cross-tenant metrics: %v", err)
	}
	deps.Metrics = metrics

	// An unknown kind: outcome "error".
	dispatchCrossTenant(context.Background(), deps, CrossTenantRequest{Kind: "no-such-kind"})

	// A lock_acquire from an unplaced caller: order 1 veto, outcome
	// "denied", reason "not_placed".
	dispatchCrossTenant(context.Background(), deps, CrossTenantRequest{
		Kind:         CrossTenantKindLockAcquire,
		Cluster:      testCluster,
		CallerTenant: "unplaced",
		Name:         "deploy-prod",
		RunID:        "run-1",
	})

	// A granted lock_acquire inside the caller's own prefix: outcome
	// "allowed".
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 1, "pod-a", "pod-a", nil)
	dispatchCrossTenant(context.Background(), deps, CrossTenantRequest{
		Kind:                CrossTenantKindLockAcquire,
		Cluster:             testCluster,
		Namespace:           "ns-alpha",
		Cell:                "cell-alpha",
		CallerTenant:        "alpha",
		PlacementGeneration: 1,
		Name:                "deploy-prod",
		RunID:               "run-2",
	})

	if got := fanoutMetricSum(t, reader, "recorder_crosstenant_requests_total",
		map[string]string{"kind": "no-such-kind", "outcome": crossTenantOutcomeError}); got != 1 {
		t.Fatalf("unknown-kind requests = %d, want 1", got)
	}
	if got := fanoutMetricSum(t, reader, "recorder_crosstenant_requests_total",
		map[string]string{"kind": CrossTenantKindLockAcquire, "outcome": crossTenantOutcomeDenied}); got != 1 {
		t.Fatalf("denied lock_acquire requests = %d, want 1", got)
	}
	if got := fanoutMetricSum(t, reader, "recorder_crosstenant_requests_total",
		map[string]string{"kind": CrossTenantKindLockAcquire, "outcome": crossTenantOutcomeAllowed}); got != 1 {
		t.Fatalf("allowed lock_acquire requests = %d, want 1", got)
	}
	if got := fanoutMetricSum(t, reader, "recorder_crosstenant_grant_denied_total",
		map[string]string{"reason": denialReasonNotPlaced}); got != 1 {
		t.Fatalf("not_placed grant denials = %d, want 1", got)
	}
}
