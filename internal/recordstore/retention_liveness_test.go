package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"
)

// seedTenantPlacementLiveness inserts one tenant_placements row on
// testCluster/testTenant with the cell-liveness signal columns a
// PlacementLiveness test needs. It leaves the seed columns
// (rehydrated_instance, pending_jobs) at their defaults, because a
// liveness test never reads them.
func seedTenantPlacementLiveness(t *testing.T, db *sql.DB, cellInstance string,
	cellAlive bool, cellAliveAt, cellLastSeenAt *time.Time, dead []TenantSyncDeadInstance) {
	t.Helper()
	if dead == nil {
		dead = []TenantSyncDeadInstance{}
	}
	deadJSON, err := json.Marshal(dead)
	if err != nil {
		t.Fatalf("marshal dead instances: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements
			(cluster, tenant, cell_instance, cell_alive, cell_alive_at, cell_last_seen_at, dead_instances)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
	`, testCluster, testTenant, cellInstance, cellAlive, cellAliveAt, cellLastSeenAt, string(deadJSON)); err != nil {
		t.Fatalf("seed tenant placement liveness row: %v", err)
	}
}

// TestPlacementLivenessDeadInstanceGone asserts CellGone answers true for
// a run whose owning instance appears in dead_instances with a
// last_seen_at past the moved window, task 9.1b's first acceptance test.
func TestPlacementLivenessDeadInstanceGone(t *testing.T) {
	pool, db := setupLedgerPool(t)

	past := time.Now().Add(-6 * time.Minute)
	seedTenantPlacementLiveness(t, db, "pod-new", true, nil, nil,
		[]TenantSyncDeadInstance{{Instance: "pod-old", LastSeenAt: past}})

	liveness := NewPlacementLiveness(pool)
	oldHeartbeat := past
	gone, err := liveness.CellGone(context.Background(), RunLiveness{
		Cluster: testCluster, Tenant: testTenant, CellInstance: "pod-old", LastHeartbeatAt: &oldHeartbeat,
	})
	if err != nil {
		t.Fatalf("CellGone: %v", err)
	}
	if !gone {
		t.Fatal("want gone: the owning instance is confirmed dead past the moved window")
	}
}

// TestPlacementLivenessChangedInstanceAloneAnswersNotGone asserts CellGone
// answers false for a run whose instance changed but was never confirmed
// dead, task 9.1b's second acceptance test.
func TestPlacementLivenessChangedInstanceAloneAnswersNotGone(t *testing.T) {
	pool, db := setupLedgerPool(t)

	seedTenantPlacementLiveness(t, db, "pod-new", true, nil, nil, nil)

	liveness := NewPlacementLiveness(pool)
	oldHeartbeat := time.Now().Add(-20 * time.Minute)
	gone, err := liveness.CellGone(context.Background(), RunLiveness{
		Cluster: testCluster, Tenant: testTenant, CellInstance: "pod-old", LastHeartbeatAt: &oldHeartbeat,
	})
	if err != nil {
		t.Fatalf("CellGone: %v", err)
	}
	if gone {
		t.Fatal("want not gone: a changed placement alone is not a reading")
	}
}

// TestFinalizeStaleLogsSeesPlacementLivenessAnswer asserts C1's
// log-finalization sweep, driven by the placement-aware Liveness, closes
// a RUNNING run whose placement row reports its owning cell instance
// dead — the same answer the interrupted-run sweep would reach, task
// 9.1b's third acceptance test.
func TestFinalizeStaleLogsSeesPlacementLivenessAnswer(t *testing.T) {
	pool, db := setupLedgerPool(t)

	runID := startRunForFinalize(t, pool, "manual:"+newTestUUIDv7String())
	// startRunForFinalize's claim runs on testCluster/testTenant/cell-a/pod-a
	// (newTestClaimRequest's fixed coordinates). 17 minutes clears both the
	// silent reading's 15-minute window and stays inside the 20-minute
	// cell_alive_at staleness bound, with margin either side for the time
	// PlacementLiveness's own time.Now() call takes to run.
	past := time.Now().Add(-17 * time.Minute)
	seedTenantPlacementLiveness(t, db, "pod-a", false, &past, &past, nil)
	if _, err := db.Exec(`UPDATE varroa.runs SET last_heartbeat_at = $1 WHERE run_id = $2`, past, runID); err != nil {
		t.Fatalf("stamp last_heartbeat_at: %v", err)
	}

	liveness := NewPlacementLiveness(pool)
	finalized, err := finalizeStaleLogs(context.Background(), pool, liveness, time.Minute, time.Now())
	if err != nil {
		t.Fatalf("finalizeStaleLogs: %v", err)
	}
	if finalized != 1 {
		t.Fatalf("want one run finalized, got %d", finalized)
	}

	found, isFinalized := readLogSummaryFinalized(t, db, runID)
	if !found || !isFinalized {
		t.Fatalf("want the run's log summary finalized, found=%v finalized=%v", found, isFinalized)
	}
}
