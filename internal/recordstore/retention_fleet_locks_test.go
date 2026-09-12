package recordstore

import (
	"context"
	"testing"
	"time"
)

// TestSweepFleetLocks_ExpiredRowIsDeleted asserts task 4.4's first
// acceptance test: an ordinary expired row is deleted.
func TestSweepFleetLocks_ExpiredRowIsDeleted(t *testing.T) {
	pool, db := setupLedgerPool(t)

	now := time.Now()
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, newTestUUIDv7String(), FleetLockStateHeld,
		now.Add(-20*time.Minute), now.Add(-time.Minute))

	moved, expired, err := sweepFleetLocks(context.Background(), pool, now)
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
	pool, db := setupLedgerPool(t)

	now := time.Now()
	runID := newTestUUIDv7String()
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, runID, FleetLockStateHeld,
		now.Add(-20*time.Minute), now.Add(5*time.Minute))

	_, expired, err := sweepFleetLocks(context.Background(), pool, now)
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
	pool, db := setupLedgerPool(t)

	now := time.Now()
	runID := newTestUUIDv7String()
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, runID, FleetLockStateReleasing,
		now.Add(-20*time.Minute), now.Add(-time.Minute))

	_, expired, err := sweepFleetLocks(context.Background(), pool, now)
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

	reader := fanoutTestMeter(t)
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
