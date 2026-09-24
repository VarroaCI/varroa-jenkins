package recordstore

import (
	"context"
	"testing"
	"time"
)

// TestReleaseConfirmedFleetLocks_PublishesOnePerRow asserts a cancel
// confirmation that deletes two releasing rows publishes two released
// notifications, each carrying that row's own job path.
func TestReleaseConfirmedFleetLocks_PublishesOnePerRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	now := time.Now().UTC()

	rows := []struct {
		name, runID, jobPath string
	}{
		{"alpha/deploy-prod", "run-1", "alpha/release-prod"},
		{"alpha/gate", "run-2", "alpha/release-gate"},
	}
	for _, row := range rows {
		insertLockRowForTenant(t, db, row.name, row.runID, "alpha", row.jobPath, FleetLockStateReleasing,
			now.Add(-10*time.Minute), now.Add(5*time.Minute))
		if _, err := db.Exec(`UPDATE varroa.fleet_locks SET cell_instance = 'pod-a' WHERE name = $1 AND run_id = $2`,
			row.name, row.runID); err != nil {
			t.Fatalf("set cell_instance: %v", err)
		}
	}

	// pod-a is gone: the tenant now runs on pod-b, and pod-a carries a
	// dead_instances entry older than the moved window.
	live := tenantPlacementLivenessRow{
		cellInstance:  "pod-b",
		cellAlive:     true,
		deadInstances: []TenantSyncDeadInstance{{Instance: "pod-a", LastSeenAt: now.Add(-6 * time.Minute)}},
	}

	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()

	released, err := ReleaseConfirmedFleetLocks(context.Background(), pool, testCluster, "alpha",
		live, now, testGrace, notifier, nil)
	if err != nil {
		t.Fatalf("ReleaseConfirmedFleetLocks: %v", err)
	}
	if released != len(rows) {
		t.Fatalf("released = %d, want %d", released, len(rows))
	}
	if got := sent.count(); got != len(rows) {
		t.Fatalf("published %d notifications, want %d", got, len(rows))
	}

	paths := map[string]string{}
	for i := 0; i < sent.count(); i++ {
		got := sent.payload(i)
		if got.Kind != "lock" || got.State != FleetLockStateReleased {
			t.Errorf("notification %d = kind %q state %q, want lock/released", i, got.Kind, got.State)
		}
		if got.Cluster != testCluster || got.Tenant != "alpha" {
			t.Errorf("notification %d coordinates = %s/%s, want %s/alpha", i, got.Cluster, got.Tenant, testCluster)
		}
		paths[got.RunID] = got.JobPath
	}
	for _, row := range rows {
		if paths[row.runID] != row.jobPath {
			t.Errorf("run %s published job path %q, want %q", row.runID, paths[row.runID], row.jobPath)
		}
	}

	// A repeat sweep finds no row, so it publishes nothing more.
	again, err := ReleaseConfirmedFleetLocks(context.Background(), pool, testCluster, "alpha",
		live, now, testGrace, notifier, nil)
	if err != nil {
		t.Fatalf("ReleaseConfirmedFleetLocks (repeat): %v", err)
	}
	if again != 0 {
		t.Fatalf("repeat released = %d, want 0", again)
	}
	if got := sent.count(); got != len(rows) {
		t.Fatalf("a repeat sweep published %d notifications in total, want %d", got, len(rows))
	}
}

// TestReleaseConfirmedFleetLocks_NilNotifierReleasesWithoutPublishing
// asserts a confirmation with no notifier still deletes its rows.
func TestReleaseConfirmedFleetLocks_NilNotifierReleasesWithoutPublishing(t *testing.T) {
	pool, db := setupLedgerPool(t)
	now := time.Now().UTC()

	insertLockRowForTenant(t, db, "alpha/deploy-prod", "run-1", "alpha", "alpha/release-prod",
		FleetLockStateReleasing, now.Add(-10*time.Minute), now.Add(5*time.Minute))
	if _, err := db.Exec(`UPDATE varroa.fleet_locks SET cell_instance = 'pod-a' WHERE name = $1 AND run_id = $2`,
		"alpha/deploy-prod", "run-1"); err != nil {
		t.Fatalf("set cell_instance: %v", err)
	}

	live := tenantPlacementLivenessRow{
		cellInstance:  "pod-b",
		cellAlive:     true,
		deadInstances: []TenantSyncDeadInstance{{Instance: "pod-a", LastSeenAt: now.Add(-6 * time.Minute)}},
	}
	released, err := ReleaseConfirmedFleetLocks(context.Background(), pool, testCluster, "alpha",
		live, now, testGrace, nil, nil)
	if err != nil {
		t.Fatalf("ReleaseConfirmedFleetLocks: %v", err)
	}
	if released != 1 {
		t.Fatalf("released = %d, want 1", released)
	}
}

// TestFleetLockRelease_ControllerTenantReleasesItsOwnLock asserts a
// non-cell controller releases its own lock by the bare name alone.
func TestFleetLockRelease_ControllerTenantReleasesItsOwnLock(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, controllerTenant, "team-pay", "ci", 0, "ci-0", "ci-0", nil)

	req := CrossTenantRequest{
		Cluster:       testCluster,
		Namespace:     "team-pay",
		Cell:          "ci",
		CallerTenant:  controllerTenant,
		CallerJobPath: "deploy",
		Name:          "deploy-prod",
		RunID:         "run-1",
	}
	if acquired, err := handleCrossTenantLockAcquire(context.Background(), deps, req); err != nil || acquired.State != FleetLockStateHeld {
		t.Fatalf("acquire: state %q, reason %q, err %v", acquired.State, acquired.ReasonCode, err)
	}

	resp, err := handleCrossTenantLockRelease(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("handleCrossTenantLockRelease: %v", err)
	}
	if resp.State != FleetLockStateReleased {
		t.Fatalf("state: got %q, want %q", resp.State, FleetLockStateReleased)
	}
	if _, found := readLockOwner(t, db, controllerTenant+"/deploy-prod"); found {
		t.Fatal("the release left the controller tenant's row in place")
	}
}

// TestFleetLockRelease_SlashInNameAnswersInvalidName asserts a release
// whose name holds a "/" is vetoed and deletes nothing.
func TestFleetLockRelease_SlashInNameAnswersInvalidName(t *testing.T) {
	deps, db := lockTestDeps(t)

	now := time.Now().UTC()
	insertFleetLockRow(t, db, "alpha/deploy-prod", "run-1", "held", now.Add(-time.Minute), now.Add(5*time.Minute))

	resp, err := handleCrossTenantLockRelease(context.Background(), deps, CrossTenantRequest{
		Cluster: testCluster, CallerTenant: "alpha", Name: "alpha/deploy-prod", RunID: "run-1",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockRelease: %v", err)
	}
	if resp.ReasonCode != ReasonInvalidName {
		t.Fatalf("reason_code: got %q, want %q", resp.ReasonCode, ReasonInvalidName)
	}
	if _, found := readLockOwner(t, db, "alpha/deploy-prod"); !found {
		t.Fatal("a vetoed release deleted the row")
	}
}

// TestFleetLockRelease_OtherTenantCannotRelease asserts a release from a
// tenant other than the row's own holder deletes nothing, even when it
// names the lock's owner and the holder's run id.
func TestFleetLockRelease_OtherTenantCannotRelease(t *testing.T) {
	deps, db := lockTestDeps(t)

	now := time.Now().UTC()
	insertFleetLockRow(t, db, "alpha/deploy-prod", "run-1", "held", now.Add(-time.Minute), now.Add(5*time.Minute))

	resp, err := handleCrossTenantLockRelease(context.Background(), deps, CrossTenantRequest{
		Cluster: testCluster, CallerTenant: "beta", TargetTenant: "alpha", Name: "deploy-prod", RunID: "run-1",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockRelease: %v", err)
	}
	if resp.State != FleetLockStateReleased {
		t.Fatalf("state: got %q, want %q", resp.State, FleetLockStateReleased)
	}
	if _, found := readLockOwner(t, db, "alpha/deploy-prod"); !found {
		t.Fatal("another tenant's release deleted alpha's row")
	}
}
