package recordstore

import (
	"context"
	"testing"
)

// TestGrantStampsPlacementIncarnation asserts a granted claim writes the
// placement row's incarnation onto its run_intents row.
func TestGrantStampsPlacementIncarnation(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedTenantPlacement(t, db, testTenant, testNamespace, "cell-a", 3, "pod-a", "pod-a", nil)
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET incarnation = 'uid-a'
		WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant); err != nil {
		t.Fatalf("set placement incarnation: %v", err)
	}
	ledger := NewLedger(pool, NewPlacementFence(pool, nil))

	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 3)
	if resp := ledger.Claim(context.Background(), req); resp.Decision != DecisionGranted {
		t.Fatalf("want granted, got %+v", resp)
	}

	var got string
	if err := db.QueryRow(`SELECT incarnation FROM varroa.run_intents WHERE intent_key = $1`,
		req.IntentKey).Scan(&got); err != nil {
		t.Fatalf("read intent incarnation: %v", err)
	}
	if got != "uid-a" {
		t.Fatalf("intent incarnation = %q, want uid-a", got)
	}
}

// TestRegrantFromNewIncarnationStampsNewIncarnation asserts that a repeat claim
// of an unstarted run, from a cell instance of a replacement incarnation,
// moves the run's incarnation to the replacement.
func TestRegrantFromNewIncarnationStampsNewIncarnation(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedTenantPlacement(t, db, testTenant, testNamespace, "cell-a", 0, "pod-a", "pod-a", nil)
	if _, err := db.Exec(`UPDATE varroa.tenant_placements SET incarnation = 'uid-a'
		WHERE cluster = $1 AND tenant = $2`, testCluster, testTenant); err != nil {
		t.Fatalf("set incarnation A: %v", err)
	}
	ledger := NewLedger(pool, NewPlacementFence(pool, nil))

	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	first := ledger.Claim(context.Background(), req)
	if first.Decision != DecisionGranted {
		t.Fatalf("first claim: want granted, got %+v", first)
	}

	// The replacement Tenant takes the name, and its cell instance announces.
	if _, err := db.Exec(`UPDATE varroa.tenant_placements
		SET incarnation = 'uid-b', cell_instance = 'pod-b', rehydrated_instance = 'pod-b'
		WHERE cluster = $1 AND tenant = $2`, testCluster, testTenant); err != nil {
		t.Fatalf("replace placement: %v", err)
	}

	regrant := req
	regrant.CellInstance = "pod-b"
	second := ledger.Claim(context.Background(), regrant)
	if second.Decision != DecisionGranted || second.RunID != first.RunID {
		t.Fatalf("re-claim from the new incarnation: want granted run %s, got %+v", first.RunID, second)
	}

	var incarnation, cellInstance string
	if err := db.QueryRow(`SELECT incarnation, cell_instance FROM varroa.run_intents WHERE intent_key = $1`,
		req.IntentKey).Scan(&incarnation, &cellInstance); err != nil {
		t.Fatalf("read intent: %v", err)
	}
	if incarnation != "uid-b" || cellInstance != "pod-b" {
		t.Fatalf("intent = (%q, %q), want (uid-b, pod-b)", incarnation, cellInstance)
	}
}
