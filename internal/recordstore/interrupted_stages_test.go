package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The stage tree each test in this file seeds: a finished Checkout stage,
// an open Build stage, an open parallel branch in Build, and an open Test
// stage in that branch.
const (
	stageCheckout = "2"
	stageBuild    = "3"
	stageBranch   = "5"
	stageTest     = "6"
)

// openStageIDs are the stages of the tree that are open when the mark comes.
var openStageIDs = []string{stageBuild, stageBranch, stageTest}

// checkoutDurationMs is the duration of the finished Checkout stage.
const checkoutDurationMs = int64(60000)

// stageTreePayloads answers the run.stage payloads of the stage tree, with
// each open stage at status.
func stageTreePayloads(status string) []RunStagePayload {
	started := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Millisecond)
	ended := started.Add(time.Minute)
	duration := checkoutDurationMs
	build, branch := stageBuild, stageBranch
	return []RunStagePayload{
		{StageID: stageCheckout, Name: "Checkout", NodeType: "stage", Status: "SUCCESS",
			StartedAt: &started, EndedAt: &ended, DurationMs: &duration},
		{StageID: stageBuild, Name: "Build", NodeType: "stage", Status: status, StartedAt: &ended},
		{StageID: stageBranch, ParentID: &build, Name: "Branch: unit", NodeType: "parallel", Status: status, StartedAt: &ended},
		{StageID: stageTest, ParentID: &branch, Name: "Test", NodeType: "stage", Status: status, StartedAt: &ended},
	}
}

// applyStage applies one run.stage envelope from sender and answers its
// result.
func applyStage(t *testing.T, g *Ingester, runID uuid.UUID, intentKey, sender string, st RunStagePayload) ApplyResult {
	t.Helper()
	env := newTestEnvelope(t, runID, intentKey, KindRunStage, st)
	env.CellInstance = sender
	res, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("apply run.stage %s from %q: %v", st.StageID, sender, err)
	}
	return res
}

// openStageTree applies the stage tree from sender, with every open stage
// IN_PROGRESS.
func openStageTree(t *testing.T, g *Ingester, runID uuid.UUID, intentKey, sender string) {
	t.Helper()
	for _, st := range stageTreePayloads("IN_PROGRESS") {
		applyStage(t, g, runID, intentKey, sender, st)
	}
}

// storedStage is what one run_stages row holds of a stage end. reason is
// the stage payload's interrupted_reason key.
type storedStage struct {
	status   string
	endedAt  sql.NullTime
	duration sql.NullInt64
	reason   sql.NullString
}

// readStoredStage reads the storedStage of one stage of runID.
func readStoredStage(t *testing.T, db *sql.DB, runID uuid.UUID, stageID string) storedStage {
	t.Helper()
	var s storedStage
	if err := db.QueryRow(`
		SELECT status, ended_at, duration_ms, payload->>'interrupted_reason'
		FROM varroa.run_stages WHERE run_id = $1 AND stage_id = $2
	`, runID, stageID).Scan(&s.status, &s.endedAt, &s.duration, &s.reason); err != nil {
		t.Fatalf("read stage %s: %v", stageID, err)
	}
	return s
}

// assertStageEndedByMark fails the test unless the stage holds the end
// that a mark with reason at markAt writes.
func assertStageEndedByMark(t *testing.T, db *sql.DB, runID uuid.UUID, stageID, reason string, markAt time.Time) {
	t.Helper()
	s := readStoredStage(t, db, runID, stageID)
	if s.status != "ABORTED" || !s.endedAt.Valid || !s.endedAt.Time.Equal(markAt) {
		t.Errorf("stage %s: status=%q ended_at=%v, want ABORTED at %v", stageID, s.status, s.endedAt, markAt)
	}
	if s.duration.Valid {
		t.Errorf("stage %s: duration_ms=%d, want NULL", stageID, s.duration.Int64)
	}
	if s.reason.String != reason {
		t.Errorf("stage %s: interrupted_reason=%v, want %q", stageID, s.reason, reason)
	}
}

// assertStageOpen fails the test unless the stage is still IN_PROGRESS
// with no end and no interrupted_reason key.
func assertStageOpen(t *testing.T, db *sql.DB, runID uuid.UUID, stageID string) {
	t.Helper()
	s := readStoredStage(t, db, runID, stageID)
	if s.status != "IN_PROGRESS" || s.endedAt.Valid || s.reason.Valid {
		t.Errorf("stage %s = %+v, want IN_PROGRESS with no end and no interrupted_reason", stageID, s)
	}
}

// assertStageFromOwner fails the test unless the stage holds the owner's
// own SUCCESS end: a duration and no interrupted_reason key.
func assertStageFromOwner(t *testing.T, db *sql.DB, runID uuid.UUID, stageID string) {
	t.Helper()
	s := readStoredStage(t, db, runID, stageID)
	if s.status != "SUCCESS" || !s.duration.Valid || s.duration.Int64 != checkoutDurationMs || s.reason.Valid {
		t.Errorf("stage %s = %+v, want the owner's SUCCESS with its duration and no interrupted_reason", stageID, s)
	}
}

// ownerStageEnd answers the owner's terminal run.stage payload for one
// stage of the tree.
func ownerStageEnd(t *testing.T, stageID string) RunStagePayload {
	t.Helper()
	for _, st := range stageTreePayloads("SUCCESS") {
		if st.StageID == stageID {
			ended := st.StartedAt.Add(time.Minute)
			duration := checkoutDurationMs
			st.EndedAt = &ended
			st.DurationMs = &duration
			return st
		}
	}
	t.Fatalf("stage %s is not in the stage tree", stageID)
	return RunStagePayload{}
}

// markAt answers a mark time that Postgres stores with no loss.
func markAt(offset time.Duration) time.Time {
	return time.Now().UTC().Add(offset).Truncate(time.Microsecond)
}

// markRunWithOpenStages seeds one RUNNING run of pod-a with the open stage
// tree and marks it with reason at the answered time.
func markRunWithOpenStages(t *testing.T, pool *pgxpool.Pool, reason string) (*Ingester, uuid.UUID, string, time.Time) {
	t.Helper()
	intentKey := "manual:" + newTestUUIDv7String()
	runID := startRunForFinalize(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	openStageTree(t, g, runID, intentKey, "pod-a")
	at := markAt(0)
	if changed, err := markInterrupted(context.Background(), pool, fetchRunningCandidate(t, pool, runID), reason, at, ""); err != nil || !changed {
		t.Fatalf("mark %s: changed=%v err=%v, want a mark", reason, changed, err)
	}
	return g, runID, intentKey, at
}

// TestMarkInterruptedEndsTheOpenStages asserts that every mark that ends a
// RUNNING run ends each open stage of the run, at every depth: the root
// stage, the parallel branch in it, and the stage in that branch. Each
// open stage reads ABORTED at the mark time with no duration and the
// mark's reason. The finished stage keeps its own end. The run read that
// the build page uses answers the same stages.
func TestMarkInterruptedEndsTheOpenStages(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	for _, reason := range []string{ReasonCellLost, ReasonCellRolled, ReasonTenantMoved, ReasonNoHeartbeat,
		ReasonResumeTimeout, ReasonResumeFailed} {
		t.Run(reason, func(t *testing.T) {
			_, runID, _, at := markRunWithOpenStages(t, pool, reason)

			for _, id := range openStageIDs {
				assertStageEndedByMark(t, db, runID, id, reason, at)
			}
			assertStageFromOwner(t, db, runID, stageCheckout)

			_, stages, err := reader.GetRun(context.Background(), runID.String())
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if len(stages) != 4 {
				t.Fatalf("stages = %d, want 4", len(stages))
			}
			for _, st := range stages {
				if st.Status == "IN_PROGRESS" {
					t.Errorf("GetRun stage %s is IN_PROGRESS, want every stage ended", st.StageID)
				}
			}
		})
	}
}

// TestMarkInterruptedKeepsTheOpenStagesOfAProvisionalMark asserts the
// marks that keep the open stages. The owner of a heartbeat-lost or a
// start-lost run can still run them. A QUEUED run keeps them too, because
// a later run.started supersedes the mark of an accepted row.
func TestMarkInterruptedKeepsTheOpenStagesOfAProvisionalMark(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	for _, reason := range []string{ReasonHeartbeatLost, ReasonStartLost} {
		t.Run(reason, func(t *testing.T) {
			_, runID, _, _ := markRunWithOpenStages(t, pool, reason)
			for _, id := range openStageIDs {
				assertStageOpen(t, db, runID, id)
			}
		})
	}

	t.Run("a queued run", func(t *testing.T) {
		intentKey := "manual:" + newTestUUIDv7String()
		runID := seedGrantedIntent(t, pool, intentKey)
		openStageTree(t, NewIngester(pool, nil, nil), runID, intentKey, "pod-a")
		if _, err := db.Exec(`
			INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status)
			SELECT run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, 'QUEUED'
			FROM varroa.run_intents WHERE run_id = $1
		`, runID); err != nil {
			t.Fatalf("seed the queued row: %v", err)
		}
		c := fetchRunningCandidate(t, pool, runID)
		if c.status != StatusQueued {
			t.Fatalf("candidate status = %q, want QUEUED", c.status)
		}
		if changed, err := markInterrupted(context.Background(), pool, c, ReasonCellLost, markAt(0), ""); err != nil || !changed {
			t.Fatalf("mark: changed=%v err=%v, want a mark", changed, err)
		}
		for _, id := range openStageIDs {
			assertStageOpen(t, db, runID, id)
		}
	})
}

// TestOwnerStageEndReplacesAStageTheMarkEnded asserts the first order of
// arrival after a mark: the owner's terminal run.stage fact replaces the
// stage that the mark ended. The real status, end, and duration win, and
// the interrupted_reason key goes. The other ended stages keep the mark's
// end.
func TestOwnerStageEndReplacesAStageTheMarkEnded(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g, runID, intentKey, at := markRunWithOpenStages(t, pool, ReasonNoHeartbeat)

	applyStage(t, g, runID, intentKey, "pod-a", ownerStageEnd(t, stageBuild))

	assertStageFromOwner(t, db, runID, stageBuild)
	assertStageEndedByMark(t, db, runID, stageTest, ReasonNoHeartbeat, at)
}

// TestRedeliveredStageStartKeepsAStageTheMarkEnded asserts the second
// order of arrival: a redelivered IN_PROGRESS fact of the owner never
// opens a stage that the mark ended. A resumed build sends no second
// start for an open stage, so that fact is stale.
func TestRedeliveredStageStartKeepsAStageTheMarkEnded(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g, runID, intentKey, at := markRunWithOpenStages(t, pool, ReasonCellRolled)

	for _, st := range stageTreePayloads("IN_PROGRESS") {
		applyStage(t, g, runID, intentKey, "pod-a", st)
	}

	for _, id := range openStageIDs {
		assertStageEndedByMark(t, db, runID, id, ReasonCellRolled, at)
	}
}

// TestReplacementPodCompletionReplacesTheStagesAMarkEnded asserts the
// takeover order. A controller with no cell role ran the stage tree on
// its replaced pod, and a sweep marked the run cell-lost. Jenkins on the
// replacement pod resumed the build. Its completion group, the terminal
// stage facts and then run.completed, replaces each ended stage and the
// mark. A late stage fact of the replaced pod changes nothing.
func TestReplacementPodCompletionReplacesTheStagesAMarkEnded(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, intentKey := seedReplacedControllerRun(t, pool, db, 0)
	g := NewIngester(pool, nil, nil)
	openStageTree(t, g, runID, intentKey, replacedPod)
	at := markAt(0)
	if changed, err := markInterrupted(context.Background(), pool, fetchRunningCandidate(t, pool, runID), ReasonCellLost, at, ""); err != nil || !changed {
		t.Fatalf("mark cell-lost: changed=%v err=%v, want a mark", changed, err)
	}
	for _, id := range openStageIDs {
		assertStageEndedByMark(t, db, runID, id, ReasonCellLost, at)
	}

	for _, id := range openStageIDs {
		applyStage(t, g, runID, intentKey, replacementPod, ownerStageEnd(t, id))
	}
	res := applyFromPod(t, g, runID, intentKey, KindRunCompleted, "cell-a", replacementPod,
		RunCompletedPayload{Result: strPtr("SUCCESS")})
	if !res.CompletedOwner {
		t.Fatal("CompletedOwner = false, want true")
	}

	late := ownerStageEnd(t, stageBuild)
	late.Status = "FAILURE"
	if stale := applyStage(t, g, runID, intentKey, replacedPod, late); !stale.ForeignSender {
		t.Fatal("ForeignSender = false for the replaced pod's late stage, want true")
	}

	for _, id := range openStageIDs {
		assertStageFromOwner(t, db, runID, id)
	}
	if s := readReplacementRunState(t, db, runID); s.result != "SUCCESS" || !s.supersededMark {
		t.Fatalf("run = %+v, want SUCCESS with the mark superseded", s)
	}
}

// TestResumedTargetReplacesAStageTheMarkEnded asserts the mesh order. The
// mesh moved a cell run to another cell under the same run id, and the
// resume passed its deadline, so the sweep marked the run resume-timeout.
// The target's late terminal stage fact replaces the ended stage. A late
// fact of the dead instance changes nothing.
func TestResumedTargetReplacesAStageTheMarkEnded(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, intentKey := seedMovedCellRun(t, pool, db)
	if _, err := db.Exec(`
		INSERT INTO varroa.run_stages (run_id, stage_id, created_at, name, node_type, status, started_at)
		SELECT run_id, $2, created_at, 'Build', 'stage', 'IN_PROGRESS', now()
		FROM varroa.run_intents WHERE run_id = $1
	`, runID, stageBuild); err != nil {
		t.Fatalf("seed the stage the dead instance opened: %v", err)
	}
	at := markAt(resumeDeadline + time.Minute)
	if changed, err := markInterrupted(context.Background(), pool, fetchRunningCandidate(t, pool, runID), ReasonResumeTimeout, at, ""); err != nil || !changed {
		t.Fatalf("mark resume-timeout: changed=%v err=%v, want a mark", changed, err)
	}
	assertStageEndedByMark(t, db, runID, stageBuild, ReasonResumeTimeout, at)

	g := NewIngester(pool, nil, nil)
	late := ownerStageEnd(t, stageBuild)
	late.Status = "FAILURE"
	if stale := applyStage(t, g, runID, intentKey, meshDeadInstance, late); !stale.ForeignSender {
		t.Fatal("ForeignSender = false for the dead instance's stage, want true")
	}
	assertStageEndedByMark(t, db, runID, stageBuild, ReasonResumeTimeout, at)

	applyStage(t, g, runID, intentKey, meshResumeTarget, ownerStageEnd(t, stageBuild))
	assertStageFromOwner(t, db, runID, stageBuild)
}
