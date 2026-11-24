package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// stageDeploy is a stage that first starts after the completion. The stage
// tree of interrupted_stages_test.go does not hold it.
const stageDeploy = "7"

// completionStage is what one run_stages row holds of a stage end that
// a completion wrote. reason is the payload's interrupted_reason key, and
// atCompletion is the payload's open_at_completion key.
type completionStage struct {
	status       string
	endedAt      sql.NullTime
	duration     sql.NullInt64
	reason       sql.NullString
	atCompletion bool
}

// readCompletionStage reads the completionStage of one stage of runID.
func readCompletionStage(t *testing.T, db *sql.DB, runID uuid.UUID, stageID string) completionStage {
	t.Helper()
	var s completionStage
	if err := db.QueryRow(`
		SELECT status, ended_at, duration_ms, payload->>'interrupted_reason',
		       COALESCE((payload->>'open_at_completion')::bool, false)
		FROM varroa.run_stages WHERE run_id = $1 AND stage_id = $2
	`, runID, stageID).Scan(&s.status, &s.endedAt, &s.duration, &s.reason, &s.atCompletion); err != nil {
		t.Fatalf("read stage %s: %v", stageID, err)
	}
	return s
}

// assertStageEndedByCompletion fails the test unless the stage holds the
// end that an owner completion at endedAt writes: status, the completion's
// end, no duration, the open_at_completion key, and no interrupted_reason
// key.
func assertStageEndedByCompletion(t *testing.T, db *sql.DB, runID uuid.UUID, stageID, status string, endedAt time.Time) {
	t.Helper()
	s := readCompletionStage(t, db, runID, stageID)
	if s.status != status || !s.endedAt.Valid || !s.endedAt.Time.Equal(endedAt) {
		t.Errorf("stage %s: status=%q ended_at=%v, want %s at %v", stageID, s.status, s.endedAt, status, endedAt)
	}
	if s.duration.Valid {
		t.Errorf("stage %s: duration_ms=%d, want NULL", stageID, s.duration.Int64)
	}
	if !s.atCompletion || s.reason.Valid {
		t.Errorf("stage %s: open_at_completion=%v interrupted_reason=%v, want the completion key alone",
			stageID, s.atCompletion, s.reason)
	}
}

// completeFrom applies one run.completed of runID from sender, with result
// at endedAt, and answers its result. A nil result sends no result.
func completeFrom(t *testing.T, g *Ingester, runID uuid.UUID, intentKey, sender string, result *string, endedAt time.Time) ApplyResult {
	t.Helper()
	return applyFromPod(t, g, runID, intentKey, KindRunCompleted, "cell-a", sender,
		RunCompletedPayload{Result: result, EndedAt: &endedAt, Seq: 2})
}

// TestOwnerCompletionEndsTheOpenStages asserts that the owner's
// run.completed ends each stage that the build left open, at every depth.
// Each open stage takes SUCCESS on a SUCCESS run, and ABORTED for every
// other result and for no result. It takes the completion's end and no
// duration. The finished stage keeps its own end, and the run
// read that the build page uses answers no open stage.
func TestOwnerCompletionEndsTheOpenStages(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	for _, tc := range []struct {
		name   string
		result *string
		want   string
	}{
		{"SUCCESS", strPtr("SUCCESS"), "SUCCESS"},
		{"UNSTABLE", strPtr("UNSTABLE"), "ABORTED"},
		{"FAILURE", strPtr("FAILURE"), "ABORTED"},
		{"ABORTED", strPtr("ABORTED"), "ABORTED"},
		{"NOT_BUILT", strPtr("NOT_BUILT"), "ABORTED"},
		{"no result", nil, "ABORTED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			intentKey := "manual:" + newTestUUIDv7String()
			runID := startRunForFinalize(t, pool, intentKey)
			g := NewIngester(pool, nil, nil)
			openStageTree(t, g, runID, intentKey, "pod-a")

			endedAt := markAt(0)
			if res := completeFrom(t, g, runID, intentKey, "pod-a", tc.result, endedAt); !res.CompletedOwner {
				t.Fatal("CompletedOwner = false, want true")
			}

			for _, id := range openStageIDs {
				assertStageEndedByCompletion(t, db, runID, id, tc.want, endedAt)
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

// TestCompletionStageFactBeforeTheCompletionKeepsItsEnd asserts the usual
// order of the completion group: the terminal stage fact arrives before
// run.completed. That stage keeps the owner's own end, and the completion
// ends only the stages that stay open.
func TestCompletionStageFactBeforeTheCompletionKeepsItsEnd(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := startRunForFinalize(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	openStageTree(t, g, runID, intentKey, "pod-a")

	applyStage(t, g, runID, intentKey, "pod-a", ownerStageEnd(t, stageBuild))
	endedAt := markAt(0)
	completeFrom(t, g, runID, intentKey, "pod-a", strPtr("FAILURE"), endedAt)

	assertStageFromOwner(t, db, runID, stageBuild)
	if s := readCompletionStage(t, db, runID, stageBuild); s.atCompletion {
		t.Errorf("stage %s holds open_at_completion, want the owner's own payload", stageBuild)
	}
	assertStageEndedByCompletion(t, db, runID, stageBranch, "ABORTED", endedAt)
	assertStageEndedByCompletion(t, db, runID, stageTest, "ABORTED", endedAt)
}

// TestLateCompletionStageFactReplacesTheCompletionEnd asserts the other
// order: a terminal stage fact of the completion group arrives after
// run.completed. The real end replaces the end that the completion wrote,
// and the open_at_completion key goes. The other stages keep the
// completion's end.
func TestLateCompletionStageFactReplacesTheCompletionEnd(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := startRunForFinalize(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	openStageTree(t, g, runID, intentKey, "pod-a")

	endedAt := markAt(0)
	completeFrom(t, g, runID, intentKey, "pod-a", strPtr("FAILURE"), endedAt)
	applyStage(t, g, runID, intentKey, "pod-a", ownerStageEnd(t, stageBuild))

	assertStageFromOwner(t, db, runID, stageBuild)
	if s := readCompletionStage(t, db, runID, stageBuild); s.atCompletion {
		t.Errorf("stage %s keeps open_at_completion, want the late fact's payload", stageBuild)
	}
	assertStageEndedByCompletion(t, db, runID, stageBranch, "ABORTED", endedAt)
	assertStageEndedByCompletion(t, db, runID, stageTest, "ABORTED", endedAt)
}

// TestLateStageStartAfterTheCompletion asserts the IN_PROGRESS facts that
// arrive after run.completed. A redelivered start never opens a stage that
// the completion ended. A first start of a stage with no row writes the
// row already ended, because no later fact of the owner ends it.
func TestLateStageStartAfterTheCompletion(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := startRunForFinalize(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	openStageTree(t, g, runID, intentKey, "pod-a")

	endedAt := markAt(0)
	completeFrom(t, g, runID, intentKey, "pod-a", strPtr("SUCCESS"), endedAt)

	openStageTree(t, g, runID, intentKey, "pod-a")
	for _, id := range openStageIDs {
		assertStageEndedByCompletion(t, db, runID, id, "SUCCESS", endedAt)
	}
	assertStageFromOwner(t, db, runID, stageCheckout)

	started := endedAt.Add(-time.Minute)
	applyStage(t, g, runID, intentKey, "pod-a", RunStagePayload{
		StageID: stageDeploy, Name: "Deploy", NodeType: "stage", Status: "IN_PROGRESS", StartedAt: &started,
	})
	assertStageEndedByCompletion(t, db, runID, stageDeploy, "SUCCESS", endedAt)
}

// TestForeignCompletionKeepsTheOpenStages asserts that a completion from an
// instance that does not own the run ends no stage, because the owner can
// still run them. A first stage start after it stays open too. The owner's
// own completion then ends every open stage.
func TestForeignCompletionKeepsTheOpenStages(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := startRunForFinalize(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	openStageTree(t, g, runID, intentKey, "pod-a")

	if res := completeFrom(t, g, runID, intentKey, "pod-b", strPtr("ABORTED"), markAt(0)); res.CompletedOwner {
		t.Fatal("CompletedOwner = true for pod-b, want a foreign completion")
	}
	for _, id := range openStageIDs {
		assertStageOpen(t, db, runID, id)
	}
	started := markAt(0)
	applyStage(t, g, runID, intentKey, "pod-a", RunStagePayload{
		StageID: stageDeploy, Name: "Deploy", NodeType: "stage", Status: "IN_PROGRESS", StartedAt: &started,
	})
	assertStageOpen(t, db, runID, stageDeploy)

	endedAt := markAt(time.Minute)
	completeFrom(t, g, runID, intentKey, "pod-a", strPtr("SUCCESS"), endedAt)
	for _, id := range append(append([]string{}, openStageIDs...), stageDeploy) {
		assertStageEndedByCompletion(t, db, runID, id, "SUCCESS", endedAt)
	}
}

// TestDuplicateRunCompletionEndsTheOpenStages asserts that the owner
// completion of a DUPLICATE run ends its open stages too. The completion
// writes no outcome effect for such a run, but the stage rows are the
// run's own record.
func TestDuplicateRunCompletionEndsTheOpenStages(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	p := seedDuplicatePair(t, pool, g, "dup-stages")
	openStageTree(t, g, p.duplicate, p.duplicateKey, "pod-a")

	endedAt := markAt(0)
	res := completeFrom(t, g, p.duplicate, p.duplicateKey, "pod-a", strPtr("ABORTED"), endedAt)
	if !res.CompletedOwner || !res.RunDuplicate {
		t.Fatalf("CompletedOwner=%v RunDuplicate=%v, want the owner write of a DUPLICATE run", res.CompletedOwner, res.RunDuplicate)
	}
	for _, id := range openStageIDs {
		assertStageEndedByCompletion(t, db, p.duplicate, id, "ABORTED", endedAt)
	}
}

// TestOwnerCompletionReplacesTheStagesAMarkEnded asserts the completion of
// a run that a final mark ended. The owner's completion carries no fact
// for the stages that stay open, so the stages that the mark ended take
// the real result and the completion's end. The interrupted_reason key
// goes, because the real result supersedes the mark.
func TestOwnerCompletionReplacesTheStagesAMarkEnded(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g, runID, intentKey, at := markRunWithOpenStages(t, pool, ReasonCellRolled)
	for _, id := range openStageIDs {
		assertStageEndedByMark(t, db, runID, id, ReasonCellRolled, at)
	}

	endedAt := markAt(time.Minute)
	if res := completeFrom(t, g, runID, intentKey, "pod-a", strPtr("SUCCESS"), endedAt); !res.CompletedOwner {
		t.Fatal("CompletedOwner = false, want true")
	}
	for _, id := range openStageIDs {
		assertStageEndedByCompletion(t, db, runID, id, "SUCCESS", endedAt)
	}
	assertStageFromOwner(t, db, runID, stageCheckout)
}

// TestOwnerCompletionEndsTheStagesAHeartbeatLostMarkKept asserts the
// completion of a heartbeat-lost run. The provisional mark keeps the open
// stages, and the owner's later FAILURE completion ends them ABORTED.
func TestOwnerCompletionEndsTheStagesAHeartbeatLostMarkKept(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g, runID, intentKey, _ := markRunWithOpenStages(t, pool, ReasonHeartbeatLost)
	for _, id := range openStageIDs {
		assertStageOpen(t, db, runID, id)
	}

	endedAt := markAt(time.Minute)
	completeFrom(t, g, runID, intentKey, "pod-a", strPtr("FAILURE"), endedAt)
	for _, id := range openStageIDs {
		assertStageEndedByCompletion(t, db, runID, id, "ABORTED", endedAt)
	}
}

// TestRepeatedOwnerCompletionFollowsTheLatestResult asserts that a stage
// the completion ended follows the run row. A second owner completion
// writes its own result and end on the run, and on each such stage. A
// stage that holds a real end keeps it.
func TestRepeatedOwnerCompletionFollowsTheLatestResult(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := startRunForFinalize(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	openStageTree(t, g, runID, intentKey, "pod-a")

	completeFrom(t, g, runID, intentKey, "pod-a", strPtr("SUCCESS"), markAt(0))
	endedAt := markAt(time.Minute)
	completeFrom(t, g, runID, intentKey, "pod-a", strPtr("ABORTED"), endedAt)

	for _, id := range openStageIDs {
		assertStageEndedByCompletion(t, db, runID, id, "ABORTED", endedAt)
	}
	assertStageFromOwner(t, db, runID, stageCheckout)
}

// TestSweepMarkThenOwnerCompletionEndsTheOpenStages runs the stage ends
// through SweepInterruptedRuns, not through a direct mark. The sweep marks
// a silent run on a cell that is not alive no-heartbeat and ends its open
// stages. The owner's later completion then writes its own result and end
// on each of those stages.
func TestSweepMarkThenOwnerCompletionEndsTheOpenStages(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedRunningRun(t, pool, newTestClaimRequest(intentKey, 0))
	g := NewIngester(pool, nil, nil)
	openStageTree(t, g, runID, intentKey, "pod-a")

	past := time.Now().Add(-17 * time.Minute)
	if _, err := db.Exec(`UPDATE varroa.runs SET heartbeat_received_at = $1 WHERE run_id = $2`, past, runID); err != nil {
		t.Fatalf("backdate heartbeat_received_at: %v", err)
	}
	seedTenantPlacementLiveness(t, db, "pod-a", false, &past, &past, nil)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)

	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, nil, nil, nil, nil, nil,
		DefaultSweepMaxPending, time.Now(), testGrace, "", nil)
	if err != nil {
		t.Fatalf("SweepInterruptedRuns: %v", err)
	}
	if marked != 1 {
		t.Fatalf("marked = %d, want 1", marked)
	}
	for _, id := range openStageIDs {
		s := readCompletionStage(t, db, runID, id)
		if s.status != "ABORTED" || !s.endedAt.Valid || s.duration.Valid || s.reason.String != ReasonNoHeartbeat {
			t.Errorf("stage %s = %+v, want ABORTED by the %s mark with no duration", id, s, ReasonNoHeartbeat)
		}
	}

	endedAt := markAt(time.Minute)
	if res := completeFrom(t, g, runID, intentKey, "pod-a", strPtr("SUCCESS"), endedAt); !res.CompletedOwner {
		t.Fatal("CompletedOwner = false, want true")
	}
	for _, id := range openStageIDs {
		assertStageEndedByCompletion(t, db, runID, id, "SUCCESS", endedAt)
	}
	assertStageFromOwner(t, db, runID, stageCheckout)
}

// TestOwnerFailureCompletionLeavesTheRealFailedStageAlone asserts that a
// stage the recorder ends never reads as the stage that failed. Two
// parallel branches run: SlowBranch starts and sends no end, and RealFail
// sends a real FAILURE. The owner completion reports FAILURE. SlowBranch
// ends ABORTED, and RealFail keeps its own end. The run read and the
// failing-jobs read name RealFail as the only FAILURE stage, so the run
// digest names it too.
func TestOwnerFailureCompletionLeavesTheRealFailedStageAlone(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := startRunForFinalize(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)

	const slowBranch, realFail = "10", "11"
	started := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Millisecond)
	failedAt := started.Add(time.Minute)
	duration := checkoutDurationMs
	failure := "script returned exit code 1"
	applyStage(t, g, runID, intentKey, "pod-a", RunStagePayload{
		StageID: slowBranch, Name: "SlowBranch", NodeType: "parallel", Status: "IN_PROGRESS", StartedAt: &started,
	})
	applyStage(t, g, runID, intentKey, "pod-a", RunStagePayload{
		StageID: realFail, Name: "RealFail", NodeType: "parallel", Status: "FAILURE",
		StartedAt: &started, EndedAt: &failedAt, DurationMs: &duration, Error: &failure,
	})

	endedAt := markAt(0)
	if res := completeFrom(t, g, runID, intentKey, "pod-a", strPtr("FAILURE"), endedAt); !res.CompletedOwner {
		t.Fatal("CompletedOwner = false, want true")
	}

	assertStageEndedByCompletion(t, db, runID, slowBranch, "ABORTED", endedAt)
	realStage := readCompletionStage(t, db, runID, realFail)
	if realStage.status != "FAILURE" || !realStage.duration.Valid || realStage.duration.Int64 != checkoutDurationMs ||
		realStage.atCompletion {
		t.Errorf("stage %s = %+v, want the real FAILURE with its duration and no open_at_completion", realFail, realStage)
	}

	r := NewReader(pool)
	_, stages, err := r.GetRun(context.Background(), runID.String())
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	var failed []string
	for _, st := range stages {
		if st.Status == "FAILURE" {
			failed = append(failed, st.Name)
		}
	}
	if len(failed) != 1 || failed[0] != "RealFail" {
		t.Errorf("GetRun FAILURE stages = %v, want [RealFail]", failed)
	}

	page, err := r.FailingJobs(context.Background(), FailingJobsQuery{
		Scopes: testRunScopes, Since: time.Now().Add(-time.Hour), Limit: 10,
	})
	if err != nil {
		t.Fatalf("FailingJobs: %v", err)
	}
	if len(page.Jobs) != 1 {
		t.Fatalf("failing jobs = %d, want 1", len(page.Jobs))
	}
	listed := make([]string, 0, len(page.Jobs[0].FailedStages))
	for _, st := range page.Jobs[0].FailedStages {
		listed = append(listed, st.Name)
	}
	if len(listed) != 1 || listed[0] != "RealFail" {
		t.Errorf("failing-jobs stages = %v, want [RealFail]", listed)
	}
}
