package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// replacedPod and replacementPod are two pods of one controller with no
// cell role. The claim granted the run to replacedPod. The placement row
// then names replacementPod, the pod that the StatefulSet created next.
const (
	replacedPod    = "pod-replaced"
	replacementPod = "pod-replacement"
)

// seedControllerPlacement inserts the placement row of the claim's own
// tenant, namespace, and cell, at generation, with instance as the
// current pod. Generation zero is the row the operator announces for a
// controller with no cell role.
func seedControllerPlacement(t *testing.T, db *sql.DB, generation int64, instance string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements
			(cluster, tenant, namespace, cell, generation, cell_instance, rehydrated_instance, cell_alive)
		VALUES ($1, $2, $3, 'cell-a', $4, $5, $5, true)
	`, testCluster, testTenant, testNamespace, generation, instance); err != nil {
		t.Fatalf("seed controller placement: %v", err)
	}
}

// intentOwnerOf reads the cell instance that the run's intent names.
func intentOwnerOf(t *testing.T, db *sql.DB, runID uuid.UUID) string {
	t.Helper()
	var owner sql.NullString
	if err := db.QueryRow(`SELECT cell_instance FROM varroa.run_intents WHERE run_id = $1`, runID).Scan(&owner); err != nil {
		t.Fatalf("read the intent owner: %v", err)
	}
	return owner.String
}

// applyFromPod applies one envelope of kind for runID from sender on cell
// and answers its result.
func applyFromPod(t *testing.T, g *Ingester, runID uuid.UUID, intentKey, kind, cell, sender string, payload any) ApplyResult {
	t.Helper()
	env := newTestEnvelope(t, runID, intentKey, kind, payload)
	env.Cell = cell
	env.CellInstance = sender
	res, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("apply %s from %s/%s: %v", kind, cell, sender, err)
	}
	return res
}

// replacementRunState is what one run row holds of the takeover: its
// status, its result, whether a heartbeat set its receive stamp, whether
// its payload holds an owner completion and a foreign completion, and
// whether a completion superseded an interrupted mark.
type replacementRunState struct {
	status, result    string
	heartbeat         bool
	ownerCompletion   bool
	foreignCompletion bool
	supersededMark    bool
}

// readReplacementRunState reads the replacementRunState of runID.
func readReplacementRunState(t *testing.T, db *sql.DB, runID uuid.UUID) replacementRunState {
	t.Helper()
	var s replacementRunState
	var result sql.NullString
	if err := db.QueryRow(`
		SELECT status, result, heartbeat_received_at IS NOT NULL,
		       COALESCE(payload ? 'run.completed', false), COALESCE(payload ? 'foreign_completion', false),
		       interrupted_superseded_at IS NOT NULL
		  FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&s.status, &result, &s.heartbeat, &s.ownerCompletion, &s.foreignCompletion,
		&s.supersededMark); err != nil {
		t.Fatalf("read the run state: %v", err)
	}
	s.result = result.String
	return s
}

// seedReplacedControllerRun claims one run for replacedPod under
// generation and applies its run.started, then seeds the placement row
// at generation naming replacementPod.
func seedReplacedControllerRun(t *testing.T, pool *pgxpool.Pool, db *sql.DB, generation int64) (uuid.UUID, string) {
	t.Helper()
	runID, intentKey := seedOwnedRun(t, pool, replacedPod, generation)
	seedControllerPlacement(t, db, generation, replacementPod)
	return runID, intentKey
}

// TestReplacementPodHeartbeatTakesOverTheRun proves the heartbeat rule.
// The replacement pod of a controller with no cell role resumes the build
// from the controller's volume and sends no new claim. Its first
// heartbeat moves the run to it and raises the liveness stamp, so the
// sweep sees a live owner.
func TestReplacementPodHeartbeatTakesOverTheRun(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, intentKey := seedReplacedControllerRun(t, pool, db, 0)

	res := applyFromPod(t, NewIngester(pool, nil, nil), runID, intentKey, KindRunHeartbeat, "cell-a", replacementPod,
		map[string]any{"seq": 1})

	if !res.AdoptedOwner {
		t.Fatal("AdoptedOwner = false, want true")
	}
	if owner := intentOwnerOf(t, db, runID); owner != replacementPod {
		t.Fatalf("intent owner = %q, want %q", owner, replacementPod)
	}
	if s := readReplacementRunState(t, db, runID); !s.heartbeat {
		t.Fatal("heartbeat_received_at is null, want the replacement pod's heartbeat")
	}
}

// TestReplacementPodCompletionEndsTheRun proves the completion rule. A
// replacement pod can end the run before its first heartbeat: the boot
// reconcile of a freestyle build, or a short resumed tail. Its
// run.completed is then the run's own terminal write.
func TestReplacementPodCompletionEndsTheRun(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, intentKey := seedReplacedControllerRun(t, pool, db, 0)

	res := applyFromPod(t, NewIngester(pool, nil, nil), runID, intentKey, KindRunCompleted, "cell-a", replacementPod,
		RunCompletedPayload{Result: strPtr("SUCCESS")})

	if !res.AdoptedOwner || !res.CompletedOwner {
		t.Fatalf("AdoptedOwner = %v, CompletedOwner = %v, want both true", res.AdoptedOwner, res.CompletedOwner)
	}
	s := readReplacementRunState(t, db, runID)
	if s.status != StatusCompleted || s.result != "SUCCESS" || !s.ownerCompletion || s.foreignCompletion {
		t.Fatalf("run = %+v, want COMPLETED SUCCESS from an owner completion", s)
	}
}

// TestReplacementPodCompletionReplacesACellLostMark proves that the
// replacement pod's completion still gives the real result when a sweep
// marked the run before the pod took it over.
func TestReplacementPodCompletionReplacesACellLostMark(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, intentKey := seedReplacedControllerRun(t, pool, db, 0)
	c := fetchRunningCandidate(t, pool, runID)
	if changed, err := markInterrupted(context.Background(), pool, c, ReasonCellLost, time.Now(), ""); err != nil || !changed {
		t.Fatalf("mark the run cell-lost: changed = %v, err = %v", changed, err)
	}

	res := applyFromPod(t, NewIngester(pool, nil, nil), runID, intentKey, KindRunCompleted, "cell-a", replacementPod,
		RunCompletedPayload{Result: strPtr("SUCCESS")})

	if !res.CompletedOwner {
		t.Fatal("CompletedOwner = false, want true")
	}
	if s := readReplacementRunState(t, db, runID); s.result != "SUCCESS" || !s.supersededMark {
		t.Fatalf("run = %+v, want SUCCESS with the mark superseded", s)
	}
}

// TestReplacedPodEventsAfterTheTakeOverAreDropped proves that the
// replaced pod cannot take the run back. Its late events can reach the
// recorder after the replacement pod's events. The placement row does not
// name it, so its heartbeat, its facts, and its completion change nothing.
func TestReplacedPodEventsAfterTheTakeOverAreDropped(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, intentKey := seedReplacedControllerRun(t, pool, db, 0)
	g := NewIngester(pool, nil, nil)
	applyFromPod(t, g, runID, intentKey, KindRunHeartbeat, "cell-a", replacementPod, map[string]any{"seq": 1})
	assertForeignSender(t, applyRunningFacts(t, pool, runID, intentKey, replacementPod, "live", 5), false)

	if _, err := db.Exec(`UPDATE varroa.runs SET heartbeat_received_at = NULL WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("clear the heartbeat stamp: %v", err)
	}
	beat := applyFromPod(t, g, runID, intentKey, KindRunHeartbeat, "cell-a", replacedPod, map[string]any{"seq": 9})
	stale := applyRunningFacts(t, pool, runID, intentKey, replacedPod, "stale", 9)
	done := applyFromPod(t, g, runID, intentKey, KindRunCompleted, "cell-a", replacedPod,
		RunCompletedPayload{Result: strPtr("FAILURE")})

	if beat.AdoptedOwner || done.AdoptedOwner || done.CompletedOwner {
		t.Fatalf("replaced pod: heartbeat %+v, completion %+v, want no move and no owner completion", beat, done)
	}
	assertForeignSender(t, stale, true)
	assertRunningFacts(t, db, runID, "live")
	if owner := intentOwnerOf(t, db, runID); owner != replacementPod {
		t.Fatalf("intent owner = %q, want %q", owner, replacementPod)
	}
	s := readReplacementRunState(t, db, runID)
	if s.heartbeat || s.status != StatusRunning || !s.foreignCompletion {
		t.Fatalf("run = %+v, want RUNNING with no heartbeat stamp and one foreign completion", s)
	}
}

// TestNoTakeOverWithoutTheReplacementPodRule proves each condition of
// replacementPodSQL. In each case the sender's heartbeat is dropped, its
// facts are dropped, its completion is foreign, and the intent keeps the
// replaced pod. A cell run is the case the rule must never reach: a cell
// keeps its Jenkins home on ephemeral storage, so another pod of the cell
// cannot hold the build.
func TestNoTakeOverWithoutTheReplacementPodRule(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                string
		generation          int64
		placementGeneration int64
		placement           string
		cell                string
		sender              string
	}{
		{name: "the placement names another pod", placement: "pod-other", cell: "cell-a", sender: replacementPod},
		{name: "no placement row", cell: "cell-a", sender: replacementPod},
		{name: "another controller", placement: replacementPod, cell: "cell-b", sender: replacementPod},
		{name: "a placement row at another generation", placementGeneration: cellRunGeneration,
			placement: replacementPod, cell: "cell-a", sender: replacementPod},
		{name: "a cell run", generation: cellRunGeneration, placementGeneration: cellRunGeneration,
			placement: replacementPod, cell: "cell-a", sender: replacementPod},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pool, db := setupLedgerPool(t)
			runID, intentKey := seedOwnedRun(t, pool, replacedPod, tc.generation)
			if tc.placement != "" {
				seedControllerPlacement(t, db, tc.placementGeneration, tc.placement)
			}
			g := NewIngester(pool, nil, nil)

			beat := applyFromPod(t, g, runID, intentKey, KindRunHeartbeat, tc.cell, tc.sender, map[string]any{"seq": 1})
			var facts []ApplyResult
			for _, env := range runningFactEnvelopes(t, runID, intentKey, tc.sender, "foreign", 1) {
				env.Cell = tc.cell
				res, err := g.Apply(context.Background(), env)
				if err != nil {
					t.Fatalf("apply %s: %v", env.Kind, err)
				}
				facts = append(facts, res)
			}
			done := applyFromPod(t, g, runID, intentKey, KindRunCompleted, tc.cell, tc.sender,
				RunCompletedPayload{Result: strPtr("SUCCESS")})

			if beat.AdoptedOwner || done.AdoptedOwner || done.CompletedOwner {
				t.Fatalf("heartbeat %+v, completion %+v, want no move and no owner completion", beat, done)
			}
			assertForeignSender(t, facts, true)
			if owner := intentOwnerOf(t, db, runID); owner != replacedPod {
				t.Fatalf("intent owner = %q, want %q", owner, replacedPod)
			}
			s := readReplacementRunState(t, db, runID)
			if s.heartbeat || s.status != StatusRunning || s.ownerCompletion {
				t.Fatalf("run = %+v, want RUNNING with no heartbeat stamp and no owner completion", s)
			}
		})
	}
}

// TestNoTakeOverFromAnotherClusterOrNamespace proves the cluster and the
// namespace conditions of replacementPodSQL. The placement row names the
// sender, but the sender names another cluster or another namespace than
// the intent. Its heartbeat and its completion move nothing.
func TestNoTakeOverFromAnotherClusterOrNamespace(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, cluster, namespace string
	}{
		{name: "another cluster", cluster: "member-b", namespace: testNamespace},
		{name: "another namespace", cluster: testCluster, namespace: "other-ns"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pool, db := setupLedgerPool(t)
			runID, intentKey := seedReplacedControllerRun(t, pool, db, 0)
			g := NewIngester(pool, nil, nil)
			apply := func(kind string, payload any) ApplyResult {
				t.Helper()
				env := newTestEnvelope(t, runID, intentKey, kind, payload)
				env.Cluster, env.Namespace, env.CellInstance = tc.cluster, tc.namespace, replacementPod
				res, err := g.Apply(context.Background(), env)
				if err != nil {
					t.Fatalf("apply %s: %v", kind, err)
				}
				return res
			}

			beat := apply(KindRunHeartbeat, map[string]any{"seq": 1})
			done := apply(KindRunCompleted, RunCompletedPayload{Result: strPtr("SUCCESS")})

			if beat.AdoptedOwner || done.AdoptedOwner || done.CompletedOwner {
				t.Fatalf("heartbeat %+v, completion %+v, want no move and no owner completion", beat, done)
			}
			if owner := intentOwnerOf(t, db, runID); owner != replacedPod {
				t.Fatalf("intent owner = %q, want %q", owner, replacedPod)
			}
			if s := readReplacementRunState(t, db, runID); s.heartbeat || s.status != StatusRunning {
				t.Fatalf("run = %+v, want RUNNING with no heartbeat stamp", s)
			}
		})
	}
}

// TestNoTakeOverOfARunWithNoOwner proves the owner condition of
// replacementPodSQL. An intent that names no owner admits every sender, so
// the rule moves nothing, and the intent keeps its empty owner.
func TestNoTakeOverOfARunWithNoOwner(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, intentKey := seedOwnedRun(t, pool, "", 0)
	seedControllerPlacement(t, db, 0, replacementPod)
	g := NewIngester(pool, nil, nil)

	beat := applyFromPod(t, g, runID, intentKey, KindRunHeartbeat, "cell-a", replacementPod, map[string]any{"seq": 1})
	done := applyFromPod(t, g, runID, intentKey, KindRunCompleted, "cell-a", replacementPod,
		RunCompletedPayload{Result: strPtr("SUCCESS")})

	if beat.AdoptedOwner || done.AdoptedOwner {
		t.Fatalf("heartbeat %+v, completion %+v, want no move", beat, done)
	}
	if owner := intentOwnerOf(t, db, runID); owner != "" {
		t.Fatalf("intent owner = %q, want no owner", owner)
	}
}

// TestReplacementPodTakeOverSignalsTheOwnerMove proves that the move
// sends one owner move signal after its commit, so every gateway replica
// drops the replaced pod's cached match. A second event from the same pod
// moves nothing and sends nothing.
func TestReplacementPodTakeOverSignalsTheOwnerMove(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestActionConn(t)
	received := collectLedgerMessages(t, conn, db)
	runID, intentKey := seedReplacedControllerRun(t, pool, db, 0)
	g := NewIngester(pool, conn, nil)

	applyFromPod(t, g, runID, intentKey, KindRunHeartbeat, "cell-a", replacementPod, map[string]any{"seq": 1})
	if m := expectOwnerMove(t, received, runID.String()); !m.found || m.instance != replacementPod {
		t.Fatalf("ledger read on the signal = %+v, want the new owner %s", m, replacementPod)
	}

	applyFromPod(t, g, runID, intentKey, KindRunHeartbeat, "cell-a", replacementPod, map[string]any{"seq": 2})
	expectNoLedgerMessage(t, conn, received)
}

// TestOwnerLookupNamesTheReplacementPod proves the ledger owner answer.
// The gateway decides on the first chunk and on a completion before any
// write of the replacement pod reaches the recorder, so the answer
// applies the same rule for the caller it names. The answer writes
// nothing.
func TestOwnerLookupNamesTheReplacementPod(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, _ := seedReplacedControllerRun(t, pool, db, 0)
	caller := func(cell, instance string) ownerLookupRequest {
		return ownerLookupRequest{RunID: runID.String(), Cluster: testCluster, Namespace: testNamespace, Cell: cell, Instance: instance}
	}

	cases := []struct {
		name string
		req  ownerLookupRequest
		want string
	}{
		{name: "the replacement pod", req: caller("cell-a", replacementPod), want: replacementPod},
		{name: "the replaced pod", req: caller("cell-a", replacedPod), want: replacedPod},
		{name: "a pod the placement does not name", req: caller("cell-a", "pod-other"), want: replacedPod},
		{name: "another controller", req: caller("cell-b", replacementPod), want: replacedPod},
		{name: "no caller", req: ownerLookupRequest{RunID: runID.String()}, want: replacedPod},
	}
	for _, tc := range cases {
		resp, err := lookupOwner(context.Background(), pool, tc.req)
		if err != nil {
			t.Fatalf("%s: lookupOwner: %v", tc.name, err)
		}
		if !resp.Found || resp.Instance != tc.want {
			t.Fatalf("%s: answer = %+v, want instance %q", tc.name, resp, tc.want)
		}
	}
	if owner := intentOwnerOf(t, db, runID); owner != replacedPod {
		t.Fatalf("intent owner = %q, want %q: the answer must write nothing", owner, replacedPod)
	}

	cellPool, cellDB := setupLedgerPool(t)
	cellRun, _ := seedReplacedControllerRun(t, cellPool, cellDB, cellRunGeneration)
	resp, err := lookupOwner(context.Background(), cellPool, ownerLookupRequest{
		RunID: cellRun.String(), Cluster: testCluster, Namespace: testNamespace, Cell: "cell-a", Instance: replacementPod,
	})
	if err != nil {
		t.Fatalf("cell run: lookupOwner: %v", err)
	}
	if resp.Instance != replacedPod {
		t.Fatalf("cell run: answer = %+v, want instance %q", resp, replacedPod)
	}
}

// TestSweepLeavesTheRunOfAReplacedControllerPod proves that a pod read of
// the replaced pod ends no run of a controller with no cell role. The
// operator confirms that pod gone on the tick the replacement appears,
// and Jenkins on the replacement resumes the build from the volume. The
// same reading still ends a cell run cell-lost.
func TestSweepLeavesTheRunOfAReplacedControllerPod(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		generation int64
		wantMarked int
	}{
		{name: "a controller with no cell role", generation: 0, wantMarked: 0},
		{name: "a cell", generation: cellRunGeneration, wantMarked: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pool, _ := setupLedgerPool(t)
			runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), tc.generation))
			if got := fetchRunningCandidate(t, pool, runID).generationZero; got != (tc.generation == 0) {
				t.Fatalf("candidate generationZero = %v, want %v", got, tc.generation == 0)
			}

			now := time.Now()
			confirmed := now.Add(-time.Minute)
			row := resolveLivenessMembers(tenantPlacementLivenessRow{
				cell: "cell-a", cellInstance: "pod-b", cellAlive: true, cellAliveAt: &now,
				deadInstances: []TenantSyncDeadInstance{{Instance: "pod-a", LastSeenAt: confirmed, ConfirmedGoneAt: &confirmed}},
			})
			marked, err := MarkMovedRunsForTenant(context.Background(), pool, nil, nil, nil, nil, nil, nil,
				testCluster, testTenant, row, now, testGrace, "", nil)
			if err != nil {
				t.Fatalf("MarkMovedRunsForTenant: %v", err)
			}
			if marked != tc.wantMarked {
				t.Fatalf("marked = %d, want %d", marked, tc.wantMarked)
			}
		})
	}
}

// TestEvaluateInterruptionOfAControllerWithNoCellRole proves the reading
// of a run that a controller with no cell role owns. A pod read and a
// pod change prove no loss, because the next pod resumes the build. A
// controller that stays silent still ends the run, and so does the
// heartbeat-lost window.
func TestEvaluateInterruptionOfAControllerWithNoCellRole(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-time.Minute)
	confirmed := now.Add(-time.Minute)
	tenMinutes := now.Add(-10 * time.Minute)
	twentyMinutes := now.Add(-20 * time.Minute)
	pastWindow := now.Add(-liveOwnerSilentWindow - time.Minute)

	confirmedRow := tenantPlacementLivenessRow{cell: "cell-a", cellInstance: "pod-b", cellAlive: true, cellAliveAt: &fresh,
		deadInstances: []TenantSyncDeadInstance{{Instance: "pod-a", LastSeenAt: confirmed, ConfirmedGoneAt: &confirmed}}}
	movedRow := tenantPlacementLivenessRow{cell: "cell-a", cellInstance: "pod-b", cellAlive: true, cellAliveAt: &fresh,
		deadInstances: []TenantSyncDeadInstance{{Instance: "pod-a", LastSeenAt: tenMinutes}}}
	silentRow := tenantPlacementLivenessRow{cell: "cell-a", cellInstance: "pod-b", cellAlive: false, cellAliveAt: &fresh,
		cellLastSeenAt: &twentyMinutes}

	cases := []struct {
		name       string
		c          runInterruptCandidate
		row        tenantPlacementLivenessRow
		wantOK     bool
		wantReason string
	}{
		{
			name: "the replaced pod is confirmed gone",
			c: runInterruptCandidate{status: StatusRunning, cell: "cell-a", cellInstance: "pod-a",
				heartbeatReceivedAt: &fresh, generationZero: true},
			row: confirmedRow,
		},
		{
			name: "a cell run on a pod confirmed gone",
			c: runInterruptCandidate{status: StatusRunning, cell: "cell-a", cellInstance: "pod-a",
				heartbeatReceivedAt: &fresh},
			row:    confirmedRow,
			wantOK: true, wantReason: ReasonCellLost,
		},
		{
			name: "the pod changed past the moved window",
			c: runInterruptCandidate{status: StatusRunning, cell: "cell-a", cellInstance: "pod-a",
				heartbeatReceivedAt: &tenMinutes, generationZero: true},
			row: movedRow,
		},
		{
			name: "the controller is silent",
			c: runInterruptCandidate{status: StatusRunning, cell: "cell-a", cellInstance: "pod-a",
				heartbeatReceivedAt: &twentyMinutes, generationZero: true},
			row:    silentRow,
			wantOK: true, wantReason: ReasonNoHeartbeat,
		},
		{
			name: "the run is silent past the heartbeat-lost window",
			c: runInterruptCandidate{status: StatusRunning, cell: "cell-a", cellInstance: "pod-a",
				heartbeatReceivedAt: &pastWindow, generationZero: true},
			row:    confirmedRow,
			wantOK: true, wantReason: ReasonHeartbeatLost,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, ok := evaluateInterruption(now, tc.c, resolveLivenessMembers(tc.row), true, testGrace)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v (reason %q), want %v", ok, reason, tc.wantOK)
			}
			if ok && reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}
