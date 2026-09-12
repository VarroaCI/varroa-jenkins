package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// buildTestDeps opens a fresh schema, a pgxpool.Pool, and a connected
// bus.Conn over an embedded NATS server, and seeds one tenant_placements
// row for tenant at (cluster, namespace, cell, generation). It returns
// everything a build-kind test needs.
func buildTestDeps(t *testing.T) (CrossTenantDeps, *sql.DB) {
	tenant := "alpha"
	namespace := "ns-a"
	cell := "cell-a"
	generation := int64(1)
	t.Helper()
	pool, db := setupLedgerPool(t)
	seedTenantPlacement(t, db, tenant, namespace, cell, generation, "pod-a", "pod-a", nil)

	s := startLedgerActionTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	return CrossTenantDeps{
		Pool:      pool,
		Evaluator: NewGrantEvaluator(pool),
		Reader:    NewReader(pool),
		Conn:      conn,
	}, db
}

// subscribeLedgerActions subscribes to bus.LedgerActionSubject(cluster)
// and returns a channel of every action received during the test.
func subscribeLedgerActions(t *testing.T, deps CrossTenantDeps) <-chan LedgerAction {
	cluster := "core"
	t.Helper()
	received := make(chan LedgerAction, 8)
	sub, err := deps.Conn.Subscribe(bus.LedgerActionSubject(cluster), func(msg *nats.Msg) {
		var action LedgerAction
		if err := json.Unmarshal(msg.Data, &action); err == nil {
			received <- action
		}
	})
	if err != nil {
		t.Fatalf("subscribe ledger actions: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	deps.Conn.NATSConn().Flush()
	return received
}

// TestCrossTenantBuild_NoRowPublishesTriggerRunPending covers task 3.3's
// fourth acceptance: a reservation with no ledger row answers pending
// with an empty run_id, and its first acceptance: the published action
// carries actor svc-<caller-tenant>.
func TestCrossTenantBuild_NoRowPublishesTriggerRunPending(t *testing.T) {
	deps, _ := buildTestDeps(t)
	actions := subscribeLedgerActions(t, deps)

	req := CrossTenantRequest{
		Cluster:             "core",
		Namespace:           "ns-a",
		Cell:                "cell-a",
		CallerTenant:        "alpha",
		PlacementGeneration: 1,
		Kind:                CrossTenantKindBuild,
		CallerRunID:         "caller-run-1",
		Seq:                 "s1",
		TargetTenant:        "alpha",
		JobPath:             "release",
		Params:              json.RawMessage(`{"branch":"main"}`),
	}

	resp, err := handleCrossTenantBuild(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("handleCrossTenantBuild: %v", err)
	}
	if resp.State != CrossTenantStatePending {
		t.Fatalf("state: got %q, want %q", resp.State, CrossTenantStatePending)
	}
	if resp.RunID != "" {
		t.Fatalf("run_id: got %q, want empty", resp.RunID)
	}

	select {
	case action := <-actions:
		if action.Kind != LedgerActionTriggerRun {
			t.Fatalf("kind: got %q, want %q", action.Kind, LedgerActionTriggerRun)
		}
		if action.Actor != "svc-alpha" {
			t.Fatalf("actor: got %q, want %q", action.Actor, "svc-alpha")
		}
		if action.IntentKey != "upstream:caller-run-1:s1" {
			t.Fatalf("intent_key: got %q, want %q", action.IntentKey, "upstream:caller-run-1:s1")
		}
		if action.RunID == "" {
			t.Fatal("run_id: got empty, want a minted UUIDv7")
		}
		if action.JobPath != "alpha/release" {
			t.Fatalf("job_path: got %q, want the qualified %q (req.JobPath %q was folder relative to target_tenant %q)",
				action.JobPath, "alpha/release", req.JobPath, req.TargetTenant)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the published trigger_run action")
	}
}

// TestCrossTenantBuild_RelativeJobPathReachesExportedGrant covers the
// documented step call, varroa.build(tenant: 'platform', job:
// 'release'): the wire job_path arrives folder relative to
// target_tenant, and the recorder must qualify it before the grant
// check reaches platform's exported "release" job.
func TestCrossTenantBuild_RelativeJobPathReachesExportedGrant(t *testing.T) {
	deps, db := buildTestDeps(t)
	seedGrantPlacement(t, db, "core", "platform", "ns-p", "cell-p", 1,
		`{"exports":[{"job":"release","callers":["alpha"]}]}`)
	actions := subscribeLedgerActions(t, deps)

	req := CrossTenantRequest{
		Cluster:             "core",
		Namespace:           "ns-a",
		Cell:                "cell-a",
		CallerTenant:        "alpha",
		PlacementGeneration: 1,
		Kind:                CrossTenantKindBuild,
		CallerRunID:         "caller-run-relative",
		Seq:                 "s1",
		TargetTenant:        "platform",
		JobPath:             "release",
	}
	resp, err := handleCrossTenantBuild(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("handleCrossTenantBuild: %v", err)
	}
	if resp.ReasonCode != "" {
		t.Fatalf("reason_code: got %q, want none (the relative job path must reach the grant)", resp.ReasonCode)
	}
	if resp.State != CrossTenantStatePending {
		t.Fatalf("state: got %q, want %q", resp.State, CrossTenantStatePending)
	}

	select {
	case action := <-actions:
		if action.JobPath != "platform/release" {
			t.Fatalf("job_path: got %q, want the qualified %q", action.JobPath, "platform/release")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the published trigger_run action")
	}
}

// TestCrossTenantBuild_MalformedSeqRejected covers design.md D6's
// s<n> seq form: a request whose seq does not match it is refused with
// no grant lookup and no published action.
func TestCrossTenantBuild_MalformedSeqRejected(t *testing.T) {
	deps, _ := buildTestDeps(t)
	actions := subscribeLedgerActions(t, deps)

	tests := []string{"1", "s0", "s01", "s", "s-1", "S1"}
	for _, seq := range tests {
		req := CrossTenantRequest{
			Cluster:             "core",
			Namespace:           "ns-a",
			Cell:                "cell-a",
			CallerTenant:        "alpha",
			PlacementGeneration: 1,
			Kind:                CrossTenantKindBuild,
			CallerRunID:         "caller-run-seq",
			Seq:                 seq,
			TargetTenant:        "alpha",
			JobPath:             "release",
		}
		resp, err := handleCrossTenantBuild(context.Background(), deps, req)
		if err != nil {
			t.Fatalf("handleCrossTenantBuild(seq=%q): %v", seq, err)
		}
		if resp.Code != CodeInternal {
			t.Fatalf("seq=%q: code = %q, want %q", seq, resp.Code, CodeInternal)
		}
	}

	select {
	case action := <-actions:
		t.Fatalf("got an unexpected action for a malformed seq: %+v", action)
	case <-time.After(200 * time.Millisecond):
		// No action published, as a malformed request never reaches the grant check.
	}
}

// TestCrossTenantBuild_WritesNoRunIntentsRow covers task 3.3's second
// acceptance: the recorder writes no run_intents row of its own.
func TestCrossTenantBuild_WritesNoRunIntentsRow(t *testing.T) {
	deps, db := buildTestDeps(t)
	_ = subscribeLedgerActions(t, deps)

	req := CrossTenantRequest{
		Cluster:             "core",
		Namespace:           "ns-a",
		Cell:                "cell-a",
		CallerTenant:        "alpha",
		PlacementGeneration: 1,
		Kind:                CrossTenantKindBuild,
		CallerRunID:         "caller-run-2",
		Seq:                 "s1",
		TargetTenant:        "alpha",
		JobPath:             "release",
	}
	if _, err := handleCrossTenantBuild(context.Background(), deps, req); err != nil {
		t.Fatalf("handleCrossTenantBuild: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_intents WHERE intent_key = $1`, "upstream:caller-run-2:s1").Scan(&count); err != nil {
		t.Fatalf("count run_intents: %v", err)
	}
	if count != 0 {
		t.Fatalf("run_intents rows: got %d, want 0", count)
	}
}

// TestCrossTenantBuild_RepeatAfterClaimAnswersStoredRunID covers task
// 3.3's third acceptance: a request repeated after the target's claim
// created the row asserts no second action and the stored run_id.
func TestCrossTenantBuild_RepeatAfterClaimAnswersStoredRunID(t *testing.T) {
	deps, db := buildTestDeps(t)
	actions := subscribeLedgerActions(t, deps)

	intentKey := "upstream:caller-run-3:s1"
	storedRunID := seedGrantedIntent(t, deps.Pool, intentKey)

	req := CrossTenantRequest{
		Cluster:             "core",
		Namespace:           "ns-a",
		Cell:                "cell-a",
		CallerTenant:        "alpha",
		PlacementGeneration: 1,
		Kind:                CrossTenantKindBuild,
		CallerRunID:         "caller-run-3",
		Seq:                 "s1",
		TargetTenant:        "alpha",
		JobPath:             "release",
	}
	resp, err := handleCrossTenantBuild(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("handleCrossTenantBuild: %v", err)
	}
	if resp.RunID != storedRunID.String() {
		t.Fatalf("run_id: got %q, want %q", resp.RunID, storedRunID.String())
	}

	select {
	case action := <-actions:
		t.Fatalf("got an unexpected second action: %+v", action)
	case <-time.After(500 * time.Millisecond):
		// No second action, as design.md D6 requires.
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_intents WHERE intent_key = $1`, intentKey).Scan(&count); err != nil {
		t.Fatalf("count run_intents: %v", err)
	}
	if count != 1 {
		t.Fatalf("run_intents rows: got %d, want 1", count)
	}
}
