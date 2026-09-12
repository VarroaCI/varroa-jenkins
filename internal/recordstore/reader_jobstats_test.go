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
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	jobPath := "acme/app-window"
	windowStart := time.Now().UTC().Add(-30 * 24 * time.Hour)

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
