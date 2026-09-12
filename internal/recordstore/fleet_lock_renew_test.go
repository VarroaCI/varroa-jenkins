package recordstore

import (
	"context"
	"testing"
	"time"
)

// TestFleetLockRenew_StaleRowGenerationAnswersFenced covers task 3.10's
// first acceptance: a row whose stored placement_generation is below the
// current placement is refused, so a cell cannot renew a lease it took
// under an older generation.
func TestFleetLockRenew_StaleRowGenerationAnswersFenced(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 5, "pod-a", "pod-a", nil)

	now := time.Now().UTC()
	insertFleetLockRow(t, db, "alpha/deploy-prod", "run-1", "held", now.Add(-time.Minute), now.Add(5*time.Minute))
	if _, err := db.Exec(`UPDATE varroa.fleet_locks SET placement_generation = 3 WHERE name = $1 AND run_id = $2`,
		"alpha/deploy-prod", "run-1"); err != nil {
		t.Fatalf("stamp stale placement_generation: %v", err)
	}

	resp, err := handleCrossTenantLockRenew(context.Background(), deps, CrossTenantRequest{
		Cluster:             testCluster,
		Namespace:           "ns-alpha",
		Cell:                "cell-alpha",
		CallerTenant:        "alpha",
		PlacementGeneration: 5,
		Name:                "deploy-prod",
		RunID:               "run-1",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockRenew: %v", err)
	}
	if resp.ReasonCode != ReasonFenced {
		t.Fatalf("reason_code: got %q, want %q (message %q)", resp.ReasonCode, ReasonFenced, resp.Message)
	}
}

// TestFleetLockRenew_RevokedGrantAnswersCrossTenant covers task 3.10's
// second acceptance: a renew after the owning tenant revoked the
// cross-prefix grant is refused.
func TestFleetLockRenew_RevokedGrantAnswersCrossTenant(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)
	// beta exports nothing: the grant alpha once held is gone.
	seedTenantPlacement(t, db, "beta", "ns-beta", "cell-beta", 0, "pod-b", "pod-b", nil)

	now := time.Now().UTC()
	insertFleetLockRow(t, db, "beta/deploy-prod", "run-1", "held", now.Add(-time.Minute), now.Add(5*time.Minute))

	resp, err := handleCrossTenantLockRenew(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		Name:         "beta/deploy-prod",
		RunID:        "run-1",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockRenew: %v", err)
	}
	if resp.ReasonCode != ReasonCrossTenant {
		t.Fatalf("reason_code: got %q, want %q (message %q)", resp.ReasonCode, ReasonCrossTenant, resp.Message)
	}
}

// TestFleetLockRenew_NoMatchingRowAnswersLost covers task 3.10's third
// acceptance.
func TestFleetLockRenew_NoMatchingRowAnswersLost(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)

	resp, err := handleCrossTenantLockRenew(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		Name:         "deploy-prod",
		RunID:        "run-gone",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockRenew: %v", err)
	}
	if resp.State != FleetLockStateLost {
		t.Fatalf("state: got %q, want %q", resp.State, FleetLockStateLost)
	}
}

// TestFleetLockRelease_IsIdempotent covers task 3.10's fourth
// acceptance: a release answers released even when no row remains, and
// a repeated release changes nothing.
func TestFleetLockRelease_IsIdempotent(t *testing.T) {
	deps, db := lockTestDeps(t)

	now := time.Now().UTC()
	insertFleetLockRow(t, db, "alpha/deploy-prod", "run-1", "held", now.Add(-time.Minute), now.Add(5*time.Minute))

	first := handleCrossTenantLockReleaseForTest(t, deps, "run-1")
	if first.State != FleetLockStateReleased {
		t.Fatalf("first release state: got %q, want %q", first.State, FleetLockStateReleased)
	}

	second := handleCrossTenantLockReleaseForTest(t, deps, "run-1")
	if second.State != FleetLockStateReleased {
		t.Fatalf("second release state: got %q, want %q", second.State, FleetLockStateReleased)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2`,
		"alpha/deploy-prod", "run-1").Scan(&count); err != nil {
		t.Fatalf("count fleet_locks: %v", err)
	}
	if count != 0 {
		t.Fatalf("fleet_locks rows: got %d, want 0", count)
	}
}

// TestFleetLockRenew_ReleasingRowExtendsTheLease covers task 3.10's
// fifth acceptance: a renew of a releasing row succeeds, keeps its
// state, and extends the lease.
func TestFleetLockRenew_ReleasingRowExtendsTheLease(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)

	now := time.Now().UTC()
	originalExpiry := now.Add(30 * time.Second)
	insertFleetLockRow(t, db, "alpha/deploy-prod", "run-1", "releasing", now.Add(-time.Minute), originalExpiry)

	resp, err := handleCrossTenantLockRenew(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		Name:         "deploy-prod",
		RunID:        "run-1",
		LeaseSeconds: 300,
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockRenew: %v", err)
	}
	if resp.State != FleetLockStateReleasing {
		t.Fatalf("state: got %q, want %q", resp.State, FleetLockStateReleasing)
	}
	if resp.ExpiresAt == nil || !resp.ExpiresAt.After(originalExpiry) {
		t.Fatalf("expires_at: got %v, want an extension past %v", resp.ExpiresAt, originalExpiry)
	}
}
