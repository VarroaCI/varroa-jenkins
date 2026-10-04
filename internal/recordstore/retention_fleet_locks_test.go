package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// insertLockRowForTenant inserts one fleet_locks row whose own tenant,
// job path, state, and expiry a test chooses, for the sweep's own
// notification and survival cases.
func insertLockRowForTenant(t *testing.T, db *sql.DB, name, runID, tenant, jobPath, state string,
	waitingSince, expiresAt time.Time) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.fleet_locks (name, owner_tenant, run_id, cluster, tenant, job_path, state, waiting_since, expires_at)
		VALUES ($1, split_part($1, '/', 1), $2, $3, $4, $5, $6, $7, $8)
	`, name, runID, testCluster, tenant, jobPath, state, waitingSince, expiresAt); err != nil {
		t.Fatalf("insert fleet lock %s/%s at %s: %v", name, runID, tenant, err)
	}
}

// TestSweepFleetLocks_ExpiredRowIsDeleted asserts task 4.4's first
// acceptance test: an ordinary expired row is deleted.
func TestSweepFleetLocks_ExpiredRowIsDeleted(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	now := time.Now()
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, newTestUUIDv7String(), FleetLockStateHeld,
		now.Add(-20*time.Minute), now.Add(-time.Minute))

	moved, expired, err := sweepFleetLocks(context.Background(), pool, now, nil)
	if err != nil {
		t.Fatalf("sweepFleetLocks: %v", err)
	}
	if moved != 0 {
		t.Fatalf("moved = %d, want 0: this row's run is not INTERRUPTED", moved)
	}
	if expired != 1 {
		t.Fatalf("expired = %d, want 1", expired)
	}
}

// TestSweepFleetLocks_RenewedRowIsKept asserts task 4.4's second
// acceptance test: a row whose lease was renewed, and so has not
// expired, survives the round.
func TestSweepFleetLocks_RenewedRowIsKept(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	now := time.Now()
	runID := newTestUUIDv7String()
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, runID, FleetLockStateHeld,
		now.Add(-20*time.Minute), now.Add(5*time.Minute))

	_, expired, err := sweepFleetLocks(context.Background(), pool, now, nil)
	if err != nil {
		t.Fatalf("sweepFleetLocks: %v", err)
	}
	if expired != 0 {
		t.Fatalf("expired = %d, want 0", expired)
	}
	if _, _, found := fleetLockRowState(t, db, runID); !found {
		t.Fatal("a renewed row must survive the round")
	}
}

// TestSweepFleetLocks_ExpiredReleasingRowIsKept asserts task 4.4's third
// acceptance test: the sweep skips a releasing row, whatever its lease.
func TestSweepFleetLocks_ExpiredReleasingRowIsKept(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	now := time.Now()
	runID := newTestUUIDv7String()
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, runID, FleetLockStateReleasing,
		now.Add(-20*time.Minute), now.Add(-time.Minute))

	_, expired, err := sweepFleetLocks(context.Background(), pool, now, nil)
	if err != nil {
		t.Fatalf("sweepFleetLocks: %v", err)
	}
	if expired != 0 {
		t.Fatalf("expired = %d, want 0: the sweep must skip a releasing row", expired)
	}
	if state, _, found := fleetLockRowState(t, db, runID); !found || state != FleetLockStateReleasing {
		t.Fatalf("state = %q (found=%v), want releasing", state, found)
	}
}

// TestSweepFleetLocks_MetricsCountedThroughMaintenanceRound asserts the
// expired counter increases through the full RunMaintenanceRound path,
// under the maintenance advisory lock, not only through sweepFleetLocks
// called directly.
func TestSweepFleetLocks_MetricsCountedThroughMaintenanceRound(t *testing.T) {
	pool, db := setupLedgerPool(t)

	now := time.Now()
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, newTestUUIDv7String(), FleetLockStateHeld,
		now.Add(-20*time.Minute), now.Add(-time.Minute))

	reader := useTestMeterProvider(t)
	metrics, err := NewFleetLockMetrics(pool)
	if err != nil {
		t.Fatalf("new fleet lock metrics: %v", err)
	}

	cfg := MaintenanceConfig{
		RunRetention:      365 * 24 * time.Hour,
		LogRetention:      365 * 24 * time.Hour,
		ArtifactRetention: 365 * 24 * time.Hour,
		ActivityRetention: 30 * 24 * time.Hour,
		FleetLockMetrics:  metrics,
		Now:               func() time.Time { return now },
	}
	ran, err := RunMaintenanceRound(context.Background(), pool, cfg)
	if err != nil {
		t.Fatalf("RunMaintenanceRound: %v", err)
	}
	if !ran {
		t.Fatal("want the round to run")
	}

	if got := fanoutMetricSum(t, reader, "recorder_fleet_lock_expired_total", nil); got != 1 {
		t.Fatalf("recorder_fleet_lock_expired_total = %d, want 1", got)
	}
}

// TestSweepFleetLocks_MovePublishesOneNotificationPerMovedRow asserts a
// move across three tenants publishes three releasing notifications,
// each carrying that row's own tenant and job path.
func TestSweepFleetLocks_MovePublishesOneNotificationPerMovedRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	now := time.Now()
	tenants := []string{"alpha", "beta", "gamma"}
	for _, tenant := range tenants {
		runID := newTestUUIDv7String()
		insertInterruptedRun(t, db, runID)
		// One lock name per tenant: the one-holder index admits one held
		// row per name.
		insertLockRowForTenant(t, db, tenant+"/deploy-prod", runID, tenant,
			tenant+"/release", FleetLockStateHeld, now.Add(-time.Minute), now.Add(time.Hour))
	}

	n, sent, _ := newCaptureNotifier(t)
	defer n.Close()

	moved, expired, err := sweepFleetLocks(context.Background(), pool, now, n)
	if err != nil {
		t.Fatalf("sweepFleetLocks: %v", err)
	}
	if moved != len(tenants) || expired != 0 {
		t.Fatalf("moved = %d, expired = %d, want %d moved and none expired", moved, expired, len(tenants))
	}
	if got := sent.count(); got != len(tenants) {
		t.Fatalf("published %d notifications, want %d", got, len(tenants))
	}
	seen := map[string]string{}
	for i := 0; i < sent.count(); i++ {
		got := sent.payload(i)
		if got.Kind != "lock" || got.State != FleetLockStateReleasing {
			t.Errorf("notification %d = kind %q state %q, want lock/releasing", i, got.Kind, got.State)
		}
		seen[got.Tenant] = got.JobPath
	}
	for _, tenant := range tenants {
		if seen[tenant] != tenant+"/release" {
			t.Errorf("tenant %s published %q, want its own job path", tenant, seen[tenant])
		}
	}
}

// TestSweepFleetLocks_PublishesOneNotificationPerDeletedRow asserts an
// expired row of each of two tenants publishes two released
// notifications in one batch, each naming its own row.
func TestSweepFleetLocks_PublishesOneNotificationPerDeletedRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	now := time.Now()
	insertLockRowForTenant(t, db, fleetLockStateMachineTestName, newTestUUIDv7String(), "alpha",
		"alpha/release", FleetLockStateWaiting, now.Add(-time.Hour), now.Add(-time.Minute))
	insertLockRowForTenant(t, db, fleetLockStateMachineTestName, newTestUUIDv7String(), "beta",
		"beta/release", FleetLockStateWaiting, now.Add(-time.Hour), now.Add(-time.Minute))

	n, sent, _ := newCaptureNotifier(t)
	defer n.Close()

	moved, expired, err := sweepFleetLocks(context.Background(), pool, now, n)
	if err != nil {
		t.Fatalf("sweepFleetLocks: %v", err)
	}
	if moved != 0 || expired != 2 {
		t.Fatalf("moved = %d, expired = %d, want 0 moved and 2 expired", moved, expired)
	}
	if got := sent.count(); got != 2 {
		t.Fatalf("published %d notifications, want 2", got)
	}
	seen := map[string]string{}
	for i := 0; i < sent.count(); i++ {
		got := sent.payload(i)
		if got.Kind != "lock" || got.State != FleetLockStateReleased {
			t.Errorf("notification %d = kind %q state %q, want lock/released", i, got.Kind, got.State)
		}
		seen[got.Tenant] = got.LockName
	}
	if seen["alpha"] != fleetLockStateMachineTestName || seen["beta"] != fleetLockStateMachineTestName {
		t.Fatalf("published rows = %v, want both tenants' own rows", seen)
	}
}

// TestSweepFleetLocks_ReleasingRowPublishesNothing asserts a releasing
// row survives the sweep, whatever its lease, and publishes nothing,
// because its state does not change.
func TestSweepFleetLocks_ReleasingRowPublishesNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	now := time.Now()
	runID := newTestUUIDv7String()
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, runID, FleetLockStateReleasing,
		now.Add(-20*time.Minute), now.Add(-time.Minute))

	n, sent, _ := newCaptureNotifier(t)
	defer n.Close()

	moved, expired, err := sweepFleetLocks(context.Background(), pool, now, n)
	if err != nil {
		t.Fatalf("sweepFleetLocks: %v", err)
	}
	if moved != 0 || expired != 0 {
		t.Fatalf("moved = %d, expired = %d, want 0 and 0", moved, expired)
	}
	if got := sent.count(); got != 0 {
		t.Fatalf("published %d notifications for a releasing row, want 0", got)
	}
	if state, _, found := fleetLockRowState(t, db, runID); !found || state != FleetLockStateReleasing {
		t.Fatalf("state = %q (found=%v), want releasing", state, found)
	}
}

// TestSweepFleetLocks_NilNotifierStillDeletesRows asserts a maintenance
// round with no LockNotifier completes and deletes the same rows.
func TestSweepFleetLocks_NilNotifierStillDeletesRows(t *testing.T) {
	pool, db := setupLedgerPool(t)

	now := time.Now()
	runID := newTestUUIDv7String()
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, runID, FleetLockStateHeld,
		now.Add(-20*time.Minute), now.Add(-time.Minute))

	reader := useTestMeterProvider(t)
	metrics, err := NewFleetLockMetrics(pool)
	if err != nil {
		t.Fatalf("new fleet lock metrics: %v", err)
	}

	cfg := MaintenanceConfig{
		RunRetention:      365 * 24 * time.Hour,
		LogRetention:      365 * 24 * time.Hour,
		ArtifactRetention: 365 * 24 * time.Hour,
		ActivityRetention: 30 * 24 * time.Hour,
		FleetLockMetrics:  metrics,
		Now:               func() time.Time { return now },
	}
	ran, err := RunMaintenanceRound(context.Background(), pool, cfg)
	if err != nil {
		t.Fatalf("RunMaintenanceRound: %v", err)
	}
	if !ran {
		t.Fatal("want the round to run")
	}
	if got := fanoutMetricSum(t, reader, "recorder_fleet_lock_expired_total", nil); got != 1 {
		t.Fatalf("recorder_fleet_lock_expired_total = %d, want 1", got)
	}
	if _, _, found := fleetLockRowState(t, db, runID); found {
		t.Fatal("a round with no notifier must still delete the expired row")
	}
}
