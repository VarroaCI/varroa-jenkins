package recordstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// forwardRequest builds one intra-tenant forward of alpha/nightly,
// carrying the key the claimer derived.
func forwardRequest(intentKey string) CrossTenantRequest {
	return CrossTenantRequest{
		Cluster:             "core",
		Namespace:           "ns-a",
		Cell:                "cell-a",
		CallerTenant:        "alpha",
		PlacementGeneration: 1,
		Kind:                CrossTenantKindBuild,
		CallerRunID:         "caller-run-1",
		Seq:                 "s1",
		TargetTenant:        "alpha",
		JobPath:             "nightly",
		Params:              json.RawMessage(`{}`),
		IntentKey:           intentKey,
	}
}

// TestCrossTenantBuild_SameTenantForwardKeepsItsKey asserts the
// responder uses a supplied key unchanged when the caller tenant equals
// the target tenant.
func TestCrossTenantBuild_SameTenantForwardKeepsItsKey(t *testing.T) {
	deps, _ := buildTestDeps(t)
	actions := subscribeLedgerActions(t, deps)
	supplied := "cron:alpha/nightly:2026-09-17T03:00"

	resp, err := handleCrossTenantBuild(context.Background(), deps, forwardRequest(supplied))
	if err != nil {
		t.Fatalf("handleCrossTenantBuild: %v", err)
	}
	if resp.Code != "" || resp.ReasonCode != "" {
		t.Fatalf("a same-tenant forward must be accepted, got code=%q reason=%q", resp.Code, resp.ReasonCode)
	}

	select {
	case action := <-actions:
		if action.IntentKey != supplied {
			t.Fatalf("intent_key = %q, want the supplied %q", action.IntentKey, supplied)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the trigger_run action")
	}
}

// TestCrossTenantBuild_SecondForwardOfOneCronAnswersTheExistingRun
// asserts a repeat forward of one key finds the first intent and
// publishes no second action.
func TestCrossTenantBuild_SecondForwardOfOneCronAnswersTheExistingRun(t *testing.T) {
	deps, db := buildTestDeps(t)
	actions := subscribeLedgerActions(t, deps)
	supplied := "cron:alpha/nightly:2026-09-17T03:00"

	if _, err := handleCrossTenantBuild(context.Background(), deps, forwardRequest(supplied)); err != nil {
		t.Fatalf("first forward: %v", err)
	}
	var first LedgerAction
	select {
	case first = <-actions:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first action")
	}

	// The target cell's claim gate writes the one run_intents row; the
	// responder only reads it.
	if _, err := db.Exec(`
		INSERT INTO varroa.run_intents
			(intent_key, run_id, created_at, cluster, tenant, job_path, state, cell, namespace, cell_instance)
		VALUES ($1, $2, now(), 'core', 'alpha', 'alpha/nightly', 'GRANTED', 'cell-a', 'ns-a', 'pod-a')
	`, supplied, first.RunID); err != nil {
		t.Fatalf("seed run_intents: %v", err)
	}

	resp, err := handleCrossTenantBuild(context.Background(), deps, forwardRequest(supplied))
	if err != nil {
		t.Fatalf("second forward: %v", err)
	}
	if resp.RunID != first.RunID {
		t.Fatalf("run_id = %q, want the existing %q", resp.RunID, first.RunID)
	}
	select {
	case extra := <-actions:
		t.Fatalf("got a second action %+v, want none", extra)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestCrossTenantBuild_CrossTenantCallMayNotSupplyAKey asserts a request
// whose tenants differ and that carries an intent key is refused with
// code=INTERNAL, and that it publishes nothing.
func TestCrossTenantBuild_CrossTenantCallMayNotSupplyAKey(t *testing.T) {
	deps, _ := buildTestDeps(t)
	actions := subscribeLedgerActions(t, deps)

	req := forwardRequest("upstream:caller-run-1:s1")
	req.TargetTenant = "beta"

	resp, err := handleCrossTenantBuild(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("handleCrossTenantBuild: %v", err)
	}
	if resp.Code != CodeInternal {
		t.Fatalf("code = %q, want %q", resp.Code, CodeInternal)
	}
	select {
	case extra := <-actions:
		t.Fatalf("got action %+v, want none", extra)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestCrossTenantBuild_UnparsableKeyAnswersInvalidKey asserts a supplied
// key outside the allocated schemes answers reason_code=INVALID_KEY and
// publishes nothing.
func TestCrossTenantBuild_UnparsableKeyAnswersInvalidKey(t *testing.T) {
	deps, _ := buildTestDeps(t)
	actions := subscribeLedgerActions(t, deps)

	resp, err := handleCrossTenantBuild(context.Background(), deps, forwardRequest("no-scheme-at-all"))
	if err != nil {
		t.Fatalf("handleCrossTenantBuild: %v", err)
	}
	if resp.ReasonCode != ReasonInvalidKey {
		t.Fatalf("reason_code = %q, want %q", resp.ReasonCode, ReasonInvalidKey)
	}
	select {
	case extra := <-actions:
		t.Fatalf("got action %+v, want none", extra)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestCrossTenantBuild_NoKeyStillDerivesOne asserts a call that carries
// no intent key derives the key exactly as it does today.
func TestCrossTenantBuild_NoKeyStillDerivesOne(t *testing.T) {
	deps, _ := buildTestDeps(t)
	actions := subscribeLedgerActions(t, deps)

	if _, err := handleCrossTenantBuild(context.Background(), deps, forwardRequest("")); err != nil {
		t.Fatalf("handleCrossTenantBuild: %v", err)
	}

	select {
	case action := <-actions:
		if action.IntentKey != "upstream:caller-run-1:s1" {
			t.Fatalf("intent_key = %q, want the derived upstream:caller-run-1:s1", action.IntentKey)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the trigger_run action")
	}
}

// TestCrossTenantBuild_MalformedSeqIsStillRefusedWithAKey asserts the
// seq format test keeps its place before the key is read.
func TestCrossTenantBuild_MalformedSeqIsStillRefusedWithAKey(t *testing.T) {
	deps, _ := buildTestDeps(t)

	req := forwardRequest("cron:alpha/nightly:2026-09-17T03:00")
	req.Seq = "1"

	resp, err := handleCrossTenantBuild(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("handleCrossTenantBuild: %v", err)
	}
	if resp.Code != CodeInternal {
		t.Fatalf("code = %q, want %q", resp.Code, CodeInternal)
	}
}
