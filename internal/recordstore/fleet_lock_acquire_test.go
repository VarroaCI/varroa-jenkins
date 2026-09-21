package recordstore

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"
)

// lockTestDeps opens a fresh schema and a pgxpool.Pool for a fleet-lock
// test. Neither kind reads Reader, Blob, or Conn.
func lockTestDeps(t *testing.T) (CrossTenantDeps, *sql.DB) {
	t.Helper()
	pool, db := setupLedgerPool(t)
	return CrossTenantDeps{
		Pool:      pool,
		Evaluator: NewGrantEvaluator(pool),
	}, db
}

// insertInterruptedRun inserts a minimal varroa.runs row whose status
// and result mark it interrupted, so a fleet-lock test can name it as a
// held lock's owning run.
func insertInterruptedRun(t *testing.T, db *sql.DB, runID string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status, result)
		VALUES ($1, now(), $2, $3, $4, 'cell-alpha', 'alpha', 'alpha/release', 'COMPLETED', 'INTERRUPTED')
	`, runID, "manual:interrupted-"+runID, testCluster, "ns-alpha"); err != nil {
		t.Fatalf("insert interrupted run: %v", err)
	}
}

// insertFleetLockRow inserts one varroa.fleet_locks row directly, with
// full control over every column a task 3.8 test needs.
func insertFleetLockRow(t *testing.T, db *sql.DB, name, runID, state string, waitingSince, expiresAt time.Time) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.fleet_locks (name, run_id, cluster, tenant, job_path, state, waiting_since, expires_at)
		VALUES ($1, $2, $3, 'alpha', 'alpha/release', $4, $5, $6)
	`, name, runID, testCluster, state, waitingSince, expiresAt); err != nil {
		t.Fatalf("insert fleet lock row: %v", err)
	}
}

// fleetLockTestName is the one lock name every acquire/renew/release
// test in this file exercises.
const fleetLockTestName = "deploy-prod"

// acquireForCaller runs handleCrossTenantLockAcquire for one caller run,
// against fleetLockTestName.
func acquireForCaller(t *testing.T, deps CrossTenantDeps, runID string) CrossTenantResponse {
	t.Helper()
	resp, err := handleCrossTenantLockAcquire(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		Name:         fleetLockTestName,
		RunID:        runID,
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockAcquire: %v", err)
	}
	return resp
}

// TestFleetLockAcquire_FIFOOrderAcrossThreeWaiters covers task 3.8's
// first acceptance. It proves true (waiting_since, run_id) ordering, not
// merely acquire-call order: after the holder releases, the newer
// waiter polls first and still stays waiting, and only the older waiter
// is promoted.
func TestFleetLockAcquire_FIFOOrderAcrossThreeWaiters(t *testing.T) {
	deps, db := lockTestDeps(t)
	// pendingJobs is non-nil here only to prove GrantEvaluator.CheckPlacement
	// ignores it: pending-job gating is decidePlacement's claim-path order,
	// not a lock-path or request-path concern (see grant_evaluator.go).
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", []string{"alpha/unrelated-job"})

	r1 := acquireForCaller(t, deps, "run-1")
	if r1.State != FleetLockStateHeld {
		t.Fatalf("r1 state: got %q, want %q", r1.State, FleetLockStateHeld)
	}

	r2 := acquireForCaller(t, deps, "run-2")
	if r2.State != FleetLockStateWaiting {
		t.Fatalf("r2 state: got %q, want %q", r2.State, FleetLockStateWaiting)
	}

	r3 := acquireForCaller(t, deps, "run-3")
	if r3.State != FleetLockStateWaiting {
		t.Fatalf("r3 state: got %q, want %q", r3.State, FleetLockStateWaiting)
	}
	if r3.Position <= r2.Position {
		t.Fatalf("r3 position %d is not after r2 position %d", r3.Position, r2.Position)
	}

	if resp := handleCrossTenantLockReleaseForTest(t, deps, "run-1"); resp.State != FleetLockStateReleased {
		t.Fatalf("release r1: got state %q", resp.State)
	}

	// The newer waiter polls first: it must still wait behind r2.
	r3Again := acquireForCaller(t, deps, "run-3")
	if r3Again.State != FleetLockStateWaiting {
		t.Fatalf("r3 after release: got %q, want %q (FIFO order must not let a newer waiter jump ahead)", r3Again.State, FleetLockStateWaiting)
	}

	// The older waiter is promoted.
	r2Again := acquireForCaller(t, deps, "run-2")
	if r2Again.State != FleetLockStateHeld {
		t.Fatalf("r2 after release: got %q, want %q", r2Again.State, FleetLockStateHeld)
	}
}

// handleCrossTenantLockReleaseForTest is a thin wrapper so acquire and
// renew tests do not need to import the release kind's request shape by
// hand. It releases fleetLockTestName.
func handleCrossTenantLockReleaseForTest(t *testing.T, deps CrossTenantDeps, runID string) CrossTenantResponse {
	t.Helper()
	resp, err := handleCrossTenantLockRelease(context.Background(), deps, CrossTenantRequest{
		Cluster: testCluster, CallerTenant: "alpha", Name: fleetLockTestName, RunID: runID,
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockRelease: %v", err)
	}
	return resp
}

// TestFleetLockAcquire_StaleCellAnswersFenced asserts an acquire from a
// cell the tenant holds no member on is refused permanently and writes
// no row. A stale generation alone answers the retryable NOT_READY, so
// it is not the shape this test drives.
func TestFleetLockAcquire_StaleCellAnswersFenced(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 5, "pod-a", "pod-a", nil)

	resp, err := handleCrossTenantLockAcquire(context.Background(), deps, CrossTenantRequest{
		Cluster:             testCluster,
		Namespace:           "ns-alpha",
		Cell:                "cell-old",
		CallerTenant:        "alpha",
		PlacementGeneration: 5,
		Name:                "deploy-prod",
		RunID:               "run-1",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockAcquire: %v", err)
	}
	if resp.ReasonCode != ReasonFenced {
		t.Fatalf("reason_code: got %q, want %q", resp.ReasonCode, ReasonFenced)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.fleet_locks`).Scan(&count); err != nil {
		t.Fatalf("count fleet_locks: %v", err)
	}
	if count != 0 {
		t.Fatalf("fleet_locks rows: got %d, want 0 (a fenced acquire must not touch the table)", count)
	}
}

// TestFleetLockAcquire_UngrantedCrossPrefixAnswersCrossTenant covers
// task 3.8's third acceptance.
func TestFleetLockAcquire_UngrantedCrossPrefixAnswersCrossTenant(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)
	seedTenantPlacement(t, db, "beta", "ns-beta", "cell-beta", 0, "pod-b", "pod-b", nil)

	resp, err := handleCrossTenantLockAcquire(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		Name:         "beta/deploy-prod",
		RunID:        "run-1",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockAcquire: %v", err)
	}
	if resp.ReasonCode != ReasonCrossTenant {
		t.Fatalf("reason_code: got %q, want %q (message %q)", resp.ReasonCode, ReasonCrossTenant, resp.Message)
	}
}

// TestFleetLockAcquire_ReleasingRowKeepsAWaiterWaiting covers task 3.8's
// fourth acceptance.
func TestFleetLockAcquire_ReleasingRowKeepsAWaiterWaiting(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)

	now := time.Now().UTC()
	insertFleetLockRow(t, db, "alpha/deploy-prod", "holder-run", "releasing", now.Add(-time.Minute), now.Add(5*time.Minute))

	resp := acquireForCaller(t, deps, "waiter-run")
	if resp.State != FleetLockStateWaiting {
		t.Fatalf("state: got %q, want %q", resp.State, FleetLockStateWaiting)
	}
	if resp.Holder != "holder-run" {
		t.Fatalf("holder: got %q, want %q", resp.Holder, "holder-run")
	}
}

// TestFleetLockAcquire_SuccessiveHoldersGetIncreasingTokens covers task
// 3.8's fifth acceptance.
func TestFleetLockAcquire_SuccessiveHoldersGetIncreasingTokens(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)

	first := acquireForCaller(t, deps, "holder-1")
	if first.State != FleetLockStateHeld {
		t.Fatalf("first state: got %q, want %q", first.State, FleetLockStateHeld)
	}
	handleCrossTenantLockReleaseForTest(t, deps, "holder-1")

	second := acquireForCaller(t, deps, "holder-2")
	if second.State != FleetLockStateHeld {
		t.Fatalf("second state: got %q, want %q", second.State, FleetLockStateHeld)
	}
	if second.FenceToken <= first.FenceToken {
		t.Fatalf("second token %d is not greater than first token %d", second.FenceToken, first.FenceToken)
	}
}

// TestFleetLockAcquire_ExpiredInterruptedHolderBecomesReleasing covers
// task 3.8's sixth acceptance: an expired held row of an interrupted run
// becomes releasing and is not deleted.
func TestFleetLockAcquire_ExpiredInterruptedHolderBecomesReleasing(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)

	holderRunID := newTestUUIDv7String()
	insertInterruptedRun(t, db, holderRunID)
	now := time.Now().UTC()
	insertFleetLockRow(t, db, "alpha/deploy-prod", holderRunID, "held", now.Add(-time.Hour), now.Add(-time.Minute))

	resp := acquireForCaller(t, deps, "waiter-run")
	if resp.State != FleetLockStateWaiting {
		t.Fatalf("waiter state: got %q, want %q", resp.State, FleetLockStateWaiting)
	}

	var state string
	if err := db.QueryRow(`SELECT state FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2`,
		"alpha/deploy-prod", holderRunID).Scan(&state); err != nil {
		t.Fatalf("read holder row: %v", err)
	}
	if state != FleetLockStateReleasing {
		t.Fatalf("holder state: got %q, want %q (an expired interrupted holder must move to releasing, not be deleted)", state, FleetLockStateReleasing)
	}
}

// TestFleetLockAcquire_ConcurrentAcquiresLeaveExactlyOneHolder covers
// task 3.9's first acceptance: 20 concurrent acquires leave exactly one
// holder and reach no caller with a database error.
func TestFleetLockAcquire_ConcurrentAcquiresLeaveExactlyOneHolder(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)

	const n = 20
	var wg sync.WaitGroup
	responses := make([]CrossTenantResponse, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := handleCrossTenantLockAcquire(context.Background(), deps, CrossTenantRequest{
				Cluster:      testCluster,
				Namespace:    "ns-alpha",
				Cell:         "cell-alpha",
				CallerTenant: "alpha",
				Name:         "deploy-prod",
				RunID:        "run-" + string(rune('a'+i)),
			})
			responses[i] = resp
			errs[i] = err
		}(i)
	}
	wg.Wait()

	held := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d got a database error: %v", i, err)
		}
		if responses[i].Error != "" {
			t.Fatalf("caller %d got a server fault: %s", i, responses[i].Error)
		}
		if responses[i].State == FleetLockStateHeld {
			held++
		}
	}
	if held != 1 {
		t.Fatalf("held responses: got %d, want exactly 1", held)
	}

	var heldRows int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.fleet_locks WHERE name = 'alpha/deploy-prod' AND state = 'held'`).Scan(&heldRows); err != nil {
		t.Fatalf("count held rows: %v", err)
	}
	if heldRows != 1 {
		t.Fatalf("held rows in the table: got %d, want 1", heldRows)
	}
}

// TestFleetLockAcquire_RepeatedViolationAnswersWaiting covers task 3.9's
// second acceptance directly against readFleetLockWaitOutcome, the exact
// function acquireFleetLock falls back to after a second violation
// (design.md D11: "the winner now holds a row that step 4 can see").
// design commitment 10 makes the collision itself a database-level
// event this package cannot force twice for one caller on demand
// (the first-time-acquire race the partial unique index resolves has no
// steady-state repeat once a "held" row exists), so this test seeds the
// state a second violation always finds and asserts the read that
// follows it answers exactly what the acceptance names.
func TestFleetLockAcquire_RepeatedViolationAnswersWaiting(t *testing.T) {
	deps, db := lockTestDeps(t)
	now := time.Now().UTC()
	insertFleetLockRow(t, db, "alpha/deploy-prod", "winner-run", "held", now.Add(-time.Minute), now.Add(5*time.Minute))
	insertFleetLockRow(t, db, "alpha/deploy-prod", "loser-run", "waiting", now, now.Add(5*time.Minute))

	outcome, err := readFleetLockWaitOutcome(context.Background(), deps.Pool, "alpha/deploy-prod", "loser-run", 5*time.Minute)
	if err != nil {
		t.Fatalf("readFleetLockWaitOutcome: %v", err)
	}
	if outcome.state != FleetLockStateWaiting {
		t.Fatalf("state: got %q, want %q", outcome.state, FleetLockStateWaiting)
	}
	if outcome.holder != "winner-run" {
		t.Fatalf("holder: got %q, want %q", outcome.holder, "winner-run")
	}
}

// TestFleetLockAcquire_PublishesWaitingThenHeld asserts a fresh acquire
// publishes the caller's own waiting notification, and then the held one
// when the promotion lands. The held notification follows the notifier's
// own coalescing rule, so the test ends the run's interval before it
// counts.
func TestFleetLockAcquire_PublishesWaitingThenHeld(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)
	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	deps.LockNotifier = notifier

	runID := newTestUUIDv7String()
	resp := acquireForCaller(t, deps, runID)
	if resp.State != FleetLockStateHeld {
		t.Fatalf("state: got %q, want %q", resp.State, FleetLockStateHeld)
	}

	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications inside the interval, want 1", got)
	}
	first := sent.payload(0)
	if first.Kind != "lock" || first.State != FleetLockStateWaiting {
		t.Fatalf("first notification = kind %q state %q, want lock/waiting", first.Kind, first.State)
	}
	if first.RunID != runID || first.LockName != "alpha/deploy-prod" {
		t.Fatalf("first notification names %s/%s, want the caller's own row", first.RunID, first.LockName)
	}
	if first.Cluster != testCluster || first.Tenant != "alpha" {
		t.Fatalf("first notification coordinates = %s/%s, want %s/alpha", first.Cluster, first.Tenant, testCluster)
	}

	notifier.now = func() time.Time { return notifyClockStart.Add(2 * notifier.interval) }
	notifier.flush(runID)
	if got := sent.count(); got != 2 {
		t.Fatalf("published %d notifications after the interval, want 2", got)
	}
	second := sent.payload(1)
	if second.Kind != "lock" || second.State != FleetLockStateHeld {
		t.Fatalf("second notification = kind %q state %q, want lock/held", second.Kind, second.State)
	}
	if second.LockName != "alpha/deploy-prod" || second.RunID != runID {
		t.Fatalf("second notification names %s/%s, want the caller's own row", second.RunID, second.LockName)
	}
}

// TestFleetLockAcquire_RepeatPollPublishesNothing asserts a second
// acquire that only refreshes the row's expiry publishes nothing,
// because the row's state did not change.
func TestFleetLockAcquire_RepeatPollPublishesNothing(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)
	now := time.Now().UTC()
	// Another run holds the lock, so this caller only ever waits.
	insertFleetLockRow(t, db, "alpha/deploy-prod", "holder-run", FleetLockStateHeld,
		now.Add(-time.Minute), now.Add(time.Hour))

	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	deps.LockNotifier = notifier

	runID := newTestUUIDv7String()
	if resp := acquireForCaller(t, deps, runID); resp.State != FleetLockStateWaiting {
		t.Fatalf("first poll state: got %q, want %q", resp.State, FleetLockStateWaiting)
	}
	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications for the first insert, want 1", got)
	}

	notifier.now = func() time.Time { return notifyClockStart.Add(2 * notifier.interval) }
	if resp := acquireForCaller(t, deps, runID); resp.State != FleetLockStateWaiting {
		t.Fatalf("repeat poll state: got %q, want %q", resp.State, FleetLockStateWaiting)
	}
	notifier.flush(runID)
	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications after a repeat poll, want 1", got)
	}
}

// TestFleetLockAcquire_PromotionIntoAHeldRowPublishesNothing asserts a
// promotion whose own predicate matches no row publishes nothing: the
// other run still holds the lock, so this caller's poll writes no state.
func TestFleetLockAcquire_PromotionIntoAHeldRowPublishesNothing(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)
	now := time.Now().UTC()
	insertFleetLockRow(t, db, "alpha/deploy-prod", "holder-run", FleetLockStateHeld,
		now.Add(-time.Minute), now.Add(4*time.Hour))

	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	deps.LockNotifier = notifier

	runID := newTestUUIDv7String()
	// The first poll inserts this run's own waiting row and publishes.
	if resp := acquireForCaller(t, deps, runID); resp.State != FleetLockStateWaiting {
		t.Fatalf("first poll state: got %q, want %q", resp.State, FleetLockStateWaiting)
	}
	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications for the first insert, want 1", got)
	}

	// A later poll re-tests the promotion predicate against a row the
	// holder still holds, so the transaction promotes nothing.
	notifier.now = func() time.Time { return notifyClockStart.Add(time.Hour) }
	if resp := acquireForCaller(t, deps, runID); resp.State != FleetLockStateWaiting {
		t.Fatalf("second poll state: got %q, want %q", resp.State, FleetLockStateWaiting)
	}
	notifier.flush(runID)
	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications after an unpromoted poll, want 1", got)
	}
}

// TestFleetLockAcquire_ExpiryDeletePublishesOnePerRow asserts one
// transaction's expiry delete of two waiters publishes two released
// notifications, each naming its own row.
func TestFleetLockAcquire_ExpiryDeletePublishesOnePerRow(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)
	now := time.Now().UTC()
	// Two waiters whose leases already expired.
	insertFleetLockRow(t, db, "alpha/deploy-prod", "waiter-1", FleetLockStateWaiting,
		now.Add(-time.Hour), now.Add(-time.Minute))
	insertFleetLockRow(t, db, "alpha/deploy-prod", "waiter-2", FleetLockStateWaiting,
		now.Add(-time.Hour), now.Add(-time.Minute))

	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	deps.LockNotifier = notifier

	resp := acquireForCaller(t, deps, newTestUUIDv7String())
	if resp.State != FleetLockStateHeld {
		t.Fatalf("state: got %q, want %q", resp.State, FleetLockStateHeld)
	}

	// The two released notifications carry distinct run ids, so neither
	// coalesces; the caller's own held notification is held for the
	// interval and is not part of this count.
	released := map[string]bool{}
	for i := 0; i < sent.count(); i++ {
		got := sent.payload(i)
		if got.State == FleetLockStateReleased {
			released[got.RunID] = true
		}
	}
	if !released["waiter-1"] || !released["waiter-2"] {
		t.Fatalf("released notifications = %v, want one per expired waiter", released)
	}
}

// TestFleetLockAcquire_StoresCallerJobPathOnTheRowAndItsNotifications
// asserts a request's caller_job_path reaches the row's own job_path
// column, the cell-instance read, and both of the caller's own
// notifications.
func TestFleetLockAcquire_StoresCallerJobPathOnTheRowAndItsNotifications(t *testing.T) {
	deps, db := lockTestDeps(t)
	// Two members, so the caller's job path is homed on slot 1 rather
	// than slot 0: a stored cell_instance of pod-b proves the read
	// resolved the caller's own job path.
	seedTenantPlacementMembers(t, db, "alpha", 0, []member{
		{Cell: "cell-a", Namespace: "ns-alpha", Role: MemberRoleActive, Slot: slotPtr(0),
			Instance: "pod-a", RehydratedInstance: "pod-a"},
		{Cell: "cell-b", Namespace: "ns-alpha", Role: MemberRoleActive, Slot: slotPtr(1),
			Instance: "pod-b", RehydratedInstance: "pod-b"},
	}, map[string]int32{"alpha/release": 1})

	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	deps.LockNotifier = notifier

	runID := newTestUUIDv7String()
	resp, err := handleCrossTenantLockAcquire(context.Background(), deps, CrossTenantRequest{
		Cluster:       testCluster,
		Namespace:     "ns-alpha",
		Cell:          "cell-a",
		CallerTenant:  "alpha",
		CallerJobPath: "alpha/release",
		Name:          fleetLockTestName,
		RunID:         runID,
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockAcquire: %v", err)
	}
	if resp.State != FleetLockStateHeld {
		t.Fatalf("state: got %q, want %q", resp.State, FleetLockStateHeld)
	}

	var jobPath, cellInstance string
	if err := db.QueryRow(`SELECT job_path, cell_instance FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2`,
		"alpha/"+fleetLockTestName, runID).Scan(&jobPath, &cellInstance); err != nil {
		t.Fatalf("read fleet lock row: %v", err)
	}
	if jobPath != "alpha/release" {
		t.Fatalf("row job_path = %q, want the caller's own job path", jobPath)
	}
	if cellInstance != "pod-b" {
		t.Fatalf("row cell_instance = %q, want pod-b, the member the caller's own job path is homed on", cellInstance)
	}

	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications inside the interval, want 1", got)
	}
	waiting := sent.payload(0)
	if waiting.State != FleetLockStateWaiting || waiting.JobPath != "alpha/release" {
		t.Fatalf("waiting notification = state %q job_path %q, want waiting/alpha/release", waiting.State, waiting.JobPath)
	}

	notifier.now = func() time.Time { return notifyClockStart.Add(2 * notifier.interval) }
	notifier.flush(runID)
	if got := sent.count(); got != 2 {
		t.Fatalf("published %d notifications after the interval, want 2", got)
	}
	held := sent.payload(1)
	if held.State != FleetLockStateHeld || held.JobPath != "alpha/release" {
		t.Fatalf("held notification = state %q job_path %q, want held/alpha/release", held.State, held.JobPath)
	}
}

// TestFleetLockAcquire_NilNotifierPublishesNothing asserts an acquire
// with no notifier completes and panics nothing.
func TestFleetLockAcquire_NilNotifierPublishesNothing(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)
	deps.LockNotifier = nil

	resp := acquireForCaller(t, deps, newTestUUIDv7String())
	if resp.State != FleetLockStateHeld {
		t.Fatalf("state: got %q, want %q", resp.State, FleetLockStateHeld)
	}
}
