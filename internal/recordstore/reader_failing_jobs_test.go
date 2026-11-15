package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedFailedStage inserts one FAILURE stage with an error under runID.
func seedFailedStage(t *testing.T, db *sql.DB, runID uuid.UUID, name, errText string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.run_stages (run_id, stage_id, created_at, name, node_type, status, error)
		VALUES ($1, $2, $3, $4, 'stage', 'FAILURE', $5)
	`, runID, "stage-"+name, time.Now().UTC(), name, errText); err != nil {
		t.Fatalf("seed failed stage %s: %v", name, err)
	}
}

// failingScope names the one scope that the failing-jobs tests read.
var failingScope = []RunScope{{Cluster: testCluster, Tenant: "pay"}}

// TestFailingJobsNewestCountedRunFailed asserts a job whose newest counted run
// failed is listed with that run, its failed stage, and its last pass.
func TestFailingJobsNewestCountedRunFailed(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	success, failure := "SUCCESS", "FAILURE"
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-3*time.Hour), "COMPLETED", &success)
	failed := seedAcrossRun(t, db, "pay", "pay/build", now.Add(-time.Hour), "COMPLETED", &failure)
	seedFailedStage(t, db, failed, "Test", "boom")

	page, err := r.FailingJobs(context.Background(), FailingJobsQuery{Scopes: failingScope, Since: now.Add(-FailingJobsWindow)})
	if err != nil {
		t.Fatalf("FailingJobs: %v", err)
	}
	if len(page.Jobs) != 1 || page.More {
		t.Fatalf("jobs = %d, more = %v; want one job and no more", len(page.Jobs), page.More)
	}
	job := page.Jobs[0]
	if job.Failed.RunID != failed.String() {
		t.Fatalf("failed run = %s, want %s", job.Failed.RunID, failed)
	}
	if len(job.FailedStages) != 1 || job.FailedStages[0].Name != "Test" {
		t.Fatalf("failed stages = %+v, want one stage named Test", job.FailedStages)
	}
	if job.LastPassed == nil || job.LastPassed.RunID == failed.String() {
		t.Fatalf("last passed = %+v, want the earlier success", job.LastPassed)
	}
	if job.FailuresSincePass != 1 {
		t.Fatalf("failures since pass = %d, want 1", job.FailuresSincePass)
	}
}

// TestFailingJobsPassedNewestIsNotFailing asserts a job whose newest counted run
// passed is not listed, even with an older failure.
func TestFailingJobsPassedNewestIsNotFailing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	success, failure := "SUCCESS", "FAILURE"
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-3*time.Hour), "COMPLETED", &failure)
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-time.Hour), "COMPLETED", &success)

	page, err := r.FailingJobs(context.Background(), FailingJobsQuery{Scopes: failingScope, Since: now.Add(-FailingJobsWindow)})
	if err != nil {
		t.Fatalf("FailingJobs: %v", err)
	}
	if len(page.Jobs) != 0 {
		t.Fatalf("jobs = %d, want none", len(page.Jobs))
	}
}

// TestFailingJobsSkipsInterruptedNewest asserts an INTERRUPTED newest run does
// not clear a failure: the newest counted run still decides.
func TestFailingJobsSkipsInterruptedNewest(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	failure, interrupted := "FAILURE", "INTERRUPTED"
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-2*time.Hour), "COMPLETED", &failure)
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-time.Hour), "COMPLETED", &interrupted)

	page, err := r.FailingJobs(context.Background(), FailingJobsQuery{Scopes: failingScope, Since: now.Add(-FailingJobsWindow)})
	if err != nil {
		t.Fatalf("FailingJobs: %v", err)
	}
	if len(page.Jobs) != 1 {
		t.Fatalf("jobs = %d, want the failing job", len(page.Jobs))
	}
}

// TestFailingJobsLastPassedAndFailureCount asserts the failure count after the
// last pass: two failures follow one pass, and an older failure does not count.
func TestFailingJobsLastPassedAndFailureCount(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	success, failure := "SUCCESS", "FAILURE"
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-5*time.Hour), "COMPLETED", &failure)
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-4*time.Hour), "COMPLETED", &success)
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-3*time.Hour), "COMPLETED", &failure)
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-time.Hour), "COMPLETED", &failure)

	page, err := r.FailingJobs(context.Background(), FailingJobsQuery{Scopes: failingScope, Since: now.Add(-FailingJobsWindow)})
	if err != nil {
		t.Fatalf("FailingJobs: %v", err)
	}
	if len(page.Jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(page.Jobs))
	}
	if got := page.Jobs[0].FailuresSincePass; got != 2 {
		t.Fatalf("failures since pass = %d, want 2", got)
	}
}

// TestFailingJobsNoPassOnRecord asserts a failing job with no earlier pass
// carries no last-passed run and counts every failure.
func TestFailingJobsNoPassOnRecord(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	failure := "FAILURE"
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-2*time.Hour), "COMPLETED", &failure)
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-time.Hour), "COMPLETED", &failure)

	page, err := r.FailingJobs(context.Background(), FailingJobsQuery{Scopes: failingScope, Since: now.Add(-FailingJobsWindow)})
	if err != nil {
		t.Fatalf("FailingJobs: %v", err)
	}
	if len(page.Jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(page.Jobs))
	}
	job := page.Jobs[0]
	if job.LastPassed != nil || job.FailuresSincePass != 2 {
		t.Fatalf("last passed = %+v, failures = %d; want none and 2", job.LastPassed, job.FailuresSincePass)
	}
}

// TestFailingJobsWindowBound asserts a failure older than since is not listed.
func TestFailingJobsWindowBound(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	failure := "FAILURE"
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-3*time.Hour), "COMPLETED", &failure)

	page, err := r.FailingJobs(context.Background(), FailingJobsQuery{Scopes: failingScope, Since: now.Add(-2 * time.Hour)})
	if err != nil {
		t.Fatalf("FailingJobs: %v", err)
	}
	if len(page.Jobs) != 0 {
		t.Fatalf("jobs = %d, want none before since", len(page.Jobs))
	}
}

// TestFailingJobsJobPatternsApply asserts a scope's job patterns narrow the read.
func TestFailingJobsJobPatternsApply(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	failure := "FAILURE"
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-time.Hour), "COMPLETED", &failure)
	seedAcrossRun(t, db, "pay", "pay/deploy", now.Add(-time.Hour), "COMPLETED", &failure)

	scopes := []RunScope{{Cluster: testCluster, Tenant: "pay", JobPatterns: []string{`pay/build`}}}
	page, err := r.FailingJobs(context.Background(), FailingJobsQuery{Scopes: scopes, Since: now.Add(-FailingJobsWindow)})
	if err != nil {
		t.Fatalf("FailingJobs: %v", err)
	}
	if len(page.Jobs) != 1 || page.Jobs[0].Failed.JobPath != "pay/build" {
		t.Fatalf("jobs = %+v, want only pay/build", page.Jobs)
	}
}

// TestFailingJobsLimitSetsMore asserts a page cut at the limit sets More.
func TestFailingJobsLimitSetsMore(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	failure := "FAILURE"
	seedAcrossRun(t, db, "pay", "pay/a", now.Add(-2*time.Hour), "COMPLETED", &failure)
	seedAcrossRun(t, db, "pay", "pay/b", now.Add(-time.Hour), "COMPLETED", &failure)

	page, err := r.FailingJobs(context.Background(), FailingJobsQuery{Scopes: failingScope, Since: now.Add(-FailingJobsWindow), Limit: 1})
	if err != nil {
		t.Fatalf("FailingJobs: %v", err)
	}
	if len(page.Jobs) != 1 || !page.More || page.Jobs[0].Failed.JobPath != "pay/b" {
		t.Fatalf("page = %+v, want the newest failure and More", page)
	}
}

// TestFailingJobsEmptyScopes asserts an empty scope list answers an empty page
// without a store call, so a closed pool does not matter.
func TestFailingJobsEmptyScopes(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	r := NewReader(pool)
	page, err := r.FailingJobs(context.Background(), FailingJobsQuery{})
	if err != nil || len(page.Jobs) != 0 || page.More {
		t.Fatalf("FailingJobs(no scope) = %+v, %v; want an empty page", page, err)
	}
}

// seedStageStarted inserts one FAILURE stage under runID with a fixed start time.
func seedStageStarted(t *testing.T, db *sql.DB, runID uuid.UUID, stageID, name string, startedAt time.Time) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.run_stages (run_id, stage_id, created_at, name, node_type, status, started_at)
		VALUES ($1, $2, $3, $4, 'stage', 'FAILURE', $5)
	`, runID, stageID, time.Now().UTC(), name, startedAt); err != nil {
		t.Fatalf("seed stage %s: %v", name, err)
	}
}

// TestFailingJobsSkipsNoBuildRows asserts a run that records no build does not
// become the newest counted run, so the real failure is the failing run.
func TestFailingJobsSkipsNoBuildRows(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	failure, triggerFailed := "FAILURE", "TRIGGER_FAILED"
	realFailure := seedAcrossRun(t, db, "pay", "pay/build", now.Add(-2*time.Hour), "COMPLETED", &failure)
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-time.Hour), "COMPLETED", &triggerFailed)

	page, err := r.FailingJobs(context.Background(), FailingJobsQuery{Scopes: failingScope, Since: now.Add(-FailingJobsWindow)})
	if err != nil {
		t.Fatalf("FailingJobs: %v", err)
	}
	if len(page.Jobs) != 1 || page.Jobs[0].Failed.RunID != realFailure.String() {
		t.Fatalf("jobs = %+v, want the real failure %s", page.Jobs, realFailure)
	}
}

// TestFailingJobsNoBuildSuccessIsNoPass asserts a SUCCESS row that records no
// build is not a last passed run, so it does not lower the failure count.
func TestFailingJobsNoBuildSuccessIsNoPass(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	success, failure := "SUCCESS", "FAILURE"
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-3*time.Hour), "COMPLETED", &success,
		withPayload(`{"accepted_at":"`+now.Add(-3*time.Hour).Format(time.RFC3339)+`"}`))
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-2*time.Hour), "COMPLETED", &failure)

	page, err := r.FailingJobs(context.Background(), FailingJobsQuery{Scopes: failingScope, Since: now.Add(-FailingJobsWindow)})
	if err != nil {
		t.Fatalf("FailingJobs: %v", err)
	}
	if len(page.Jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(page.Jobs))
	}
	job := page.Jobs[0]
	if job.LastPassed != nil || job.FailuresSincePass != 1 {
		t.Fatalf("last passed = %+v, failures = %d; want no pass and 1 failure", job.LastPassed, job.FailuresSincePass)
	}
}

// TestFailingJobsReadsFailedStagesInStageOrder asserts the failed stages come in
// the order of their start time, not their insert order.
func TestFailingJobsReadsFailedStagesInStageOrder(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	failure := "FAILURE"
	failed := seedAcrossRun(t, db, "pay", "pay/build", now.Add(-time.Hour), "COMPLETED", &failure)
	seedStageStarted(t, db, failed, "late", "Deploy", now.Add(-30*time.Minute))
	seedStageStarted(t, db, failed, "early", "Test", now.Add(-50*time.Minute))

	page, err := r.FailingJobs(context.Background(), FailingJobsQuery{Scopes: failingScope, Since: now.Add(-FailingJobsWindow)})
	if err != nil {
		t.Fatalf("FailingJobs: %v", err)
	}
	if len(page.Jobs) != 1 || len(page.Jobs[0].FailedStages) != 2 {
		t.Fatalf("jobs = %+v, want one job with two failed stages", page.Jobs)
	}
	stages := page.Jobs[0].FailedStages
	if stages[0].Name != "Test" || stages[1].Name != "Deploy" {
		t.Fatalf("stage order = %s, %s; want Test then Deploy", stages[0].Name, stages[1].Name)
	}
}
