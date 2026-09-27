package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// seedTenantPlacement inserts one varroa.tenant_placements row directly,
// so a fence test can control the stored placement without going
// through the tenant.sync responder.
func seedTenantPlacement(t *testing.T, db *sql.DB, tenant, namespace, cell string,
	generation int64, cellInstance, rehydratedInstance string, pendingJobs []string) {
	cluster := testCluster
	t.Helper()
	pendingJSON, err := json.Marshal(pendingJobs)
	if err != nil {
		t.Fatalf("marshal pending jobs: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements
			(cluster, tenant, namespace, cell, generation, cell_instance, rehydrated_instance, pending_jobs)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb)
	`, cluster, tenant, namespace, cell, generation, cellInstance, rehydratedInstance, string(pendingJSON)); err != nil {
		t.Fatalf("seed tenant placement: %v", err)
	}
}

// baselinePlacementRow answers a tenantPlacementFenceRow and a
// ClaimRequest that agree on every field, so a test can violate exactly
// one comparison and expect exactly one order to fire. The row carries an
// empty member list, which resolves to the synthetic slot-0 member: this
// is the single-member tenant every canonical scenario describes.
func baselinePlacementRow() (tenantPlacementFenceRow, ClaimRequest) {
	req := newTestClaimRequest("key", 3)
	row := tenantPlacementFenceRow{
		namespace:          testNamespace,
		cell:               "cell-a",
		generation:         3,
		cellInstance:       "pod-a",
		rehydratedInstance: "pod-a",
	}
	resolveFenceRowMembers(&row)
	return row, req
}

// resolveFenceRowMembers rebuilds a fence row's member view from its own
// scalar columns, the way scanTenantPlacementFenceRow does after a read.
// A test that mutates a scalar calls it again.
func resolveFenceRowMembers(row *tenantPlacementFenceRow) {
	resolved, err := decodePlacementMembers(nil, nil, placementScalars{
		cell: row.cell, namespace: row.namespace, cellInstance: row.cellInstance,
		rehydratedInstance: row.rehydratedInstance, pendingJobs: row.pendingJobs,
	})
	if err != nil {
		panic(err)
	}
	row.members = resolved
}

// twoSlotFenceRow answers a fence row for a tenant with two active slots
// and one standby, plus a home map that homes testJobPath on slot 0.
func twoSlotFenceRow(generation int64) tenantPlacementFenceRow {
	return tenantPlacementFenceRow{
		namespace: testNamespace, cell: "cell-a", generation: generation,
		cellInstance: "pod-a", rehydratedInstance: "pod-a",
		members: placementMembers{
			members: []member{
				{Cell: "cell-a", Namespace: testNamespace, Role: MemberRoleActive, Slot: slotPtr(0),
					Instance: "pod-a", RehydratedInstance: "pod-a"},
				{Cell: "cell-b", Namespace: testNamespace, Role: MemberRoleActive, Slot: slotPtr(1),
					Instance: "pod-b", RehydratedInstance: "pod-b"},
				{Cell: "cell-c", Namespace: testNamespace, Role: MemberRoleStandby,
					Instance: "pod-c", RehydratedInstance: "pod-c"},
			},
			jobHomes: map[string]int32{testJobPath: 0},
		},
	}
}

// TestDecidePlacementOrders asserts design D5's twelve-order decision
// table, one subtest per order, each violating exactly the comparison
// under test.
func TestDecidePlacementOrders(t *testing.T) {
	tests := []struct {
		name       string
		exists     bool
		row        func() tenantPlacementFenceRow
		mutateReq  func(*ClaimRequest)
		wantAllow  bool
		wantReason string
		wantMsg    string
	}{
		{
			name:       "order 1: no row",
			exists:     false,
			wantReason: ReasonNotReady,
			wantMsg:    "no placement row for tenant acme",
		},
		{
			name:   "order 2: no member in the request namespace",
			exists: true,
			row: func() tenantPlacementFenceRow {
				row, _ := baselinePlacementRow()
				row.namespace = "other-ns"
				resolveFenceRowMembers(&row)
				return row
			},
			wantReason: ReasonFenced,
			wantMsg:    "cell cell-a is not the placement of tenant acme",
		},
		{
			name:   "order 3: no member on the request cell",
			exists: true,
			row: func() tenantPlacementFenceRow {
				row, _ := baselinePlacementRow()
				row.cell = "cell-b"
				resolveFenceRowMembers(&row)
				return row
			},
			wantReason: ReasonFenced,
			wantMsg:    "cell cell-a is not the placement of tenant acme",
		},
		{
			name:   "order 4: no member names the request cell instance",
			exists: true,
			row: func() tenantPlacementFenceRow {
				row, _ := baselinePlacementRow()
				row.cellInstance = "pod-b"
				resolveFenceRowMembers(&row)
				return row
			},
			wantReason: ReasonNotReady,
			wantMsg:    "cell instance pod-a is not announced for tenant acme",
		},
		{
			name:   "order 5: the claiming member is a standby",
			exists: true,
			row: func() tenantPlacementFenceRow {
				row := twoSlotFenceRow(3)
				return row
			},
			mutateReq: func(req *ClaimRequest) {
				req.Cell = "cell-c"
				req.CellInstance = "pod-c"
			},
			wantReason: ReasonNotHome,
			wantMsg:    "cell cell-c is not the home of job acme/app; home slot is 0",
		},
		{
			name:   "order 6: the claiming slot is not the job's home",
			exists: true,
			row:    func() tenantPlacementFenceRow { return twoSlotFenceRow(3) },
			mutateReq: func(req *ClaimRequest) {
				req.Cell = "cell-b"
				req.CellInstance = "pod-b"
			},
			wantReason: ReasonNotHome,
			wantMsg:    "cell cell-b is not the home of job acme/app; home slot is 0",
		},
		{
			name:   "order 7: request generation below stored",
			exists: true,
			row: func() tenantPlacementFenceRow {
				row, _ := baselinePlacementRow()
				row.generation = 5
				return row
			},
			mutateReq: func(req *ClaimRequest) {
				req.PlacementGeneration = 3
			},
			wantReason: ReasonNotReady,
			wantMsg:    "placement generation 3 is stale for tenant acme",
		},
		{
			name:   "order 8: request generation above stored",
			exists: true,
			row: func() tenantPlacementFenceRow {
				row, _ := baselinePlacementRow()
				row.generation = 1
				return row
			},
			mutateReq: func(req *ClaimRequest) {
				req.PlacementGeneration = 3
			},
			wantReason: ReasonNotReady,
			wantMsg:    "placement generation 3 is stale for tenant acme",
		},
		{
			name:   "order 9: cross-tenant actor",
			exists: true,
			mutateReq: func(req *ClaimRequest) {
				req.Actor = "svc-other"
			},
			wantReason: ReasonCrossTenant,
			wantMsg:    crossTenantMessage,
		},
		{
			name:   "order 10: the home member's rehydrated instance differs",
			exists: true,
			row: func() tenantPlacementFenceRow {
				row, _ := baselinePlacementRow()
				row.rehydratedInstance = ""
				resolveFenceRowMembers(&row)
				return row
			},
			wantReason: ReasonNotReady,
			wantMsg:    "rehydration seed is not applied for job acme/app",
		},
		{
			name:   "order 11: the job path is pending on its home member",
			exists: true,
			row: func() tenantPlacementFenceRow {
				row, _ := baselinePlacementRow()
				row.pendingJobs = []string{testJobPath}
				resolveFenceRowMembers(&row)
				return row
			},
			wantReason: ReasonNotReady,
			wantMsg:    "rehydration seed is not applied for job acme/app",
		},
		{
			name:      "order 12: every comparison passes",
			exists:    true,
			wantAllow: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row, req := baselinePlacementRow()
			if tc.row != nil {
				row = tc.row()
			}
			if tc.mutateReq != nil {
				tc.mutateReq(&req)
			}

			got := decidePlacement(row, tc.exists, req)

			if got.Allow != tc.wantAllow {
				t.Fatalf("Allow = %v, want %v (result %+v)", got.Allow, tc.wantAllow, got)
			}
			if tc.wantAllow {
				return
			}
			if got.ReasonCode != tc.wantReason {
				t.Fatalf("ReasonCode = %q, want %q", got.ReasonCode, tc.wantReason)
			}
			if got.Message != tc.wantMsg {
				t.Fatalf("Message = %q, want %q", got.Message, tc.wantMsg)
			}
		})
	}
}

// TestCrossTenantVetoMessageIsExact asserts the cross-tenant-veto
// capability's message byte for byte.
func TestCrossTenantVetoMessageIsExact(t *testing.T) {
	row, req := baselinePlacementRow()
	req.Actor = "svc-other"

	got := decidePlacement(row, true, req)

	if got.ReasonCode != ReasonCrossTenant {
		t.Fatalf("ReasonCode = %q, want %q", got.ReasonCode, ReasonCrossTenant)
	}
	if got.Message != "cross-tenant triggers go through Varroa" {
		t.Fatalf("Message = %q, want the exact cross-tenant message", got.Message)
	}
}

// TestIsCrossTenantActor asserts the human, API-key, and mite identities
// pass, and a same-tenant svc- identity passes.
func TestIsCrossTenantActor(t *testing.T) {
	cases := []struct {
		actor  string
		tenant string
		want   bool
	}{
		{actor: "svc-alpha", tenant: "beta", want: true},
		{actor: "svc-alpha", tenant: "alpha", want: false},
		{actor: "alice", tenant: "beta", want: false},
		{actor: "varroa-mite", tenant: "beta", want: false},
	}
	for _, tc := range cases {
		if got := isCrossTenantActor(tc.actor, tc.tenant); got != tc.want {
			t.Errorf("isCrossTenantActor(%q, %q) = %v, want %v", tc.actor, tc.tenant, got, tc.want)
		}
	}
}

// TestPlacementFenceMissingRowVetoWritesNoIntentRow asserts a claim for
// a tenant with no stored placement is vetoed NOT_READY through the
// real PlacementFence, and writes no run_intents row.
func TestPlacementFenceMissingRowVetoWritesNoIntentRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, NewPlacementFence(pool, nil))

	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	resp := ledger.Claim(context.Background(), req)

	if resp.Decision != DecisionVetoed || resp.ReasonCode != ReasonNotReady {
		t.Fatalf("want vetoed/NOT_READY, got %+v", resp)
	}
	assertIntentRowCount(t, db, req.IntentKey, 0)
}

// TestPlacementFenceGrantsAMatchingClaim asserts a claim that matches a
// fully seeded placement row, with an applied seed and no pending job,
// is granted.
func TestPlacementFenceGrantsAMatchingClaim(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedTenantPlacement(t, db, testTenant, testNamespace, "cell-a", 3, "pod-a", "pod-a", nil)
	ledger := NewLedger(pool, NewPlacementFence(pool, nil))

	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 3)
	resp := ledger.Claim(context.Background(), req)

	if resp.Decision != DecisionGranted {
		t.Fatalf("want granted, got %+v", resp)
	}
}

// TestPlacementFenceCheckFailsClosedOnReadError asserts the real
// PlacementFence's Check answers an error, not a decision, when its
// database read fails.
func TestPlacementFenceCheckFailsClosedOnReadError(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	fence := NewPlacementFence(pool, nil)
	pool.Close()

	_, err := fence.Check(context.Background(), newTestClaimRequest("key", 0))
	if err == nil {
		t.Fatal("want an error from a closed pool, got nil")
	}
}

// TestPlacementFenceGrantsCrossTenantUpstreamClaim asserts a cross-tenant
// claim carrying an upstream: intent key and a matching export grant is
// granted, replacing decidePlacement's own permanent cross-tenant veto.
func TestPlacementFenceGrantsCrossTenantUpstreamClaim(t *testing.T) {
	pool, db := setupLedgerPool(t)
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements
			(cluster, tenant, namespace, cell, generation, cell_instance, policy, rehydrated_instance)
		VALUES ($1, $2, $3, 'cell-a', 0, 'pod-a', $4::jsonb, 'pod-a')
	`, testCluster, testTenant, testNamespace, `{"exports":[{"job":"app","callers":["alpha"]}]}`); err != nil {
		t.Fatalf("seed tenant placement with policy: %v", err)
	}
	ledger := NewLedger(pool, NewPlacementFence(pool, nil))

	req := newTestClaimRequest("upstream:"+newTestUUIDv7String()+":1", 0)
	req.Actor = "svc-alpha"
	resp := ledger.Claim(context.Background(), req)

	if resp.Decision != DecisionGranted {
		t.Fatalf("want granted, got %+v", resp)
	}
}

// TestPlacementFenceGrantedCrossTenantClaimStillWaitsForTheSeed asserts
// a granted cross-tenant claim still runs the seed orders: an unseeded
// tenant answers NOT_READY, not granted.
func TestPlacementFenceGrantedCrossTenantClaimStillWaitsForTheSeed(t *testing.T) {
	pool, db := setupLedgerPool(t)
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements
			(cluster, tenant, namespace, cell, generation, cell_instance, policy, rehydrated_instance)
		VALUES ($1, $2, $3, 'cell-a', 0, 'pod-a', $4::jsonb, 'pod-old')
	`, testCluster, testTenant, testNamespace, `{"exports":[{"job":"app","callers":["alpha"]}]}`); err != nil {
		t.Fatalf("seed tenant placement with policy: %v", err)
	}
	ledger := NewLedger(pool, NewPlacementFence(pool, nil))

	req := newTestClaimRequest("upstream:"+newTestUUIDv7String()+":1", 0)
	req.Actor = "svc-alpha"
	resp := ledger.Claim(context.Background(), req)

	if resp.Decision == DecisionGranted || resp.ReasonCode != ReasonNotReady {
		t.Fatalf("want NOT_READY for an unseeded tenant, got %+v", resp)
	}
}

// TestPlacementFenceRefusesUngrantedCrossTenantClaim asserts a
// cross-tenant claim with no matching export grant is still vetoed
// CROSS_TENANT, opening with the frozen message.
func TestPlacementFenceRefusesUngrantedCrossTenantClaim(t *testing.T) {
	pool, db := setupLedgerPool(t)
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements
			(cluster, tenant, namespace, cell, generation, cell_instance, rehydrated_instance)
		VALUES ($1, $2, $3, 'cell-a', 0, 'pod-a', 'pod-a')
	`, testCluster, testTenant, testNamespace); err != nil {
		t.Fatalf("seed tenant placement: %v", err)
	}
	ledger := NewLedger(pool, NewPlacementFence(pool, nil))

	req := newTestClaimRequest("upstream:"+newTestUUIDv7String()+":1", 0)
	req.Actor = "svc-alpha"
	resp := ledger.Claim(context.Background(), req)

	if resp.Decision != DecisionVetoed || resp.ReasonCode != ReasonCrossTenant {
		t.Fatalf("want vetoed/CROSS_TENANT, got %+v", resp)
	}
	if !strings.HasPrefix(resp.Message, crossTenantMessage) {
		t.Fatalf("Message = %q, want a prefix of %q", resp.Message, crossTenantMessage)
	}
}

// TestClaimFenceLockSerializesAgainstConcurrentAnnounce proves the fix
// for the race the epic review found: reading the placement row before
// the claim transaction opens let a placement announcement commit a
// newer generation between the read and the insert, granting a run
// against a generation the row had already left behind. CheckTx now
// reads the row with FOR SHARE inside the claim's own transaction, so a
// concurrent generation bump (tenant_sync.go's announce takes the same
// row FOR UPDATE) either lands before this read, or blocks until this
// transaction commits or rolls back. This test drives the second case
// directly: it holds the claim's row lock, proves a concurrent bump
// blocks on it, then commits the claim and confirms the bump only
// proceeds afterward.
func TestClaimFenceLockSerializesAgainstConcurrentAnnounce(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedTenantPlacement(t, db, testTenant, testNamespace, "cell-a", 3, "pod-a", "pod-a", nil)
	fence := NewPlacementFence(pool, nil)
	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 3)

	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin claim transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	result, err := fence.CheckTx(context.Background(), tx, req)
	if err != nil {
		t.Fatalf("CheckTx: %v", err)
	}
	if !result.Allow {
		t.Fatalf("want Allow, got %+v", result)
	}

	// The claim transaction now holds the placement row under FOR
	// SHARE. A concurrent generation bump, mimicking an announce's
	// FOR UPDATE write, must block on that lock.
	updateDone := make(chan error, 1)
	go func() {
		_, err := pool.Exec(context.Background(),
			`UPDATE varroa.tenant_placements SET generation = 4 WHERE cluster = $1 AND tenant = $2`,
			testCluster, testTenant)
		updateDone <- err
	}()

	select {
	case err := <-updateDone:
		t.Fatalf("want the concurrent generation bump to block on the held row lock, but it completed with err=%v", err)
	case <-time.After(300 * time.Millisecond):
		// Still blocked, as the fix requires.
	}

	runID, createdAt, err := parseRunUUIDv7(req.RunID)
	if err != nil {
		t.Fatalf("parse run id: %v", err)
	}
	if _, err := claimTx(context.Background(), tx, req, runID, createdAt); err != nil {
		t.Fatalf("claimTx: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit claim transaction: %v", err)
	}

	select {
	case err := <-updateDone:
		if err != nil {
			t.Fatalf("concurrent generation bump failed once unblocked: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent generation bump did not complete after the claim transaction committed")
	}

	// The end state is consistent: the claim committed at generation 3
	// before the bump to 4 was allowed to land, so a GRANTED run at the
	// old generation is not stale — it happened strictly before the
	// placement moved on.
	var storedGeneration int64
	if err := db.QueryRow(`SELECT generation FROM varroa.tenant_placements WHERE cluster = $1 AND tenant = $2`,
		testCluster, testTenant).Scan(&storedGeneration); err != nil {
		t.Fatalf("read stored generation: %v", err)
	}
	if storedGeneration != 4 {
		t.Fatalf("want stored generation 4, got %d", storedGeneration)
	}

	var placementGeneration int64
	var state string
	if err := db.QueryRow(`SELECT placement_generation, state FROM varroa.run_intents WHERE intent_key = $1`,
		req.IntentKey).Scan(&placementGeneration, &state); err != nil {
		t.Fatalf("read run intent: %v", err)
	}
	if state != stateGranted || placementGeneration != 3 {
		t.Fatalf("want a GRANTED intent recorded at generation 3, got state=%q generation=%d", state, placementGeneration)
	}
}
