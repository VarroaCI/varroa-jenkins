package recordstore

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestCrossTenantRunPrefersTheIntentOverATriggerFailedRow asserts a
// TRIGGER_FAILED row loses to an intent row that names the same run, by
// either identifier: a redelivered trigger that a claim then granted is
// live, and the caller must keep waiting on it.
func TestCrossTenantRunPrefersTheIntentOverATriggerFailedRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:cross-state-2", 0)
	granted := mustGrant(t, ledger, req)
	_, createdAt, err := parseRunUUIDv7(granted.RunID)
	if err != nil {
		t.Fatalf("parse run id: %v", err)
	}
	seedSyntheticRowForRun(t, db, uuid.MustParse(granted.RunID), createdAt)

	for _, lookup := range [][2]string{{"run_id", granted.RunID}, {"intent_key", req.IntentKey}} {
		runID, rec, found, err := readCrossTenantRun(context.Background(), pool, lookup[0], lookup[1])
		if err != nil {
			t.Fatalf("readCrossTenantRun(%s): %v", lookup[0], err)
		}
		if !found || runID != granted.RunID {
			t.Fatalf("readCrossTenantRun(%s): found=%v run_id=%q, want %q", lookup[0], found, runID, granted.RunID)
		}
		if got := crossTenantRunState(rec); got != CrossTenantStateQueued {
			t.Fatalf("readCrossTenantRun(%s): state = %q, want %q", lookup[0], got, CrossTenantStateQueued)
		}
		if rec.triggerReasonCode != "" {
			t.Fatalf("readCrossTenantRun(%s): trigger reason %q, want none", lookup[0], rec.triggerReasonCode)
		}
	}
}

// seedVetoedCrossTenantRun marks one run of beta/release TRIGGER_FAILED
// through the veto-marking claim responder, as a permanent veto on the
// target cell does. The run holds no run_intents row. It answers the run
// identifier and the intent key.
func seedVetoedCrossTenantRun(t *testing.T, pool *pgxpool.Pool, db *sql.DB, reason string) (string, string) {
	t.Helper()
	seedGrantPlacement(t, db, testCluster, "alpha", "ns-alpha", "cell-alpha", 1, "{}")
	seedGrantPlacement(t, db, testCluster, "beta", "ns-beta", "cell-beta", 1,
		`{"exports":[{"job":"release","callers":["alpha"]}]}`)

	runID := newTestUUIDv7String()
	intentKey := "upstream:" + newTestUUIDv7String() + ":s1"
	req := newVarroaClaim(runID)
	req.IntentKey = intentKey
	req.Namespace = "ns-beta"
	req.Cell = "cell-beta"
	req.Tenant = "beta"
	req.JobPath = "beta/release"

	ledger := newVetoMarkingLedger(pool, denyingFence{reasonCode: reason}, nil)
	resp, _ := claimThroughResponder(t, ledger, nil, req)
	if resp.Decision != DecisionVetoed {
		t.Fatalf("want a vetoed claim, got %+v", resp)
	}
	assertVetoMarked(t, db, runID, reason)
	return runID, intentKey
}

// assertTriggerFailedReply asserts resp reads runID as a completed run
// with a TRIGGER_FAILED result and reason as its trigger reason code.
func assertTriggerFailedReply(t *testing.T, resp CrossTenantResponse, runID, reason string) {
	t.Helper()
	if resp.Code != "" || resp.ReasonCode != "" {
		t.Fatalf("got code %q reason_code %q (%s%s), want an allowed reply",
			resp.Code, resp.ReasonCode, resp.Error, resp.Message)
	}
	if resp.RunID != runID {
		t.Fatalf("run_id: got %q, want %q", resp.RunID, runID)
	}
	if resp.State != CrossTenantStateCompleted {
		t.Fatalf("state: got %q, want %q", resp.State, CrossTenantStateCompleted)
	}
	if resp.Result == nil || *resp.Result != ResultTriggerFailed {
		t.Fatalf("result: got %v, want %s", resp.Result, ResultTriggerFailed)
	}
	if resp.TriggerReasonCode != reason {
		t.Fatalf("trigger_reason_code: got %q, want %q", resp.TriggerReasonCode, reason)
	}
	if resp.TriggerError == "" {
		t.Fatal("trigger_error is empty, want the veto message")
	}
}

// TestCrossTenantStatus_VetoedRunByIntentKey asserts a caller that polls
// by intent key reads a vetoed run's TRIGGER_FAILED result and veto code,
// not pending.
func TestCrossTenantStatus_VetoedRunByIntentKey(t *testing.T) {
	deps, db := statusTestDeps(t)
	runID, intentKey := seedVetoedCrossTenantRun(t, deps.Pool, db, ReasonFenced)

	resp, err := handleCrossTenantStatus(context.Background(), deps, CrossTenantRequest{
		Cluster:             testCluster,
		Namespace:           "ns-alpha",
		Cell:                "cell-alpha",
		CallerTenant:        "alpha",
		PlacementGeneration: 1,
		IntentKey:           intentKey,
	})
	if err != nil {
		t.Fatalf("handleCrossTenantStatus: %v", err)
	}
	assertTriggerFailedReply(t, resp, runID, ReasonFenced)
}

// TestCrossTenantStatus_VetoedRunByRunID asserts a caller that polls by
// run identifier reads a vetoed run's result, not NOT_FOUND.
func TestCrossTenantStatus_VetoedRunByRunID(t *testing.T) {
	deps, db := statusTestDeps(t)
	runID, _ := seedVetoedCrossTenantRun(t, deps.Pool, db, ReasonCrossTenant)

	resp, err := handleCrossTenantStatus(context.Background(), deps, CrossTenantRequest{
		Cluster:             testCluster,
		Namespace:           "ns-alpha",
		Cell:                "cell-alpha",
		CallerTenant:        "alpha",
		PlacementGeneration: 1,
		RunID:               runID,
	})
	if err != nil {
		t.Fatalf("handleCrossTenantStatus: %v", err)
	}
	assertTriggerFailedReply(t, resp, runID, ReasonCrossTenant)
}

// TestCrossTenantStatus_VetoedRunNeedsTheGrant asserts a caller that
// beta does not export release to reads the grant veto alone, by either
// identifier.
func TestCrossTenantStatus_VetoedRunNeedsTheGrant(t *testing.T) {
	deps, db := statusTestDeps(t)
	runID, intentKey := seedVetoedCrossTenantRun(t, deps.Pool, db, ReasonFenced)
	seedGrantPlacement(t, db, testCluster, "gamma", "ns-gamma", "cell-gamma", 1, "{}")

	for _, req := range []CrossTenantRequest{{RunID: runID}, {IntentKey: intentKey}} {
		req.Cluster = testCluster
		req.Namespace = "ns-gamma"
		req.Cell = "cell-gamma"
		req.CallerTenant = "gamma"
		req.PlacementGeneration = 1
		resp, err := handleCrossTenantStatus(context.Background(), deps, req)
		if err != nil {
			t.Fatalf("handleCrossTenantStatus: %v", err)
		}
		if resp.ReasonCode != ReasonCrossTenant {
			t.Fatalf("reason_code: got %q, want %q", resp.ReasonCode, ReasonCrossTenant)
		}
		if resp.Result != nil || resp.RunID != "" || resp.TriggerReasonCode != "" {
			t.Fatalf("got %+v, want no run fields on a veto", resp)
		}
	}
}
