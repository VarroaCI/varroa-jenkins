package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// meshDeadInstance is the cell instance every mesh-takeover test names
// as the confirmed-gone owner of its running build.
const meshDeadInstance = "pod-dead"

// meshTargetCell is the cell every seeded placement in this file names.
const meshTargetCell = "cell-b"

// meshBuildRecordPath and meshProgramPath are the two stored paths a
// seeded mesh-takeover run carries. The build record is stored first,
// so a test that reads the reordered list proves the reorder rather
// than an incidental input order.
const (
	meshBuildRecordPath = "jobs/t/jobs/j/builds/7/build.xml"
	meshProgramPath     = "jobs/t/jobs/j/builds/7/program.dat"
)

// seedMeshRunningRun claims and starts one RUNNING run owned by the
// confirmed-gone instance, then stamps its payload with a usable mesh
// record: a state_at anchor and two stored files.
func seedMeshRunningRun(t *testing.T, pool *pgxpool.Pool, db *sql.DB) uuid.UUID {
	t.Helper()
	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	req.CellInstance = meshDeadInstance
	runID := seedRunningRun(t, pool, req)
	if _, err := db.Exec(`
		UPDATE varroa.runs
		   SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('mesh', jsonb_build_object(
		         'state_at', now(),
		         'files', jsonb_build_array(
		             jsonb_build_object('relpath', $2::text, 'bytes', 100, 'seq', 1, 'state_gen', 1),
		             jsonb_build_object('relpath', $3::text, 'bytes', 200, 'seq', 1, 'state_gen', 1))))
		 WHERE run_id = $1
	`, runID, meshBuildRecordPath, meshProgramPath); err != nil {
		t.Fatalf("seed mesh state: %v", err)
	}
	return runID
}

// seedMeshPlacement inserts the tenant's placement row naming
// cellInstance (the live target the takeover may dispatch to, empty for
// no target) and rehydratedInstance, with meshDeadInstance listed dead
// and confirmed gone.
func seedMeshPlacement(t *testing.T, db *sql.DB, cellInstance, rehydratedInstance string) {
	t.Helper()
	cell := meshTargetCell
	now := time.Now().UTC()
	confirmedAt := now.Add(-45 * time.Second)
	dead := []TenantSyncDeadInstance{{Instance: meshDeadInstance, LastSeenAt: now.Add(-time.Minute), ConfirmedGoneAt: &confirmedAt}}
	deadJSON, err := json.Marshal(dead)
	if err != nil {
		t.Fatalf("marshal dead instances: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements
			(cluster, tenant, namespace, cell, generation, cell_instance, rehydrated_instance, cell_alive, dead_instances)
		VALUES ($1, $2, $3, $4, 1, $5, $6, true, $7::jsonb)
	`, testCluster, testTenant, testNamespace, cell, cellInstance, rehydratedInstance, string(deadJSON)); err != nil {
		t.Fatalf("seed mesh placement: %v", err)
	}
}

// seedMeshFleetLock inserts one held fleet_locks row naming runID and
// cellInstance as its owner.
func seedMeshFleetLock(t *testing.T, db *sql.DB, runID uuid.UUID, cellInstance string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.fleet_locks
			(name, run_id, cluster, tenant, job_path, cell_instance, state, fence_token, waiting_since, acquired_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'held', 1, now(), now(), now() + interval '1 hour')
	`, testTenant+"/deploy-lock", runID.String(), testCluster, testTenant, testJobPath, cellInstance); err != nil {
		t.Fatalf("seed fleet lock: %v", err)
	}
}

// readMeshResume reads one run's payload->'mesh'->'resume' object as
// text fields, answering empty strings for an absent key.
func readMeshResume(t *testing.T, db *sql.DB, runID uuid.UUID) (armedAt, armExpires, instance string) {
	t.Helper()
	if err := db.QueryRow(`
		SELECT COALESCE(payload->'mesh'->'resume'->>'armed_at', ''),
		       COALESCE(payload->'mesh'->'resume'->>'arm_expires', ''),
		       COALESCE(payload->'mesh'->'resume'->>'instance', '')
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&armedAt, &armExpires, &instance); err != nil {
		t.Fatalf("read mesh resume: %v", err)
	}
	return
}

// runTakeover runs one takeover round for the test tenant and fails the
// test on error.
func runTakeover(t *testing.T, pool *pgxpool.Pool, conn *bus.Conn, metrics *InterruptedMetrics, actionMetrics *ActionMetrics) int {
	t.Helper()
	published, err := TakeOverLostRuns(context.Background(), pool, conn, metrics, actionMetrics,
		testCluster, testTenant, testGrace, time.Now(), nil)
	if err != nil {
		t.Fatalf("TakeOverLostRuns: %v", err)
	}
	return published
}

// TestTakeOverLostRunsArmsWithNoTarget asserts a confirmed-gone run with
// state is armed alone: the mesh.resume object gains armed_at and
// arm_expires, and no instance, while the tenant reports no live
// replacement.
func TestTakeOverLostRunsArmsWithNoTarget(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestActionConn(t)
	reader := useManualMeter(t)

	runID := seedMeshRunningRun(t, pool, db)
	seedMeshPlacement(t, db, "", "")

	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	if got := runTakeover(t, pool, conn, metrics, nil); got != 0 {
		t.Fatalf("published = %d, want 0: no target exists yet", got)
	}

	armedAt, armExpires, instance := readMeshResume(t, db, runID)
	if armedAt == "" || armExpires == "" {
		t.Fatalf("armed_at=%q arm_expires=%q, want both set", armedAt, armExpires)
	}
	if instance != "" {
		t.Fatalf("instance = %q, want absent on an armed-only resume", instance)
	}
	if got := sumCounterByAttr(t, reader, "recorder_run_resume_total", "outcome", resumeOutcomeArmed); got != 1 {
		t.Fatalf("recorder_run_resume_total{outcome=armed} = %d, want 1", got)
	}
}

// TestTakeOverLostRunsArmsOnlyOnce asserts a second round over the same
// unarmed run leaves the first arm unchanged and arms nothing more.
func TestTakeOverLostRunsArmsOnlyOnce(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestActionConn(t)
	reader := useManualMeter(t)

	runID := seedMeshRunningRun(t, pool, db)
	seedMeshPlacement(t, db, "", "")

	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	if got := runTakeover(t, pool, conn, metrics, nil); got != 0 {
		t.Fatalf("first round published = %d, want 0", got)
	}
	firstArmedAt, _, _ := readMeshResume(t, db, runID)
	if firstArmedAt == "" {
		t.Fatal("want the first round to arm the run")
	}

	if got := runTakeover(t, pool, conn, metrics, nil); got != 0 {
		t.Fatalf("second round published = %d, want 0", got)
	}
	secondArmedAt, _, _ := readMeshResume(t, db, runID)
	if secondArmedAt != firstArmedAt {
		t.Fatalf("armed_at changed from %q to %q, want the first arm kept", firstArmedAt, secondArmedAt)
	}
	if got := sumCounterByAttr(t, reader, "recorder_run_resume_total", "outcome", resumeOutcomeArmed); got != 1 {
		t.Fatalf("recorder_run_resume_total{outcome=armed} = %d, want 1: a second round must arm nothing", got)
	}
}

// TestTakeOverLostRunsDispatchesToLiveTarget asserts one transaction
// moves the run intent's owner, the fleet lock's owner, and the run
// row, and that the published resume_run action's Files list ends with
// the build record.
func TestTakeOverLostRunsDispatchesToLiveTarget(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestActionConn(t)
	received := collectActions(t, conn)

	runID := seedMeshRunningRun(t, pool, db)
	seedMeshFleetLock(t, db, runID, meshDeadInstance)
	seedMeshPlacement(t, db, "pod-live", "pod-live")

	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	actionMetrics, err := NewActionMetrics()
	if err != nil {
		t.Fatalf("new action metrics: %v", err)
	}
	if got := runTakeover(t, pool, conn, metrics, actionMetrics); got != 1 {
		t.Fatalf("published = %d, want 1", got)
	}

	var intentCell, intentInstance string
	if err := db.QueryRow(`SELECT cell, cell_instance FROM varroa.run_intents WHERE run_id = $1`, runID).
		Scan(&intentCell, &intentInstance); err != nil {
		t.Fatalf("read run_intents: %v", err)
	}
	if intentCell != "cell-b" || intentInstance != "pod-live" {
		t.Fatalf("run_intents cell=%q instance=%q, want cell-b/pod-live", intentCell, intentInstance)
	}

	var lockInstance string
	if err := db.QueryRow(`SELECT cell_instance FROM varroa.fleet_locks WHERE run_id = $1`, runID.String()).
		Scan(&lockInstance); err != nil {
		t.Fatalf("read fleet_locks: %v", err)
	}
	if lockInstance != "pod-live" {
		t.Fatalf("fleet lock cell_instance = %q, want pod-live", lockInstance)
	}

	var runCell string
	if err := db.QueryRow(`SELECT cell FROM varroa.runs WHERE run_id = $1`, runID).Scan(&runCell); err != nil {
		t.Fatalf("read runs: %v", err)
	}
	_, _, resumeInstance := readMeshResume(t, db, runID)
	if runCell != "cell-b" || resumeInstance != "pod-live" {
		t.Fatalf("run cell=%q resume instance=%q, want cell-b/pod-live", runCell, resumeInstance)
	}

	select {
	case a := <-received:
		if a.Kind != LedgerActionResumeRun || a.Instance != "pod-live" || a.RunID != runID.String() {
			t.Fatalf("action = %+v, want a resume_run to pod-live for %s", a, runID)
		}
		if len(a.Files) == 0 || a.Files[len(a.Files)-1] != meshBuildRecordPath {
			t.Fatalf("files = %v, want the build record last", a.Files)
		}
		// The action carries the run's own deadline, so a command that
		// waits in the queue can be refused at pickup.
		if a.Deadline == nil {
			t.Fatal("action carries no deadline, want the run's own deadline")
		}
		if left := time.Until(*a.Deadline); left <= 0 || left > resumeDeadline {
			t.Fatalf("deadline is %v away, want at most %v", left, resumeDeadline)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("want a resume_run action published")
	}
}

// TestTakeOverLostRunsSecondDispatchPublishesNothing asserts a repeat
// round over an already-dispatched run moves nothing further.
func TestTakeOverLostRunsSecondDispatchPublishesNothing(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestActionConn(t)
	received := collectActions(t, conn)

	runID := seedMeshRunningRun(t, pool, db)
	seedMeshFleetLock(t, db, runID, meshDeadInstance)
	seedMeshPlacement(t, db, "pod-live", "pod-live")

	if got := runTakeover(t, pool, conn, nil, nil); got != 1 {
		t.Fatalf("first round published = %d, want 1", got)
	}
	<-received

	if got := runTakeover(t, pool, conn, nil, nil); got != 0 {
		t.Fatalf("second round published = %d, want 0", got)
	}
	select {
	case a := <-received:
		t.Fatalf("want no second action, got %+v", a)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestDispatchMeshResumeRollsBackOnRealCompletion asserts a run that
// completes between the candidate read and the dispatch transaction
// rolls the whole transaction back: statement 3 changes no row, so the
// first two statements' moves are undone.
func TestDispatchMeshResumeRollsBackOnRealCompletion(t *testing.T) {
	pool, db := setupLedgerPool(t)

	runID := seedMeshRunningRun(t, pool, db)
	seedMeshFleetLock(t, db, runID, meshDeadInstance)
	seedMeshPlacement(t, db, "pod-live", "pod-live")

	now := time.Now()
	expires := now.Add(resumeArmWindow)
	if _, err := db.Exec(`
		UPDATE varroa.runs
		   SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('mesh',
		         COALESCE(payload->'mesh', '{}'::jsonb) || jsonb_build_object('resume',
		           jsonb_build_object('armed_at', $2::timestamptz, 'arm_expires', $3::timestamptz)))
		 WHERE run_id = $1
	`, runID, now, expires); err != nil {
		t.Fatalf("seed the arm: %v", err)
	}

	candidates, err := selectMeshCandidates(context.Background(), pool, testCluster, testTenant)
	if err != nil {
		t.Fatalf("selectMeshCandidates: %v", err)
	}
	var candidate meshCandidate
	for _, c := range candidates {
		if c.runID == runID.String() {
			candidate = c
		}
	}
	if candidate.runID == "" {
		t.Fatalf("candidate for %s not found among %+v", runID, candidates)
	}
	if candidate.mesh == nil || candidate.mesh.Resume == nil {
		t.Fatalf("candidate mesh = %+v, want an armed resume", candidate.mesh)
	}

	// A real completion lands between the read above and the dispatch
	// transaction below: the race the whole-transaction rollback guards
	// against.
	if _, err := db.Exec(`UPDATE varroa.runs SET status = 'COMPLETED', result = 'SUCCESS' WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("simulate a real completion: %v", err)
	}

	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	_, ok, err := dispatchMeshResume(context.Background(), pool, metrics, candidate, time.Now())
	if err != nil {
		t.Fatalf("dispatchMeshResume: %v", err)
	}
	if ok {
		t.Fatal("want ok=false: the transaction must roll back on a real completion")
	}

	var intentInstance string
	if err := db.QueryRow(`SELECT cell_instance FROM varroa.run_intents WHERE run_id = $1`, runID).Scan(&intentInstance); err != nil {
		t.Fatalf("read run_intents: %v", err)
	}
	if intentInstance != meshDeadInstance {
		t.Fatalf("run_intents cell_instance = %q, want the dead instance %q kept: statement 1 must roll back",
			intentInstance, meshDeadInstance)
	}

	var lockInstance string
	if err := db.QueryRow(`SELECT cell_instance FROM varroa.fleet_locks WHERE run_id = $1`, runID.String()).Scan(&lockInstance); err != nil {
		t.Fatalf("read fleet_locks: %v", err)
	}
	if lockInstance != meshDeadInstance {
		t.Fatalf("fleet lock cell_instance = %q, want the dead instance %q kept: statement 2 must roll back",
			lockInstance, meshDeadInstance)
	}

	var status, result string
	if err := db.QueryRow(`SELECT status, result FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status, &result); err != nil {
		t.Fatalf("read runs: %v", err)
	}
	if status != "COMPLETED" || result != "SUCCESS" {
		t.Fatalf("status=%q result=%q, want the real completion kept", status, result)
	}
}

// TestResumeFileListOrdersBuildRecordLast asserts resumeFileList always
// places a build.xml path last, regardless of its position in the
// input.
func TestResumeFileListOrdersBuildRecordLast(t *testing.T) {
	files := []runStateFile{
		{RelPath: meshBuildRecordPath},
		{RelPath: meshProgramPath},
		{RelPath: "jobs/t/jobs/j/builds/7/log"},
	}
	got := resumeFileList(files)
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	if got[len(got)-1] != meshBuildRecordPath {
		t.Fatalf("last entry = %q, want the build record", got[len(got)-1])
	}
	rest := map[string]bool{meshProgramPath: true, "jobs/t/jobs/j/builds/7/log": true}
	for _, f := range got[:len(got)-1] {
		if !rest[f] {
			t.Fatalf("unexpected non-last entry %q", f)
		}
	}
}
