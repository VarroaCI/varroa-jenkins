package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedReplacedPodDead lists replacedPod in the dead_instances of the test
// tenant's placement row, with a pod-read confirmation at confirmedAt, and
// stamps a fresh cell_alive_at. The operator announces this entry on each
// tick after it reads the next pod of a controller with no cell role.
func seedReplacedPodDead(t *testing.T, db *sql.DB, confirmedAt time.Time) {
	t.Helper()
	dead, err := json.Marshal([]TenantSyncDeadInstance{{
		Instance: replacedPod, LastSeenAt: confirmedAt, ConfirmedGoneAt: &confirmedAt,
	}})
	if err != nil {
		t.Fatalf("marshal dead instances: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET dead_instances = $3::jsonb, cell_alive_at = now()
		WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant, string(dead)); err != nil {
		t.Fatalf("list the replaced pod as dead: %v", err)
	}
}

// lockHolderOf reads the cell_instance of the one fleet lock row of runID.
func lockHolderOf(t *testing.T, db *sql.DB, runID uuid.UUID) string {
	t.Helper()
	var holder string
	if err := db.QueryRow(`SELECT cell_instance FROM varroa.fleet_locks WHERE run_id = $1`,
		runID.String()).Scan(&holder); err != nil {
		t.Fatalf("read the lock holder: %v", err)
	}
	return holder
}

// seedReplacedPodLock inserts one held fleet lock row for runID that names
// replacedPod as its holder.
func seedReplacedPodLock(t *testing.T, db *sql.DB, runID uuid.UUID, now time.Time) {
	t.Helper()
	insertLockRowForTenant(t, db, fleetLockStateMachineTestName, runID.String(), testTenant, testJobPath,
		FleetLockStateHeld, now.Add(-10*time.Minute), now.Add(5*time.Minute))
	if _, err := db.Exec(`UPDATE varroa.fleet_locks SET cell_instance = $2 WHERE run_id = $1`,
		runID.String(), replacedPod); err != nil {
		t.Fatalf("name the replaced pod as the lock holder: %v", err)
	}
}

// TestFinalizeStaleLogsKeepsTheLogOfAReplacedControllerPod proves that the
// log finalization sweep reads a run of a controller with no cell role as
// the interrupted sweep reads it. The operator confirms the replaced pod
// gone while the next pod starts. That pod resumes the build and its log
// from the volume, so the sweep must not finalize the log. The same
// placement row still finalizes the log of a cell run.
func TestFinalizeStaleLogsKeepsTheLogOfAReplacedControllerPod(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		generation    int64
		wantFinalized int
	}{
		{name: "a controller with no cell role", generation: 0, wantFinalized: 0},
		{name: "a cell", generation: cellRunGeneration, wantFinalized: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pool, db := setupLedgerPool(t)
			runID, _ := seedReplacedControllerRun(t, pool, db, tc.generation)
			now := time.Now().UTC()
			seedReplacedPodDead(t, db, now.Add(-time.Minute))
			if _, err := db.Exec(`UPDATE varroa.runs SET heartbeat_received_at = $2 WHERE run_id = $1`,
				runID, now.Add(-3*time.Minute)); err != nil {
				t.Fatalf("stamp the last heartbeat: %v", err)
			}

			finalized, err := finalizeStaleLogs(context.Background(), pool, NewPlacementLiveness(pool, testGrace),
				nil, time.Minute, now)
			if err != nil {
				t.Fatalf("finalizeStaleLogs: %v", err)
			}
			if finalized != tc.wantFinalized {
				t.Fatalf("finalized = %d, want %d", finalized, tc.wantFinalized)
			}
			if _, isFinalized := readLogSummaryFinalized(t, db, runID); isFinalized != (tc.wantFinalized == 1) {
				t.Fatalf("log summary finalized = %v, want %v", isFinalized, tc.wantFinalized == 1)
			}
		})
	}
}

// TestReplacementPodTakeOverMovesTheFleetLock proves that the takeover
// moves the run's fleet lock to the replacement pod. The release
// confirmation and the force release read the lock's cell instance. A lock
// that still named the replaced pod would read a confirmed-gone holder
// while the build holds the lock on the next pod.
func TestReplacementPodTakeOverMovesTheFleetLock(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, intentKey := seedReplacedControllerRun(t, pool, db, 0)
	now := time.Now().UTC()
	seedReplacedPodDead(t, db, now.Add(-time.Minute))
	seedReplacedPodLock(t, db, runID, now)

	applyFromPod(t, NewIngester(pool, nil, nil), runID, intentKey, KindRunHeartbeat, "cell-a", replacementPod,
		map[string]any{"seq": 1})

	if holder := lockHolderOf(t, db, runID); holder != replacementPod {
		t.Fatalf("lock cell_instance = %q, want %q", holder, replacementPod)
	}

	// A heartbeat-lost mark moves the lock to releasing, and the owner can
	// still hold the build. The replacement pod is alive, so neither
	// release path deletes the lock.
	if _, err := db.Exec(`UPDATE varroa.fleet_locks SET state = 'releasing', releasing_since = now() WHERE run_id = $1`,
		runID.String()); err != nil {
		t.Fatalf("move the lock to releasing: %v", err)
	}
	released, err := ReleaseConfirmedFleetLocksRound(context.Background(), pool, now, testGrace, nil, nil)
	if err != nil {
		t.Fatalf("ReleaseConfirmedFleetLocksRound: %v", err)
	}
	if released != 0 {
		t.Fatalf("released = %d, want 0", released)
	}
	req := forceReleaseRequest()
	req.RunID = runID.String()
	resp, err := ForceReleaseFleetLock(context.Background(), pool, req, nil)
	if err != nil {
		t.Fatalf("ForceReleaseFleetLock: %v", err)
	}
	if resp.Outcome != FleetLockForceHolderLive {
		t.Fatalf("force release = %+v, want %s", resp, FleetLockForceHolderLive)
	}
}

// TestReplacementPodCompletionReleasesTheFleetLock proves the completion
// arm of the lock move. A run.completed of the replacement pod moves no
// lock row: its owner write deletes every lock row of the run in the same
// transaction.
func TestReplacementPodCompletionReleasesTheFleetLock(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, intentKey := seedReplacedControllerRun(t, pool, db, 0)
	seedReplacedPodLock(t, db, runID, time.Now().UTC())

	res := applyFromPod(t, NewIngester(pool, nil, nil), runID, intentKey, KindRunCompleted, "cell-a", replacementPod,
		RunCompletedPayload{Result: strPtr("SUCCESS")})

	if !res.AdoptedOwner || !res.CompletedOwner || len(res.ReleasedFleetLocks) != 1 {
		t.Fatalf("result = %+v, want a takeover, an owner completion, and one released lock", res)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.fleet_locks WHERE run_id = $1`, runID.String()).Scan(&n); err != nil {
		t.Fatalf("count lock rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("lock rows = %d, want 0", n)
	}
}

// seedControllerMarkedIntent inserts one GRANTED intent that the claim
// granted to replacedPod on cell-a under generation, with build number 6
// and a start mark at markedAt. No runs row stands behind it.
func seedControllerMarkedIntent(t *testing.T, db *sql.DB, generation int64, markedAt time.Time) uuid.UUID {
	t.Helper()
	runID := uuid.Must(uuid.NewV7())
	if _, err := db.Exec(`
		INSERT INTO varroa.run_intents (
			intent_key, run_id, created_at, cluster, namespace, tenant, job_path,
			cell, cell_instance, placement_generation, state, actor, build_number, start_marked_at)
		VALUES ($1, $2, now(), $3, $4, $5, $6, 'cell-a', $7, $8, 'GRANTED', 'alice', 6, $9)
	`, "manual:"+runID.String(), runID, testCluster, testNamespace, testTenant, testJobPath,
		replacedPod, generation, markedAt); err != nil {
		t.Fatalf("seed marked intent: %v", err)
	}
	return runID
}

// TestEndMarkedLostIntentsLeavesTheStartOfAReplacedControllerPod proves
// the start-mark end of a controller with no cell role. The operator
// confirms the replaced pod gone while the next pod starts, and that pod
// can resume the build from the volume. The confirmed class therefore
// ends no intent that the claim granted under generation 0. The
// start-lost class still ends it after ClaimExpiryAge. A cell intent keeps
// the confirmed class.
func TestEndMarkedLostIntentsLeavesTheStartOfAReplacedControllerPod(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		generation int64
		markAge    time.Duration
		wantEnded  int
		wantReason string
	}{
		{name: "a controller with no cell role", generation: 0, markAge: 2 * time.Minute},
		{name: "a controller with no cell role past the start-lost age", generation: 0,
			markAge: ClaimExpiryAge + time.Minute, wantEnded: 1, wantReason: ReasonStartLost},
		{name: "a cell", generation: cellRunGeneration, markAge: 2 * time.Minute,
			wantEnded: 1, wantReason: ReasonCellRolled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pool, db := setupLedgerPool(t)
			now := time.Now().UTC()
			runID := seedControllerMarkedIntent(t, db, tc.generation, now.Add(-tc.markAge))
			seedControllerPlacement(t, db, tc.generation, replacementPod)
			seedReplacedPodDead(t, db, now.Add(-time.Minute))

			conn, cleanup := newSweepTestBus(t)
			defer cleanup()
			freshenSweepConsumer(t, conn)

			if ended := runEndMarked(t, pool, conn, nil, nil); ended != tc.wantEnded {
				t.Fatalf("ended = %d, want %d", ended, tc.wantEnded)
			}
			if tc.wantEnded == 0 {
				if n := countRunRows(t, db, runID); n != 0 {
					t.Fatalf("runs rows = %d, want 0", n)
				}
				return
			}
			var reason string
			if err := db.QueryRow(`SELECT payload->>'interrupted_reason' FROM varroa.runs WHERE run_id = $1`,
				runID).Scan(&reason); err != nil {
				t.Fatalf("read the mark: %v", err)
			}
			if reason != tc.wantReason {
				t.Fatalf("interrupted_reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// TestRefireFiresTheUnstartedIntentOfAReplacedControllerPod records the
// re-fire rule for a controller with no cell role. An intent with no start
// mark proves that its build never left the queue of the replaced pod. The
// next pod can hold the item in the queue it reloads from the volume, or
// it can hold no item. Only the re-fire recovers the second case. The
// plugin folds a trigger into a queued item of the same run id, so the
// first case still builds once. The re-fire therefore keeps its rule and
// addresses the controller.
func TestRefireFiresTheUnstartedIntentOfAReplacedControllerPod(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := uuid.Must(uuid.NewV7())
	intentKey := "manual:" + runID.String()
	if _, err := db.Exec(`
		INSERT INTO varroa.run_intents (
			intent_key, run_id, created_at, cluster, namespace, tenant, job_path,
			cell, cell_instance, placement_generation, state, actor)
		VALUES ($1, $2, now(), $3, $4, $5, $6, 'cell-a', $7, 0, 'GRANTED', 'alice')
	`, intentKey, runID, testCluster, testNamespace, testTenant, testJobPath, replacedPod); err != nil {
		t.Fatalf("seed granted intent: %v", err)
	}
	seedControllerPlacement(t, db, 0, replacementPod)
	seedReplacedPodDead(t, db, time.Now().UTC().Add(-time.Minute))

	conn := newTestActionConn(t)
	received := collectActions(t, conn)
	if published := runRefire(t, pool, conn, nil); published != 1 {
		t.Fatalf("published = %d, want 1", published)
	}
	select {
	case a := <-received:
		if a.Kind != LedgerActionTriggerRun || a.Namespace != testNamespace || a.Controller != "cell-a" ||
			a.RunID != runID.String() || a.IntentKey != intentKey {
			t.Fatalf("action = %+v, want one trigger_run of the run on %s/cell-a", a, testNamespace)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("want one trigger_run action")
	}
}

// TestReleaseGoneInstanceAgentsLeavesAControllerWithNoCellRole records the
// idle agent rule for a controller with no cell role. The release names
// the tenant's agent namespace, and the operator announces none for such a
// controller. A confirmed-gone replaced pod therefore publishes no
// release_instance_agents action.
func TestReleaseGoneInstanceAgentsLeavesAControllerWithNoCellRole(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedControllerPlacement(t, db, 0, replacementPod)
	seedReplacedPodDead(t, db, time.Now().UTC().Add(-2*time.Minute))

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	received := subscribeClusterActions(t, conn)

	n, err := ReleaseGoneInstanceAgents(context.Background(), pool, conn, nil, time.Now(), testGrace, nil)
	if err != nil {
		t.Fatalf("ReleaseGoneInstanceAgents: %v", err)
	}
	if n != 0 {
		t.Fatalf("published = %d, want 0", n)
	}
	select {
	case a := <-received:
		t.Fatalf("unexpected action %+v", a)
	case <-time.After(500 * time.Millisecond):
	}
}
