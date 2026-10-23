package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedJobSummaryRun inserts one varroa.runs row directly, with the given
// job path, result, and duration, so a test can build a multi-job
// fixture without going through the ledger and the ingester.
func seedJobSummaryRun(t *testing.T, db *sql.DB, jobPath string, createdAt time.Time, result string, durationMs *int64) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status, result, duration_ms)
		VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, 'COMPLETED', $8, $9)
	`, runID, createdAt, "manual:jobsummary-"+runID.String(), testCluster, testNamespace, testTenant, jobPath, result, durationMs); err != nil {
		t.Fatalf("seed run for %s: %v", jobPath, err)
	}
	return runID
}

// TestListJobSummariesGroupsByJobPath asserts ListJobSummaries reports
// the right recent results, last run, and duration trend for each of
// two jobs, without conflating them.
func TestListJobSummariesGroupsByJobPath(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	// A recent moment, not a fixed calendar date: it must land inside a
	// partition migration 00001 already created, which spans only the
	// month before and the two months after "now" at migration time.
	base := time.Now().UTC().Add(-time.Hour)
	durA1, durA2 := int64(1000), int64(2000)
	seedJobSummaryRun(t, db, "acme/app-a", base, "SUCCESS", &durA1)
	newestA := seedJobSummaryRun(t, db, "acme/app-a", base.Add(time.Hour), "FAILURE", &durA2)
	seedJobSummaryRun(t, db, "acme/app-b", base, "SUCCESS", nil)

	summaries, err := reader.ListJobSummaries(ctx, testCluster, testTenant, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("ListJobSummaries: %v", err)
	}

	byPath := map[string]JobSummary{}
	for _, s := range summaries {
		byPath[s.JobPath] = s
	}

	appA, ok := byPath["acme/app-a"]
	if !ok {
		t.Fatalf("acme/app-a missing from %v", byPath)
	}
	if len(appA.RecentResults) != 2 || appA.RecentResults[0] != "FAILURE" || appA.RecentResults[1] != "SUCCESS" {
		t.Fatalf("acme/app-a RecentResults = %v, want [FAILURE SUCCESS] newest first", appA.RecentResults)
	}
	if appA.LastRun == nil || appA.LastRun.RunID != newestA.String() {
		t.Fatalf("acme/app-a LastRun = %v, want the newest run %s", appA.LastRun, newestA)
	}
	if len(appA.DurationTrendMs) != 2 || appA.DurationTrendMs[0] != durA2 || appA.DurationTrendMs[1] != durA1 {
		t.Fatalf("acme/app-a DurationTrendMs = %v, want [%d %d] newest first", appA.DurationTrendMs, durA2, durA1)
	}

	appB, ok := byPath["acme/app-b"]
	if !ok {
		t.Fatalf("acme/app-b missing from %v", byPath)
	}
	if len(appB.RecentResults) != 1 || appB.RecentResults[0] != "SUCCESS" {
		t.Fatalf("acme/app-b RecentResults = %v, want [SUCCESS]", appB.RecentResults)
	}
	if len(appB.DurationTrendMs) != 0 {
		t.Fatalf("acme/app-b DurationTrendMs = %v, want none (its only run has no duration)", appB.DurationTrendMs)
	}
}

// TestJobSummaryRecentResultsCapsAtFiveAndSkipsUnfinishedRuns asserts
// RecentResults holds only a job's newest jobHealthRecentResultsSize
// finished runs, newest first, and that a run with no result yet
// (RUNNING) is skipped rather than occupying one of the five slots.
func TestJobSummaryRecentResultsCapsAtFiveAndSkipsUnfinishedRuns(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	var wantNewestFirst []string
	for i := 0; i < jobHealthRecentResultsSize+2; i++ {
		result := "SUCCESS"
		if i < 3 {
			result = "FAILURE"
		}
		seedJobSummaryRun(t, db, "acme/app-d", base.Add(time.Duration(i)*time.Minute), result, nil)
		wantNewestFirst = append([]string{result}, wantNewestFirst...)
	}
	wantNewestFirst = wantNewestFirst[:jobHealthRecentResultsSize]
	// A newer run still running has no result yet: it must not occupy
	// one of the five slots or shift an older finished run out of them.
	runID := uuid.New()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status)
		VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, 'RUNNING')
	`, runID, base.Add(time.Duration(jobHealthRecentResultsSize+2)*time.Minute), "manual:jobsummary-"+runID.String(), testCluster, testNamespace, testTenant, "acme/app-d"); err != nil {
		t.Fatalf("seed running run: %v", err)
	}

	summary, err := reader.GetJobSummary(ctx, testCluster, testTenant, "acme/app-d")
	if err != nil {
		t.Fatalf("GetJobSummary: %v", err)
	}
	if len(summary.RecentResults) != jobHealthRecentResultsSize {
		t.Fatalf("RecentResults = %v, want %d entries", summary.RecentResults, jobHealthRecentResultsSize)
	}
	for i, got := range summary.RecentResults {
		if got != wantNewestFirst[i] {
			t.Fatalf("RecentResults = %v, want %v newest first", summary.RecentResults, wantNewestFirst)
		}
	}
}

// TestJobSummaryDurationTrendDoesNotReachPastTheNewestRuns asserts the
// duration trend only looks at a job's newest jobSummaryTrendSize runs:
// when every one of them lacks a duration, the trend stays empty
// instead of reaching further back for an older run that has one.
func TestJobSummaryDurationTrendDoesNotReachPastTheNewestRuns(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	oldDuration := int64(500)
	seedJobSummaryRun(t, db, "acme/app-c", base, "SUCCESS", &oldDuration)
	for i := 1; i <= jobSummaryTrendSize; i++ {
		seedJobSummaryRun(t, db, "acme/app-c", base.Add(time.Duration(i)*time.Minute), "SUCCESS", nil)
	}

	summary, err := reader.GetJobSummary(ctx, testCluster, testTenant, "acme/app-c")
	if err != nil {
		t.Fatalf("GetJobSummary: %v", err)
	}
	if len(summary.DurationTrendMs) != 0 {
		t.Fatalf("DurationTrendMs = %v, want empty: the newest %d runs all lack a duration", summary.DurationTrendMs, jobSummaryTrendSize)
	}
}

// TestGetJobSummaryUnknownJobHasNoRecentResults asserts a job with no
// run at all returns an empty RecentResults and a nil LastRun.
func TestGetJobSummaryUnknownJobHasNoRecentResults(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	summary, err := reader.GetJobSummary(ctx, testCluster, testTenant, "acme/never-run")
	if err != nil {
		t.Fatalf("GetJobSummary: %v", err)
	}
	if summary.LastRun != nil {
		t.Fatalf("LastRun = %v, want nil", summary.LastRun)
	}
	if len(summary.RecentResults) != 0 {
		t.Fatalf("RecentResults = %v, want none", summary.RecentResults)
	}
}

// TestListJobSummariesAddsQueueWaitMedians asserts the tenant home table's
// queue wait comes from the runs inside the window: the median of a
// job's waits, no value for a job that never started a build, and no
// contribution from a run created before the window.
func TestListJobSummariesAddsQueueWaitMedians(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	wait1, wait2, wait3 := int64(2000), int64(4000), int64(9000)
	seedJobQueueWaitRun(t, db, "acme/queue-wait", base, &wait1)
	seedJobQueueWaitRun(t, db, "acme/queue-wait", base.Add(time.Minute), &wait2)
	seedJobQueueWaitRun(t, db, "acme/queue-wait", base.Add(2*time.Minute), &wait3)
	seedJobQueueWaitRun(t, db, "acme/queue-pending", base, nil)

	// Two hours back bounds the window. The older run carries a wait far
	// above the others, so counting it would move the median.
	windowStart := base.Add(-time.Hour)
	oldWait := int64(600000)
	seedJobQueueWaitRun(t, db, "acme/queue-wait", base.Add(-3*time.Hour), &oldWait)
	seedJobQueueWaitRun(t, db, "acme/queue-old", base.Add(-3*time.Hour), &oldWait)

	summaries, err := reader.ListJobSummaries(ctx, testCluster, testTenant, windowStart)
	if err != nil {
		t.Fatalf("ListJobSummaries: %v", err)
	}
	byPath := map[string]JobSummary{}
	for _, s := range summaries {
		byPath[s.JobPath] = s
	}

	waited, ok := byPath["acme/queue-wait"]
	if !ok {
		t.Fatalf("acme/queue-wait missing from %v", byPath)
	}
	if waited.MedianQueueWaitMs == nil || *waited.MedianQueueWaitMs != 4000 {
		t.Fatalf("acme/queue-wait MedianQueueWaitMs = %v, want 4000 (median of 2000, 4000, 9000)", waited.MedianQueueWaitMs)
	}

	pending, ok := byPath["acme/queue-pending"]
	if !ok {
		t.Fatalf("acme/queue-pending missing from %v", byPath)
	}
	if pending.MedianQueueWaitMs != nil {
		t.Fatalf("acme/queue-pending MedianQueueWaitMs = %v, want nil: no run started", *pending.MedianQueueWaitMs)
	}

	old, ok := byPath["acme/queue-old"]
	if !ok {
		t.Fatalf("acme/queue-old missing from %v", byPath)
	}
	if old.MedianQueueWaitMs != nil {
		t.Fatalf("acme/queue-old MedianQueueWaitMs = %v, want nil: its only run predates the window", *old.MedianQueueWaitMs)
	}
}
