package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// fleetLockStateMachineTestName is the qualified lock name every test in
// this file exercises, unless a test needs a second name.
const fleetLockStateMachineTestName = "acme/deploy-prod"

// fleetLockRowState reads runID's own fleet_locks row of
// fleetLockStateMachineTestName, state and releasing_since, or reports
// found=false when the row is gone.
func fleetLockRowState(t *testing.T, db *sql.DB, runID string) (state string, releasingSince sql.NullTime, found bool) {
	t.Helper()
	err := db.QueryRow(`
		SELECT state, releasing_since FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2
	`, fleetLockStateMachineTestName, runID).Scan(&state, &releasingSince)
	switch {
	case err == sql.ErrNoRows:
		return "", sql.NullTime{}, false
	case err != nil:
		t.Fatalf("read fleet lock row %s/%s: %v", fleetLockStateMachineTestName, runID, err)
	}
	return state, releasingSince, true
}

// TestFleetLockStateMachine_MarkingWritesNoRow asserts task 4.3's first
// acceptance test: the interrupted-run marking transaction touches no
// fleet_locks row of the run it marks.
func TestFleetLockStateMachine_MarkingWritesNoRow(t *testing.T) {
	pool, db := setupLedgerPool(t)

	req := newTestClaimRequest("manual:sm-marking", 0)
	runID := seedRunningRun(t, pool, req)
	candidate := fetchRunningCandidate(t, pool, runID)

	insertFleetLockRow(t, db, fleetLockStateMachineTestName, runID.String(), FleetLockStateHeld,
		time.Now().Add(-time.Minute), time.Now().Add(fleetLockDefaultLeaseSeconds*time.Second))

	changed, err := markInterrupted(context.Background(), pool, candidate, ReasonNoHeartbeat, time.Now())
	if err != nil {
		t.Fatalf("markInterrupted: %v", err)
	}
	if !changed {
		t.Fatal("want the marking update to change the run")
	}

	state, _, found := fleetLockRowState(t, db, runID.String())
	if !found {
		t.Fatal("the marking transaction deleted the fleet_locks row; it must touch none")
	}
	if state != FleetLockStateHeld {
		t.Fatalf("state = %q, want held: the marking transaction must not touch this row", state)
	}
}

// TestFleetLockStateMachine_MoveLeavesReleasingPromotesNoWaiter asserts
// task 4.3's second acceptance test: the follow-up transaction moves a
// held row of an interrupted run to releasing, stamps releasing_since,
// deletes nothing, and promotes no waiter.
func TestFleetLockStateMachine_MoveLeavesReleasingPromotesNoWaiter(t *testing.T) {
	pool, db := setupLedgerPool(t)

	holderRunID := newTestUUIDv7String()
	waiterRunID := newTestUUIDv7String()
	insertInterruptedRun(t, db, holderRunID)

	now := time.Now()
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, holderRunID, FleetLockStateHeld,
		now.Add(-time.Minute), now.Add(5*time.Minute))
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, waiterRunID, FleetLockStateWaiting,
		now.Add(-30*time.Second), now.Add(5*time.Minute))

	moved, err := moveInterruptedHoldersToReleasing(context.Background(), pool, fleetLockStateMachineTestName, now)
	if err != nil {
		t.Fatalf("moveInterruptedHoldersToReleasing: %v", err)
	}
	if moved != 1 {
		t.Fatalf("moved = %d, want 1", moved)
	}

	state, releasingSince, found := fleetLockRowState(t, db, holderRunID)
	if !found || state != FleetLockStateReleasing {
		t.Fatalf("holder state = %q (found=%v), want releasing", state, found)
	}
	if !releasingSince.Valid {
		t.Fatal("releasing_since was not stamped")
	}

	waiterState, _, found := fleetLockRowState(t, db, waiterRunID)
	if !found || waiterState != FleetLockStateWaiting {
		t.Fatalf("waiter state = %q (found=%v), want waiting: the move must promote nobody", waiterState, found)
	}
}

// TestFleetLockStateMachine_TerminalEventDeletesReleasingRow asserts
// task 4.3's acceptance: an ingested terminal run.completed for the
// holder deletes a releasing row.
func TestFleetLockStateMachine_TerminalEventDeletesReleasingRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:sm-terminal-releasing")

	now := time.Now()
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, runID.String(), FleetLockStateReleasing,
		now.Add(-5*time.Minute), now.Add(5*time.Minute))

	g := NewIngester(pool, nil, nil)
	env := newTestEnvelope(t, runID, "manual:sm-terminal-releasing", KindRunCompleted,
		RunCompletedPayload{Result: strPtr("SUCCESS")})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	if _, _, found := fleetLockRowState(t, db, runID.String()); found {
		t.Fatal("a terminal event must delete a releasing row of its holder")
	}
}

// TestFleetLockStateMachine_TerminalEventDeletesHeldRow asserts task
// 4.3's acceptance: an ingested terminal run.completed for the holder
// deletes a held row exactly as before.
func TestFleetLockStateMachine_TerminalEventDeletesHeldRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:sm-terminal-held")

	now := time.Now()
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, runID.String(), FleetLockStateHeld,
		now.Add(-time.Minute), now.Add(5*time.Minute))

	g := NewIngester(pool, nil, nil)
	env := newTestEnvelope(t, runID, "manual:sm-terminal-held", KindRunCompleted,
		RunCompletedPayload{Result: strPtr("SUCCESS")})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	if _, _, found := fleetLockRowState(t, db, runID.String()); found {
		t.Fatal("a terminal event must delete a held row of its holder")
	}
}

// TestFleetLockStateMachine_DeadInstanceEntryReleasesRow asserts task
// 4.3's acceptance: a dead_instances entry older than the window
// releases a releasing row.
func TestFleetLockStateMachine_DeadInstanceEntryReleasesRow(t *testing.T) {
	pool, db := setupLedgerPool(t)

	runID := newTestUUIDv7String()
	insertInterruptedRun(t, db, runID)
	now := time.Now()
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, runID, FleetLockStateReleasing,
		now.Add(-10*time.Minute), now.Add(5*time.Minute))
	if _, err := db.Exec(`UPDATE varroa.fleet_locks SET cell_instance = 'pod-a' WHERE name = $1 AND run_id = $2`,
		fleetLockStateMachineTestName, runID); err != nil {
		t.Fatalf("set cell_instance: %v", err)
	}

	staleSeen := now.Add(-6 * time.Minute) // older than the 5-minute moved window
	row := tenantPlacementLivenessRow{
		cellInstance:  "pod-b",
		cellAlive:     true,
		deadInstances: []TenantSyncDeadInstance{{Instance: "pod-a", LastSeenAt: staleSeen}},
	}
	released, err := ReleaseConfirmedFleetLocks(context.Background(), pool, testCluster, "alpha", row, now, nil)
	if err != nil {
		t.Fatalf("ReleaseConfirmedFleetLocks: %v", err)
	}
	if released != 1 {
		t.Fatalf("released = %d, want 1", released)
	}
	if _, _, found := fleetLockRowState(t, db, runID); found {
		t.Fatal("a dead_instances entry past the window must release the row")
	}
}

// TestFleetLockStateMachine_TenantMoveWithNoDeadInstanceKeepsRow asserts
// task 4.3's acceptance: a changed placement with no dead_instances
// entry naming the old instance keeps the row.
func TestFleetLockStateMachine_TenantMoveWithNoDeadInstanceKeepsRow(t *testing.T) {
	pool, db := setupLedgerPool(t)

	runID := newTestUUIDv7String()
	insertInterruptedRun(t, db, runID)
	now := time.Now()
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, runID, FleetLockStateReleasing,
		now.Add(-10*time.Minute), now.Add(5*time.Minute))
	if _, err := db.Exec(`UPDATE varroa.fleet_locks SET cell_instance = 'pod-a' WHERE name = $1 AND run_id = $2`,
		fleetLockStateMachineTestName, runID); err != nil {
		t.Fatalf("set cell_instance: %v", err)
	}

	// The tenant moved to pod-b, but pod-a is absent from dead_instances.
	row := tenantPlacementLivenessRow{cellInstance: "pod-b", cellAlive: true}
	released, err := ReleaseConfirmedFleetLocks(context.Background(), pool, testCluster, "alpha", row, now, nil)
	if err != nil {
		t.Fatalf("ReleaseConfirmedFleetLocks: %v", err)
	}
	if released != 0 {
		t.Fatalf("released = %d, want 0: a changed placement alone releases nothing", released)
	}
	if _, _, found := fleetLockRowState(t, db, runID); !found {
		t.Fatal("a tenant move with no dead_instances entry must keep the row")
	}
}

// TestFleetLockStateMachine_StaleCellAliveAtKeepsRow asserts task 4.3's
// acceptance: a stale cell_alive_at counts cell_alive as absent, so a
// stopped operator releases no lock.
func TestFleetLockStateMachine_StaleCellAliveAtKeepsRow(t *testing.T) {
	pool, db := setupLedgerPool(t)

	runID := newTestUUIDv7String()
	insertInterruptedRun(t, db, runID)
	now := time.Now()
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, runID, FleetLockStateReleasing,
		now.Add(-20*time.Minute), now.Add(5*time.Minute))
	if _, err := db.Exec(`UPDATE varroa.fleet_locks SET cell_instance = 'pod-a' WHERE name = $1 AND run_id = $2`,
		fleetLockStateMachineTestName, runID); err != nil {
		t.Fatalf("set cell_instance: %v", err)
	}

	staleAliveAt := now.Add(-30 * time.Minute) // older than twice the 10-minute announce interval
	lastSeen := now.Add(-20 * time.Minute)     // past the 15-minute silent window on its own
	row := tenantPlacementLivenessRow{
		cellInstance:   "pod-a",
		cellAlive:      false,
		cellAliveAt:    &staleAliveAt,
		cellLastSeenAt: &lastSeen,
	}
	released, err := ReleaseConfirmedFleetLocks(context.Background(), pool, testCluster, "alpha", row, now, nil)
	if err != nil {
		t.Fatalf("ReleaseConfirmedFleetLocks: %v", err)
	}
	if released != 0 {
		t.Fatalf("released = %d, want 0: a stale cell_alive_at must keep the row", released)
	}
}

// TestFleetLockStateMachine_OwnerDiedWithPodReleasedWithoutCancel
// asserts task 4.3's acceptance: an owner that died with its pod, for
// which the interruption path published no cancel_run because the
// owning instance already changed, is released through the
// dead_instances confirmation alone.
func TestFleetLockStateMachine_OwnerDiedWithPodReleasedWithoutCancel(t *testing.T) {
	pool, db := setupLedgerPool(t)

	runID := newTestUUIDv7String()
	insertInterruptedRun(t, db, runID)
	now := time.Now()
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, runID, FleetLockStateReleasing,
		now.Add(-10*time.Minute), now.Add(5*time.Minute))
	if _, err := db.Exec(`UPDATE varroa.fleet_locks SET cell_instance = 'pod-dead' WHERE name = $1 AND run_id = $2`,
		fleetLockStateMachineTestName, runID); err != nil {
		t.Fatalf("set cell_instance: %v", err)
	}

	staleSeen := now.Add(-8 * time.Minute)
	row := tenantPlacementLivenessRow{
		cellInstance:  "pod-new",
		cellAlive:     true,
		deadInstances: []TenantSyncDeadInstance{{Instance: "pod-dead", LastSeenAt: staleSeen}},
	}
	released, err := ReleaseConfirmedFleetLocks(context.Background(), pool, testCluster, "alpha", row, now, nil)
	if err != nil {
		t.Fatalf("ReleaseConfirmedFleetLocks: %v", err)
	}
	if released != 1 {
		t.Fatalf("released = %d, want 1: no undelivered cancel_run should block the confirmed-dead release", released)
	}
}

// TestFleetLockStateMachine_RestartReachesReleasingThroughMaintenance
// asserts task 4.3's acceptance: a recorder restart between the marking
// and the move still reaches releasing through the next maintenance
// round, with no expiry deleting the row first. This is also task 4.4's
// fourth acceptance test.
func TestFleetLockStateMachine_RestartReachesReleasingThroughMaintenance(t *testing.T) {
	pool, db := setupLedgerPool(t)

	runID := newTestUUIDv7String()
	insertInterruptedRun(t, db, runID)
	now := time.Now()
	// The lease already expired before the round runs.
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, runID, FleetLockStateHeld,
		now.Add(-20*time.Minute), now.Add(-time.Minute))

	moved, expired, err := sweepFleetLocks(context.Background(), pool, now)
	if err != nil {
		t.Fatalf("sweepFleetLocks: %v", err)
	}
	if moved != 1 {
		t.Fatalf("moved = %d, want 1", moved)
	}
	if expired != 0 {
		t.Fatalf("expired = %d, want 0: the move must run before the expiry delete", expired)
	}

	state, releasingSince, found := fleetLockRowState(t, db, runID)
	if !found {
		t.Fatal("the round deleted the row; it must move it to releasing instead")
	}
	if state != FleetLockStateReleasing {
		t.Fatalf("state = %q, want releasing", state)
	}
	if !releasingSince.Valid {
		t.Fatal("releasing_since was not stamped")
	}
}

// TestFleetLockStateMachine_NextWaiterPromotedAfterRelease asserts task
// 4.3's tenth acceptance test: once the releasing row leaves the table,
// the next waiter's poll answers held. The move and the deletion each
// run through their own production paths first.
func TestFleetLockStateMachine_NextWaiterPromotedAfterRelease(t *testing.T) {
	pool, db := setupLedgerPool(t)

	holderRunID := newTestUUIDv7String()
	insertInterruptedRun(t, db, holderRunID)
	now := time.Now()
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, holderRunID, FleetLockStateHeld,
		now.Add(-10*time.Minute), now.Add(5*time.Minute))

	waiterRunID := newTestUUIDv7String()
	insertFleetLockRow(t, db, fleetLockStateMachineTestName, waiterRunID, FleetLockStateWaiting,
		now.Add(-time.Minute), now.Add(5*time.Minute))

	if _, err := moveInterruptedHoldersToReleasing(context.Background(), pool, fleetLockStateMachineTestName, now); err != nil {
		t.Fatalf("moveInterruptedHoldersToReleasing: %v", err)
	}
	if state, _, found := fleetLockRowState(t, db, holderRunID); !found || state != FleetLockStateReleasing {
		t.Fatalf("holder state = %q (found=%v), want releasing", state, found)
	}

	// A terminal event for the holder confirms the release.
	if _, err := db.Exec(`DELETE FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2 AND state = 'releasing'`,
		fleetLockStateMachineTestName, holderRunID); err != nil {
		t.Fatalf("release holder row: %v", err)
	}

	outcome, err := acquireFleetLock(context.Background(), pool, nil, fleetLockAcquireParams{
		Name:    fleetLockStateMachineTestName,
		RunID:   waiterRunID,
		Cluster: testCluster,
		Tenant:  testTenant,
		JobPath: testJobPath,
		Lease:   5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("acquireFleetLock: %v", err)
	}
	if outcome.state != FleetLockStateHeld {
		t.Fatalf("waiter's next poll answered %q, want held", outcome.state)
	}
}

// strPtr answers a pointer to s, for a RunCompletedPayload.Result
// literal.
func strPtr(s string) *string { return &s }
