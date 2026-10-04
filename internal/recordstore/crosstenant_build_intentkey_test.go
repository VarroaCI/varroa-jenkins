package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"
)

// forwardCallerRunID is the run id the claimer of a forward holds. The
// API answered it to its caller, so the forward must keep it.
const forwardCallerRunID = "01a0e0d0-536f-7268-abbe-77cf21ed0fc1"

// buildForwardTestDeps builds the responder's dependencies over a
// two-slot alpha: cell-a holds slot 0 and homes alpha/nightly, and
// cell-b holds slot 1. forwardRequest calls from cell-b.
func buildForwardTestDeps(t *testing.T) (CrossTenantDeps, *sql.DB) {
	t.Helper()
	deps, db := buildTestDeps(t)
	seedTenantPlacementMembers(t, db, "alpha", 1, []member{
		{Cell: "cell-a", Namespace: "ns-a", Role: MemberRoleActive, Slot: slotPtr(0),
			Instance: "pod-a", RehydratedInstance: "pod-a"},
		{Cell: "cell-b", Namespace: "ns-a", Role: MemberRoleActive, Slot: slotPtr(1),
			Instance: "pod-b", RehydratedInstance: "pod-b"},
	}, map[string]int32{"alpha/nightly": 0})
	return deps, db
}

// forwardRequest builds one intra-tenant forward of alpha/nightly from
// cell-b, carrying the key the claimer derived.
func forwardRequest(intentKey string) CrossTenantRequest {
	return CrossTenantRequest{
		Cluster:             "core",
		Namespace:           "ns-a",
		Cell:                "cell-b",
		CallerTenant:        "alpha",
		PlacementGeneration: 1,
		Kind:                CrossTenantKindBuild,
		CallerRunID:         forwardCallerRunID,
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
	t.Parallel()

	deps, _ := buildForwardTestDeps(t)
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
	t.Parallel()

	deps, db := buildForwardTestDeps(t)
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
	t.Parallel()

	deps, _ := buildForwardTestDeps(t)
	actions := subscribeLedgerActions(t, deps)

	req := forwardRequest("upstream:" + forwardCallerRunID + ":s1")
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
	t.Parallel()

	deps, _ := buildForwardTestDeps(t)
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
	t.Parallel()

	deps, _ := buildForwardTestDeps(t)
	actions := subscribeLedgerActions(t, deps)

	if _, err := handleCrossTenantBuild(context.Background(), deps, forwardRequest("")); err != nil {
		t.Fatalf("handleCrossTenantBuild: %v", err)
	}

	select {
	case action := <-actions:
		want := "upstream:" + forwardCallerRunID + ":s1"
		if action.IntentKey != want {
			t.Fatalf("intent_key = %q, want the derived %s", action.IntentKey, want)
		}
		if action.RunID == forwardCallerRunID {
			t.Fatalf("a call with no key must mint its own run id, got the caller's %s", action.RunID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the trigger_run action")
	}
}

// TestCrossTenantBuild_MalformedSeqIsStillRefusedWithAKey asserts the
// seq format test keeps its place before the key is read.
func TestCrossTenantBuild_MalformedSeqIsStillRefusedWithAKey(t *testing.T) {
	t.Parallel()

	deps, _ := buildForwardTestDeps(t)

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

// TestCrossTenantBuild_ForwardKeepsTheCallerRunID asserts a forward
// triggers the home with the caller's run id and answers that id.
func TestCrossTenantBuild_ForwardKeepsTheCallerRunID(t *testing.T) {
	t.Parallel()

	deps, _ := buildForwardTestDeps(t)
	actions := subscribeLedgerActions(t, deps)
	supplied := "manual:" + forwardCallerRunID

	resp, err := handleCrossTenantBuild(context.Background(), deps, forwardRequest(supplied))
	if err != nil {
		t.Fatalf("handleCrossTenantBuild: %v", err)
	}
	if resp.ReasonCode != "" || resp.Code != "" {
		t.Fatalf("got reason=%q code=%q, want an accepted forward", resp.ReasonCode, resp.Code)
	}
	if resp.RunID != forwardCallerRunID {
		t.Fatalf("resp.RunID = %q, want the caller's %s", resp.RunID, forwardCallerRunID)
	}
	select {
	case action := <-actions:
		if action.RunID != forwardCallerRunID {
			t.Fatalf("action.RunID = %q, want the caller's %s", action.RunID, forwardCallerRunID)
		}
		if action.Controller != "cell-a" || action.Namespace != "ns-a" {
			t.Fatalf("action targets %s/%s, want the home ns-a/cell-a", action.Namespace, action.Controller)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the trigger_run action")
	}
}

// TestCrossTenantBuild_ForwardRejectsACallerRunIDThatIsNotUUIDv7 asserts
// a forward whose run id the ledger cannot claim answers INVALID_KEY and
// publishes nothing.
func TestCrossTenantBuild_ForwardRejectsACallerRunIDThatIsNotUUIDv7(t *testing.T) {
	t.Parallel()

	deps, _ := buildForwardTestDeps(t)
	actions := subscribeLedgerActions(t, deps)

	req := forwardRequest("cron:alpha/nightly:2026-09-17T03:00")
	req.CallerRunID = "caller-run-1"
	resp, err := handleCrossTenantBuild(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("handleCrossTenantBuild: %v", err)
	}
	if resp.ReasonCode != ReasonInvalidKey {
		t.Fatalf("reason_code = %q, want %q", resp.ReasonCode, ReasonInvalidKey)
	}
	assertNoLedgerAction(t, actions)
}

// TestCrossTenantBuild_ForwardToTheCallingCellAnswersNotReady asserts a
// forward whose home is the calling cell answers NOT_READY, so the
// claimer claims instead, and publishes nothing.
func TestCrossTenantBuild_ForwardToTheCallingCellAnswersNotReady(t *testing.T) {
	t.Parallel()

	deps, _ := buildForwardTestDeps(t)
	actions := subscribeLedgerActions(t, deps)

	req := forwardRequest("cron:alpha/nightly:2026-09-17T03:00")
	req.Cell = "cell-a"
	resp, err := handleCrossTenantBuild(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("handleCrossTenantBuild: %v", err)
	}
	if resp.ReasonCode != ReasonNotReady {
		t.Fatalf("reason_code = %q, want %q", resp.ReasonCode, ReasonNotReady)
	}
	assertNoLedgerAction(t, actions)
}

// TestCrossTenantBuild_ForwardToAnUnboundHomeAnswersNotReady asserts a
// forward whose home slot holds no cell answers NOT_READY, never the
// PENDING answer a claimer reads as a success.
func TestCrossTenantBuild_ForwardToAnUnboundHomeAnswersNotReady(t *testing.T) {
	t.Parallel()

	deps, db := buildForwardTestDeps(t)
	actions := subscribeLedgerActions(t, deps)
	seedTenantPlacementMembers(t, db, "alpha", 1, []member{
		{Cell: "cell-b", Namespace: "ns-a", Role: MemberRoleActive, Slot: slotPtr(1),
			Instance: "pod-b", RehydratedInstance: "pod-b"},
	}, map[string]int32{"alpha/nightly": 0})

	resp, err := handleCrossTenantBuild(context.Background(), deps, forwardRequest("cron:alpha/nightly:2026-09-17T03:00"))
	if err != nil {
		t.Fatalf("handleCrossTenantBuild: %v", err)
	}
	if resp.ReasonCode != ReasonNotReady || resp.State == CrossTenantStatePending {
		t.Fatalf("got reason=%q state=%q, want NOT_READY and no pending state", resp.ReasonCode, resp.State)
	}
	assertNoLedgerAction(t, actions)
}

// assertNoLedgerAction fails the test when an action arrives within half
// a second.
func assertNoLedgerAction(t *testing.T, actions <-chan LedgerAction) {
	t.Helper()
	select {
	case extra := <-actions:
		t.Fatalf("got action %+v, want none", extra)
	case <-time.After(500 * time.Millisecond):
	}
}
