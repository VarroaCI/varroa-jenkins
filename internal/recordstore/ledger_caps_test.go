package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

// seedCapPolicy writes a tenant placement row that carries the cap policy.
func seedCapPolicy(t *testing.T, db *sql.DB, policy any) {
	t.Helper()
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements
			(cluster, tenant, namespace, cell, generation, cell_instance, policy)
		VALUES ($1, $2, $3, 'cell-a', 0, 'pod-a', $4::jsonb)
	`, testCluster, testTenant, testNamespace, string(raw)); err != nil {
		t.Fatalf("seed cap policy: %v", err)
	}
}

// capRequest is a fresh claim for a job of the test tenant on cell.
func capRequest(cell string) ClaimRequest {
	req := newTestClaimRequest("manual:"+uuid.Must(uuid.NewV7()).String(), 0)
	req.Cell = cell
	req.JobPath = testTenant + "/build"
	return req
}

// consumeIntent ends a granted build, so it no longer holds a slot.
func capConsumeIntent(t *testing.T, db *sql.DB, runID string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE varroa.run_intents SET state = 'CONSUMED', consumed_at = now() WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("consume intent: %v", err)
	}
}

// waitRowCount counts the wait rows of one run.
func waitRowCount(t *testing.T, db *sql.DB, runID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_waits WHERE run_id = $1::uuid`, runID).Scan(&n); err != nil {
		t.Fatalf("count wait rows: %v", err)
	}
	return n
}

var capTenantLimitOne = map[string]any{
	"concurrencyLimits": []map[string]any{{"name": "builds", "jobs": []string{"*"}, "max": 1}},
}

func TestClaim_TenantLimitVetoesCappedAndWritesWait(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedCapPolicy(t, db, capTenantLimitOne)
	ledger := NewLedger(pool, nil)

	first := capRequest("cell-a")
	mustGrant(t, ledger, first)

	second := capRequest("cell-a")
	resp := ledger.Claim(context.Background(), second)
	if resp.Decision != DecisionVetoed || resp.ReasonCode != ReasonCapped {
		t.Fatalf("second claim = %+v, want vetoed CAPPED", resp)
	}
	if got := waitRowCount(t, db, second.RunID); got != 1 {
		t.Fatalf("wait rows = %d, want 1", got)
	}
	if n := intentCount(t, db, second.IntentKey); n != 0 {
		t.Fatalf("a capped veto wrote %d intents, want 0", n)
	}
}

func TestClaim_CappedWaiterGrantedAfterRunCompletes(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedCapPolicy(t, db, capTenantLimitOne)
	ledger := NewLedger(pool, nil)

	first := capRequest("cell-a")
	mustGrant(t, ledger, first)
	waiting := capRequest("cell-a")
	if resp := ledger.Claim(context.Background(), waiting); resp.ReasonCode != ReasonCapped {
		t.Fatalf("waiting claim = %+v, want CAPPED", resp)
	}
	capConsumeIntent(t, db, first.RunID)

	mustGrant(t, ledger, waiting)
	if got := waitRowCount(t, db, waiting.RunID); got != 0 {
		t.Fatalf("wait rows after the grant = %d, want 0", got)
	}
}

func TestClaim_RepeatClaimOfGrantedIntentSkipsCap(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedCapPolicy(t, db, capTenantLimitOne)
	ledger := NewLedger(pool, nil)

	first := capRequest("cell-a")
	mustGrant(t, ledger, first)
	resp := ledger.Claim(context.Background(), first)
	if resp.ReasonCode == ReasonCapped {
		t.Fatalf("a repeat of a granted claim must not wait on the cap, got %+v", resp)
	}
}

func TestClaim_CellCapCountsPerCell(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedCapPolicy(t, db, map[string]any{"cellMaxRuns": 1})
	ledger := NewLedger(pool, nil)

	mustGrant(t, ledger, capRequest("cell-a"))
	mustGrant(t, ledger, capRequest("cell-b"))
	resp := ledger.Claim(context.Background(), capRequest("cell-a"))
	if resp.ReasonCode != ReasonCapped {
		t.Fatalf("a second build on cell-a = %+v, want CAPPED", resp)
	}
}

func TestClaim_ExpiredGrantHoldsNoSlot(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedCapPolicy(t, db, capTenantLimitOne)
	ledger := NewLedger(pool, nil)

	old := capRequest("cell-a")
	mustGrant(t, ledger, old)
	if _, err := db.Exec(`UPDATE varroa.run_intents SET granted_at = now() - interval '2 hours' WHERE run_id = $1::uuid`,
		old.RunID); err != nil {
		t.Fatalf("age grant: %v", err)
	}
	mustGrant(t, ledger, capRequest("cell-a"))
}

func TestClaim_ConcurrentClaimsNeverExceedLimit(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedCapPolicy(t, db, map[string]any{
		"concurrencyLimits": []map[string]any{{"name": "pair", "jobs": []string{"*"}, "max": 2}},
	})
	ledger := NewLedger(pool, nil)

	var granted atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp := ledger.Claim(context.Background(), capRequest("cell-a")); resp.Decision == DecisionGranted {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()
	if granted.Load() != 2 {
		t.Fatalf("granted = %d, want exactly 2", granted.Load())
	}
}

func TestClaim_OlderWaiterWinsFreedSlot(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedCapPolicy(t, db, capTenantLimitOne)
	ledger := NewLedger(pool, nil)

	holder := capRequest("cell-a")
	mustGrant(t, ledger, holder)
	older := capRequest("cell-a")
	if resp := ledger.Claim(context.Background(), older); resp.ReasonCode != ReasonCapped {
		t.Fatalf("older waiter = %+v, want CAPPED", resp)
	}
	newer := capRequest("cell-a")
	if resp := ledger.Claim(context.Background(), newer); resp.ReasonCode != ReasonCapped {
		t.Fatalf("newer waiter = %+v, want CAPPED", resp)
	}
	capConsumeIntent(t, db, holder.RunID)

	if resp := ledger.Claim(context.Background(), newer); resp.Decision == DecisionGranted {
		t.Fatalf("the newer waiter must not take the slot ahead of the older one, got %+v", resp)
	}
	mustGrant(t, ledger, older)
}

func TestClaim_UncappedTenantWritesNoWait(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedCapPolicy(t, db, map[string]any{})
	ledger := NewLedger(pool, nil)

	req := capRequest("cell-a")
	mustGrant(t, ledger, req)
	if n := waitRowCount(t, db, req.RunID); n != 0 {
		t.Fatalf("an uncapped tenant wrote %d wait rows", n)
	}
}

// intentCount counts the run intents that hold one intent key.
func intentCount(t *testing.T, db *sql.DB, key string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_intents WHERE intent_key = $1`, key).Scan(&n); err != nil {
		t.Fatalf("count intents: %v", err)
	}
	return n
}

// TestClaim_TransferToFullCellIsCapped asserts that a repeat claim that moves an
// existing grant to another cell is checked against that cell's cap.
func TestClaim_TransferToFullCellIsCapped(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedCapPolicy(t, db, map[string]any{"cellMaxRuns": 1})
	ledger := NewLedger(pool, nil)

	moving := capRequest("cell-a")
	mustGrant(t, ledger, moving)
	mustGrant(t, ledger, capRequest("cell-b"))

	transfer := moving
	transfer.Cell = "cell-b"
	resp := ledger.Claim(context.Background(), transfer)
	if resp.Decision == DecisionGranted || resp.ReasonCode != ReasonCapped {
		t.Fatalf("transfer onto a full cell = %+v, want CAPPED", resp)
	}
}
