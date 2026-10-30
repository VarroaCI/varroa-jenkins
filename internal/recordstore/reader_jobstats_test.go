package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedJobStatsStage inserts one varroa.run_stages row directly, so a
// test can build a stage fixture without going through the ingester.
func seedJobStatsStage(t *testing.T, db *sql.DB, runID uuid.UUID, createdAt time.Time, name string, durationMs int64) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.run_stages (run_id, stage_id, created_at, name, node_type, status, duration_ms)
		VALUES ($1, $2, $3, $4, 'stage', 'SUCCESS', $5)
	`, runID, uuid.New().String(), createdAt, name, durationMs); err != nil {
		t.Fatalf("seed stage %s for run %s: %v", name, runID, err)
	}
}

// TestJobWindowStatsEmptyWindow asserts a job with no run in the window
// reports zero builds and no rate, median, or slowest stage.
func TestJobWindowStatsEmptyWindow(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	since := time.Now().UTC().Add(-30 * 24 * time.Hour)
	stats, err := reader.JobWindowStats(ctx, testCluster, testTenant, "acme/no-runs", since)
	if err != nil {
		t.Fatalf("JobWindowStats: %v", err)
	}
	if stats.Builds != 0 {
		t.Errorf("Builds = %d, want 0", stats.Builds)
	}
	if stats.PassRate != nil {
		t.Errorf("PassRate = %v, want nil", *stats.PassRate)
	}
	if stats.MedianDurationMs != nil {
		t.Errorf("MedianDurationMs = %v, want nil", *stats.MedianDurationMs)
	}
	if stats.SlowestStage != nil {
		t.Errorf("SlowestStage = %+v, want nil", *stats.SlowestStage)
	}
}

// TestJobWindowStatsComputesRateMedianAndSlowestStage seeds three
// finished runs (two SUCCESS, one FAILURE) and two stage names across
// them, and asserts the pass rate, the median run duration, and the
// stage whose median duration is greatest.
func TestJobWindowStatsComputesRateMedianAndSlowestStage(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	jobPath := "acme/app-stats"
	dur1, dur2, dur3 := int64(1000), int64(2000), int64(3000)
	run1 := seedJobSummaryRun(t, db, jobPath, base, "SUCCESS", &dur1)
	run2 := seedJobSummaryRun(t, db, jobPath, base.Add(time.Minute), "SUCCESS", &dur2)
	run3 := seedJobSummaryRun(t, db, jobPath, base.Add(2*time.Minute), "FAILURE", &dur3)

	// "Build" stage runs fast and consistently; "Deploy" runs slower and
	// only on two of the three runs, so its median must still beat
	// "Build"'s median rather than being diluted by a missing stage.
	seedJobStatsStage(t, db, run1, base, "Build", 500)
	seedJobStatsStage(t, db, run1, base, "Deploy", 9000)
	seedJobStatsStage(t, db, run2, base.Add(time.Minute), "Build", 700)
	seedJobStatsStage(t, db, run2, base.Add(time.Minute), "Deploy", 1000)
	seedJobStatsStage(t, db, run3, base.Add(2*time.Minute), "Build", 600)

	stats, err := reader.JobWindowStats(ctx, testCluster, testTenant, jobPath, base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("JobWindowStats: %v", err)
	}
	if stats.Builds != 3 {
		t.Fatalf("Builds = %d, want 3", stats.Builds)
	}
	if stats.PassRate == nil {
		t.Fatalf("PassRate = nil, want 2/3")
	}
	if want := 2.0 / 3.0; *stats.PassRate < want-1e-9 || *stats.PassRate > want+1e-9 {
		t.Errorf("PassRate = %v, want %v", *stats.PassRate, want)
	}
	if stats.MedianDurationMs == nil || *stats.MedianDurationMs != 2000 {
		t.Fatalf("MedianDurationMs = %v, want 2000", stats.MedianDurationMs)
	}
	if stats.SlowestStage == nil {
		t.Fatalf("SlowestStage = nil, want Deploy")
	}
	if stats.SlowestStage.Name != "Deploy" || stats.SlowestStage.DurationMs != 5000 {
		t.Fatalf("SlowestStage = %+v, want Deploy at 5000ms (median of 1000 and 9000)", stats.SlowestStage)
	}
}

// TestJobWindowStatsIgnoresRunBeforeTheWindow asserts a run created
// before since never reaches Builds, PassRate, or MedianDurationMs, even
// though it belongs to the same job.
func TestJobWindowStatsIgnoresRunBeforeTheWindow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	jobPath := "acme/app-window"
	// Use a short window, not the 30-day card window. Migration 00001
	// creates partitions from the first day of the previous month. A seed
	// 30 days back can fall before that day early in a month. This occurs
	// when the previous month has 30 days or fewer.
	windowStart := time.Now().UTC().Add(-2 * time.Hour)

	oldDuration := int64(999999)
	seedJobSummaryRun(t, db, jobPath, windowStart.Add(-time.Hour), "FAILURE", &oldDuration)

	inWindowDuration := int64(500)
	seedJobSummaryRun(t, db, jobPath, windowStart.Add(time.Hour), "SUCCESS", &inWindowDuration)

	stats, err := reader.JobWindowStats(ctx, testCluster, testTenant, jobPath, windowStart)
	if err != nil {
		t.Fatalf("JobWindowStats: %v", err)
	}
	if stats.Builds != 1 {
		t.Fatalf("Builds = %d, want 1 (the run before the window must not count)", stats.Builds)
	}
	if stats.PassRate == nil || *stats.PassRate != 1.0 {
		t.Fatalf("PassRate = %v, want 1.0", stats.PassRate)
	}
	if stats.MedianDurationMs == nil || *stats.MedianDurationMs != inWindowDuration {
		t.Fatalf("MedianDurationMs = %v, want %d", stats.MedianDurationMs, inWindowDuration)
	}
}

// TestJobWindowStatsPassRateSkipsInterruptedAndNotBuilt asserts an
// INTERRUPTED or NOT_BUILT run counts in Builds but not in the pass
// rate. Two SUCCESS runs are the only counted runs, so the rate is 1.0.
func TestJobWindowStatsPassRateSkipsInterruptedAndNotBuilt(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	jobPath := "acme/app-uncounted"
	dur := int64(1000)
	seedJobSummaryRun(t, db, jobPath, base, "SUCCESS", &dur)
	seedJobSummaryRun(t, db, jobPath, base.Add(time.Minute), "SUCCESS", &dur)
	seedJobSummaryRun(t, db, jobPath, base.Add(2*time.Minute), "INTERRUPTED", &dur)
	seedJobSummaryRun(t, db, jobPath, base.Add(3*time.Minute), "NOT_BUILT", &dur)

	stats, err := reader.JobWindowStats(ctx, testCluster, testTenant, jobPath, base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("JobWindowStats: %v", err)
	}
	if stats.Builds != 4 {
		t.Fatalf("Builds = %d, want 4", stats.Builds)
	}
	if stats.PassRate == nil || *stats.PassRate != 1.0 {
		t.Fatalf("PassRate = %v, want 1.0", stats.PassRate)
	}
}

// seedJobQueueWaitRun inserts one run directly, with queued_at set to
// createdAt. A non-nil waitMs inserts a completed build: status
// COMPLETED, result SUCCESS, duration 1000 ms, and started_at waitMs
// milliseconds after createdAt. A nil waitMs inserts the pending shape
// Jenkins reports for a run that never started: status QUEUED, with no
// result, no duration, and no started_at.
func seedJobQueueWaitRun(t *testing.T, db *sql.DB, jobPath string, createdAt time.Time, waitMs *int64) {
	t.Helper()
	runID := uuid.New()
	status := "QUEUED"
	var result *string
	var durationMs *int64
	var startedAt *time.Time
	if waitMs != nil {
		status = "COMPLETED"
		success := "SUCCESS"
		result = &success
		duration := int64(1000)
		durationMs = &duration
		started := createdAt.Add(time.Duration(*waitMs) * time.Millisecond)
		startedAt = &started
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, result, duration_ms, queued_at, started_at
		) VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, $8, $9, $10, $2, $11)
	`, runID, createdAt, "manual:queue-wait-"+runID.String(), testCluster, testNamespace, testTenant, jobPath, status, result, durationMs, startedAt); err != nil {
		t.Fatalf("seed queue-wait run for %s: %v", jobPath, err)
	}
}

// TestJobWindowStatsReportsMedianQueueWait asserts the card reports the
// median wait between the queued and the started moment of the window's
// runs, beside the build count and the duration median it already
// reports.
func TestJobWindowStatsReportsMedianQueueWait(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	jobPath := "acme/app-queue-wait"
	wait1, wait2, wait3 := int64(1000), int64(8000), int64(3000)
	seedJobQueueWaitRun(t, db, jobPath, base, &wait1)
	seedJobQueueWaitRun(t, db, jobPath, base.Add(time.Minute), &wait2)
	seedJobQueueWaitRun(t, db, jobPath, base.Add(2*time.Minute), &wait3)

	stats, err := reader.JobWindowStats(ctx, testCluster, testTenant, jobPath, base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("JobWindowStats: %v", err)
	}
	if stats.Builds != 3 {
		t.Fatalf("Builds = %d, want 3", stats.Builds)
	}
	if stats.PassRate == nil || *stats.PassRate != 1.0 {
		t.Fatalf("PassRate = %v, want 1.0", stats.PassRate)
	}
	if stats.MedianDurationMs == nil || *stats.MedianDurationMs != 1000 {
		t.Fatalf("MedianDurationMs = %v, want 1000", stats.MedianDurationMs)
	}
	if stats.MedianQueueWaitMs == nil || *stats.MedianQueueWaitMs != 3000 {
		t.Fatalf("MedianQueueWaitMs = %v, want 3000 (median of 1000, 8000, 3000)", stats.MedianQueueWaitMs)
	}
}

// TestJobWindowStatsQueueWaitAbsentWithoutStartedRun asserts a job whose
// only window run never started reports no queue wait and no duration
// median, while its build count still reports.
func TestJobWindowStatsQueueWaitAbsentWithoutStartedRun(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	jobPath := "acme/app-never-started"
	seedJobQueueWaitRun(t, db, jobPath, base, nil)

	stats, err := reader.JobWindowStats(ctx, testCluster, testTenant, jobPath, base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("JobWindowStats: %v", err)
	}
	if stats.Builds != 1 {
		t.Fatalf("Builds = %d, want 1", stats.Builds)
	}
	if stats.PassRate != nil {
		t.Fatalf("PassRate = %v, want nil: the run has no result yet", *stats.PassRate)
	}
	if stats.MedianDurationMs != nil {
		t.Fatalf("MedianDurationMs = %v, want nil: the run has no duration yet", *stats.MedianDurationMs)
	}
	if stats.MedianQueueWaitMs != nil {
		t.Fatalf("MedianQueueWaitMs = %v, want nil: the run never started", *stats.MedianQueueWaitMs)
	}
}
