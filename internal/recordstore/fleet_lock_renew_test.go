package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// readLockGeneration reads one fleet_locks row's stored generation.
func readLockGeneration(t *testing.T, db *sql.DB, name, runID string) int64 {
	t.Helper()
	var generation int64
	if err := db.QueryRow(`
		SELECT placement_generation FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2
	`, name, runID).Scan(&generation); err != nil {
		t.Fatalf("read the lock generation: %v", err)
	}
	return generation
}

// TestFleetLockRenew_GenerationRaiseKeepsALiveHold asserts a build that
// holds a lock across a slot takeover keeps it. The tenant generation
// rose after the acquire, and the renew still extends the lease and
// raises the lock's stored generation.
//
// A generation test would refuse this renew, and the build would lose a
// lock it still holds.
func TestFleetLockRenew_GenerationRaiseKeepsALiveHold(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 6, "pod-a", "pod-a", nil)

	now := time.Now().UTC()
	insertFleetLockRow(t, db, "alpha/deploy-prod", "run-1", "held", now.Add(-time.Minute), now.Add(5*time.Minute))
	if _, err := db.Exec(`UPDATE varroa.fleet_locks SET placement_generation = 3 WHERE name = $1 AND run_id = $2`,
		"alpha/deploy-prod", "run-1"); err != nil {
		t.Fatalf("stamp the acquire generation: %v", err)
	}

	resp, err := handleCrossTenantLockRenew(context.Background(), deps, CrossTenantRequest{
		Cluster:             testCluster,
		Namespace:           "ns-alpha",
		Cell:                "cell-alpha",
		CallerTenant:        "alpha",
		PlacementGeneration: 6,
		Name:                "deploy-prod",
		RunID:               "run-1",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockRenew: %v", err)
	}
	if resp.ReasonCode != "" {
		t.Fatalf("reason_code: got %q, want an extended lease (message %q)", resp.ReasonCode, resp.Message)
	}
	if resp.ExpiresAt == nil {
		t.Fatal("the renew answered no lease deadline")
	}
	if got := readLockGeneration(t, db, "alpha/deploy-prod", "run-1"); got != 6 {
		t.Fatalf("the stored generation is %d, want 6: a successful renew raises it", got)
	}
}

// TestFleetLockRenew_LowerCallerGenerationRefusesNothing asserts a caller
// whose resolver cache is behind still renews. The generation carries no
// identity the lock's own cell_instance does not already carry.
func TestFleetLockRenew_LowerCallerGenerationRefusesNothing(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 5, "pod-a", "pod-a", nil)

	now := time.Now().UTC()
	insertFleetLockRow(t, db, "alpha/deploy-prod", "run-1", "held", now.Add(-time.Minute), now.Add(5*time.Minute))
	if _, err := db.Exec(`UPDATE varroa.fleet_locks SET placement_generation = 5 WHERE name = $1 AND run_id = $2`,
		"alpha/deploy-prod", "run-1"); err != nil {
		t.Fatalf("stamp the acquire generation: %v", err)
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
	if resp.ReasonCode != "" {
		t.Fatalf("reason_code: got %q, want an extended lease", resp.ReasonCode)
	}
	if got := readLockGeneration(t, db, "alpha/deploy-prod", "run-1"); got != 5 {
		t.Fatalf("the stored generation is %d, want 5: a renew never lowers it", got)
	}
}

// TestFleetLockRenew_MovedTenantIsStillFenced asserts the placement
// orders still run first: a renew from a cell the tenant holds no member
// on answers the permanent FENCED, and the lease lapses.
func TestFleetLockRenew_MovedTenantIsStillFenced(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 5, "pod-a", "pod-a", nil)

	now := time.Now().UTC()
	insertFleetLockRow(t, db, "alpha/deploy-prod", "run-1", "held", now.Add(-time.Minute), now.Add(5*time.Minute))

	resp, err := handleCrossTenantLockRenew(context.Background(), deps, CrossTenantRequest{
		Cluster:             testCluster,
		Namespace:           "ns-alpha",
		Cell:                "cell-old",
		CallerTenant:        "alpha",
		PlacementGeneration: 5,
		Name:                "deploy-prod",
		RunID:               "run-1",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockRenew: %v", err)
	}
	if resp.ReasonCode != ReasonFenced {
		t.Fatalf("reason_code: got %q, want %q", resp.ReasonCode, ReasonFenced)
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
