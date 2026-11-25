package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedMeshSlotPlacement inserts the tenant's placement row with two
// active slots at the given generation. Slot 1 holds the live
// replacement, and the dead instance is listed confirmed gone.
func seedMeshSlotPlacement(t *testing.T, db *sql.DB, generation int64, slotOneInstance string,
	jobHomes map[string]int32) {
	t.Helper()
	seedMeshSlotPlacementIn(t, db, generation, testNamespace, slotOneInstance, jobHomes)
}

// seedMeshSlotPlacementIn is seedMeshSlotPlacement with slot 1's cell in
// slotOneNamespace. Slot 0 stays in testNamespace.
func seedMeshSlotPlacementIn(t *testing.T, db *sql.DB, generation int64, slotOneNamespace, slotOneInstance string,
	jobHomes map[string]int32) {
	t.Helper()
	now := time.Now().UTC()
	confirmedAt := now.Add(-45 * time.Second)
	dead := []TenantSyncDeadInstance{{Instance: meshDeadInstance, LastSeenAt: now.Add(-time.Minute), ConfirmedGoneAt: &confirmedAt}}
	deadJSON, err := json.Marshal(dead)
	if err != nil {
		t.Fatalf("marshal dead instances: %v", err)
	}
	members := []member{
		{Cell: "cell-slot0", Namespace: testNamespace, Role: MemberRoleActive, Slot: slotPtr(0),
			Instance: "pod-slot0", RehydratedInstance: "pod-slot0"},
		{Cell: meshTargetCell, Namespace: slotOneNamespace, Role: MemberRoleActive, Slot: slotPtr(1),
			Instance: slotOneInstance, RehydratedInstance: slotOneInstance},
	}
	membersJSON, err := json.Marshal(members)
	if err != nil {
		t.Fatalf("marshal members: %v", err)
	}
	homesJSON, err := json.Marshal(nonNilJobHomes(jobHomes))
	if err != nil {
		t.Fatalf("marshal job homes: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements
			(cluster, tenant, namespace, cell, generation, cell_instance, rehydrated_instance,
			 cell_alive, dead_instances, members, job_homes)
		VALUES ($1, $2, $3, 'cell-slot0', $4, 'pod-slot0', 'pod-slot0', true, $5::jsonb, $6::jsonb, $7::jsonb)
	`, testCluster, testTenant, testNamespace, generation, string(deadJSON),
		string(membersJSON), string(homesJSON)); err != nil {
		t.Fatalf("seed mesh slot placement: %v", err)
	}
}

// readLockRow reads one fleet_locks row's owner and generation.
func readLockRow(t *testing.T, db *sql.DB, runID uuid.UUID) (instance string, generation int64) {
	t.Helper()
	if err := db.QueryRow(`
		SELECT cell_instance, placement_generation FROM varroa.fleet_locks WHERE run_id = $1
	`, runID.String()).Scan(&instance, &generation); err != nil {
		t.Fatalf("read fleet_locks: %v", err)
	}
	return
}

// TestTakeoverResumesOnTheJobsHomeSlot asserts a run of a job homed on
// slot 1 resumes on slot 1's holder, and never on slot 0's. It also
// asserts the move raises the lock's stored generation, so the resumed
// run renews its own lock after the takeover.
func TestTakeoverResumesOnTheJobsHomeSlot(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestActionConn(t)
	received := collectActions(t, conn)

	runID := seedMeshRunningRun(t, pool, db)
	seedMeshFleetLock(t, db, runID)
	seedMeshSlotPlacement(t, db, 7, "pod-live", map[string]int32{testJobPath: 1})

	if got := runTakeover(t, pool, conn, nil, nil); got != 1 {
		t.Fatalf("published = %d, want 1", got)
	}

	var intentCell, intentInstance string
	if err := db.QueryRow(`SELECT cell, cell_instance FROM varroa.run_intents WHERE run_id = $1`, runID).
		Scan(&intentCell, &intentInstance); err != nil {
		t.Fatalf("read run_intents: %v", err)
	}
	if intentCell != meshTargetCell || intentInstance != "pod-live" {
		t.Fatalf("run_intents cell=%q instance=%q, want %s/pod-live", intentCell, intentInstance, meshTargetCell)
	}

	instance, generation := readLockRow(t, db, runID)
	if instance != "pod-live" {
		t.Fatalf("fleet lock cell_instance = %q, want pod-live", instance)
	}
	if generation != 7 {
		t.Fatalf("fleet lock placement_generation = %d, want 7: the move raises the epoch", generation)
	}

	select {
	case a := <-received:
		if a.Kind != LedgerActionResumeRun || a.Controller != meshTargetCell {
			t.Fatalf("action = %+v, want a resume_run on %s", a, meshTargetCell)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("want a resume_run action published")
	}
}

// TestTakeoverRepeatWritesNothing asserts a repeat of the ownership move
// changes no row: the guard names the dead instance, which the first move
// already replaced.
func TestTakeoverRepeatWritesNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestActionConn(t)
	received := collectActions(t, conn)

	runID := seedMeshRunningRun(t, pool, db)
	seedMeshFleetLock(t, db, runID)
	seedMeshSlotPlacement(t, db, 7, "pod-live", map[string]int32{testJobPath: 1})

	if got := runTakeover(t, pool, conn, nil, nil); got != 1 {
		t.Fatalf("first round published = %d, want 1", got)
	}
	<-received

	// The tenant generation rises again, and the round runs once more.
	if _, err := db.Exec(`UPDATE varroa.tenant_placements SET generation = 9`); err != nil {
		t.Fatalf("raise the generation: %v", err)
	}
	if got := runTakeover(t, pool, conn, nil, nil); got != 0 {
		t.Fatalf("second round published = %d, want 0", got)
	}

	instance, generation := readLockRow(t, db, runID)
	if instance != "pod-live" {
		t.Fatalf("fleet lock cell_instance = %q, want pod-live", instance)
	}
	if generation != 7 {
		t.Fatalf("fleet lock placement_generation = %d, want 7: a repeat writes nothing", generation)
	}
}

// meshOtherNamespace is the namespace of the slot-1 cell in the test of a
// move between two namespaces.
const meshOtherNamespace = "ns-other"

// TestTakeoverMovesTheNamespaceWithTheCell asserts a takeover to a cell
// in another namespace moves the namespace with the cell, on run_intents
// and on runs. It then asserts that each reader of the moved pair names
// the target controller.
func TestTakeoverMovesTheNamespaceWithTheCell(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	pool, db := setupLedgerPool(t)
	conn := newTestActionConn(t)
	received := collectActions(t, conn)

	runID := seedMeshRunningRun(t, pool, db)
	seedMeshSlotPlacementIn(t, db, 7, meshOtherNamespace, "pod-live", map[string]int32{testJobPath: 1})

	if got := runTakeover(t, pool, conn, nil, nil); got != 1 {
		t.Fatalf("published = %d, want 1", got)
	}

	var intentNamespace, intentCell, intentInstance string
	if err := db.QueryRow(`SELECT namespace, cell, cell_instance FROM varroa.run_intents WHERE run_id = $1`, runID).
		Scan(&intentNamespace, &intentCell, &intentInstance); err != nil {
		t.Fatalf("read run_intents: %v", err)
	}
	if intentNamespace != meshOtherNamespace || intentCell != meshTargetCell || intentInstance != "pod-live" {
		t.Fatalf("run_intents = %s/%s/%s, want %s/%s/pod-live",
			intentNamespace, intentCell, intentInstance, meshOtherNamespace, meshTargetCell)
	}

	var runNamespace, runCell string
	if err := db.QueryRow(`SELECT namespace, cell FROM varroa.runs WHERE run_id = $1`, runID).
		Scan(&runNamespace, &runCell); err != nil {
		t.Fatalf("read runs: %v", err)
	}
	if runNamespace != meshOtherNamespace || runCell != meshTargetCell {
		t.Fatalf("runs = %s/%s, want %s/%s", runNamespace, runCell, meshOtherNamespace, meshTargetCell)
	}

	select {
	case a := <-received:
		if a.Kind != LedgerActionResumeRun || a.Namespace != meshOtherNamespace || a.Controller != meshTargetCell {
			t.Fatalf("action = %+v, want a resume_run on %s/%s", a, meshOtherNamespace, meshTargetCell)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("want a resume_run action published")
	}

	// The gateway admits the target's log and state chunks only when the
	// owner tuple names the target's namespace.
	owner, err := lookupOwner(ctx, pool, ownerLookupRequest{RunID: runID.String()})
	if err != nil {
		t.Fatalf("lookupOwner: %v", err)
	}
	if owner.Namespace != meshOtherNamespace || owner.Cell != meshTargetCell || owner.Instance != "pod-live" {
		t.Fatalf("owner = %+v, want %s/%s/pod-live", owner, meshOtherNamespace, meshTargetCell)
	}

	// The BFF sends a cancel, an input answer, and a log tail request to
	// the controller of the run row.
	reader := NewReader(pool)
	run, _, err := reader.GetRun(ctx, runID.String())
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Namespace != meshOtherNamespace || run.Cell != meshTargetCell {
		t.Fatalf("GetRun = %s/%s, want %s/%s", run.Namespace, run.Cell, meshOtherNamespace, meshTargetCell)
	}

	// The door link of the target controller finds the run by its own
	// namespace and cell.
	page, err := reader.ListRuns(ctx, RunQuery{
		Cluster: testCluster, Tenant: testTenant, JobPath: testJobPath,
		Namespace: meshOtherNamespace, Cell: meshTargetCell, Limit: 1,
	})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(page.Runs) != 1 || page.Runs[0].RunID != runID.String() {
		t.Fatalf("ListRuns on %s/%s = %+v, want the moved run", meshOtherNamespace, meshTargetCell, page.Runs)
	}

	// The interruption sweep reads the roll cause and addresses its
	// cancel_run, end_commit_status, and release_agents actions from
	// this pair.
	candidate := fetchRunningCandidate(t, pool, runID)
	if candidate.namespace != meshOtherNamespace || candidate.cell != meshTargetCell {
		t.Fatalf("sweep candidate = %s/%s, want %s/%s",
			candidate.namespace, candidate.cell, meshOtherNamespace, meshTargetCell)
	}

	// The concurrency cancel addresses its cancel_run action from this
	// pair.
	cancels, err := selectConcurrencyCancelCandidates(ctx, pool, testCluster, testTenant, testJobPath,
		time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("selectConcurrencyCancelCandidates: %v", err)
	}
	if len(cancels) != 1 || cancels[0].namespace != meshOtherNamespace || cancels[0].cell != meshTargetCell {
		t.Fatalf("concurrency cancel candidates = %+v, want one on %s/%s", cancels, meshOtherNamespace, meshTargetCell)
	}
}

// TestResumeTargetAnswersTheHomeSlotMember asserts resumeTargetForRun
// resolves the run's own job path, so a slot-1 job never resumes on slot
// 0's cell.
func TestResumeTargetAnswersTheHomeSlotMember(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedMeshSlotPlacement(t, db, 7, "pod-live", map[string]int32{testJobPath: 1})

	assertTarget := func(jobPath, wantCell string) {
		t.Helper()
		tx, err := pool.Begin(context.Background())
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		got, refusal, err := resumeTargetForRun(context.Background(), tx, meshCandidate{
			cluster: testCluster, tenant: testTenant, jobPath: jobPath, cellInstance: meshDeadInstance,
		})
		if err != nil {
			t.Fatalf("resumeTargetForRun: %v", err)
		}
		if refusal != resumeRefusalNone || got.cell != wantCell {
			t.Fatalf("%s resolved to %+v (refusal=%q), want %s", jobPath, got, refusal, wantCell)
		}
	}
	assertTarget(testJobPath, meshTargetCell)
	// acme/api holds no entry, and the jump hash homes it on slot 0.
	assertTarget("acme/api", "cell-slot0")
}
