package recordstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedTriggerFailedRun inserts one TRIGGER_FAILED runs row directly, with no
// run_intents row, exactly as the ingest path writes one.
func seedTriggerFailedRun(t *testing.T, db *sql.DB, jobPath string, createdAt time.Time) string {
	t.Helper()
	runID := uuid.Must(uuid.NewV7()).String()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, result, ended_at, actor, payload
		) VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, 'COMPLETED', 'TRIGGER_FAILED', $2, 'user:alice',
			jsonb_build_object('trigger_error', 'no such job', 'trigger_reason_code', 'JOB_NOT_FOUND'))
	`, runID, createdAt, "manual:"+runID, testCluster, testNamespace, testTenant, jobPath); err != nil {
		t.Fatalf("seed TRIGGER_FAILED run: %v", err)
	}
	return runID
}

// TestGetRunReadsTriggerFailedThroughBoundedStep asserts GetRun answers a run
// that holds no intent row, through the bounded fallback.
func TestGetRunReadsTriggerFailedThroughBoundedStep(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)

	runID := seedTriggerFailedRun(t, db, testJobPath, time.Now().UTC().Add(-time.Hour))

	run, _, err := reader.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.RunID != runID {
		t.Fatalf("run_id = %q, want %q", run.RunID, runID)
	}
	if run.Result == nil || *run.Result != ResultTriggerFailed {
		t.Fatalf("result = %v, want %q", run.Result, ResultTriggerFailed)
	}
}

// TestGetRunReadsTriggerFailedOutsideTheWindow asserts the fallback does not
// depend on the window. The bound is a fast path, and it can never hide a row.
func TestGetRunReadsTriggerFailedOutsideTheWindow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)

	runID := seedTriggerFailedRun(t, db, testJobPath, time.Now().UTC().Add(-time.Hour))

	// Move the row past the window's end. The identifier's own time stays
	// where it was, so only the unbounded step can find the row.
	_, minted, err := parseRunUUIDv7(runID)
	if err != nil {
		t.Fatalf("parse run id: %v", err)
	}
	moved := minted.Add(runWindowAfter + 24*time.Hour)
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, result, ended_at
		) SELECT run_id, $2, intent_key, cluster, namespace, cell, tenant, job_path,
		         status, result, ended_at
		  FROM varroa.runs WHERE run_id = $1
	`, runID, moved); err != nil {
		t.Fatalf("copy run outside the window: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM varroa.runs WHERE run_id = $1 AND created_at <> $2`, runID, moved); err != nil {
		t.Fatalf("delete the in-window row: %v", err)
	}

	run, _, err := reader.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.RunID != runID {
		t.Fatalf("run_id = %q, want %q", run.RunID, runID)
	}
}

// TestGetRunUnknownIdentifierIsNotFound asserts an identifier no runs row holds
// answers ErrRunNotFound, after both steps.
func TestGetRunUnknownIdentifierIsNotFound(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)

	_, _, err := reader.GetRun(context.Background(), uuid.Must(uuid.NewV7()).String())
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("GetRun error = %v, want ErrRunNotFound", err)
	}
}

// TestTriggerFailedChangesNoJobStatistic asserts a job whose only run is a
// failed trigger reads as a job that has never run.
func TestTriggerFailedChangesNoJobStatistic(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	jobPath := "acme/refused"
	createdAt := time.Now().UTC().Add(-time.Hour)
	seedTriggerFailedRun(t, db, jobPath, createdAt)

	stats, err := reader.JobWindowStats(ctx, testCluster, testTenant, jobPath, createdAt.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("JobWindowStats: %v", err)
	}
	if stats.Builds != 0 {
		t.Fatalf("builds = %d, want 0", stats.Builds)
	}
	if stats.PassRate != nil {
		t.Fatalf("pass rate = %v, want none", *stats.PassRate)
	}
	if stats.MedianDurationMs != nil {
		t.Fatalf("median duration = %v, want none", *stats.MedianDurationMs)
	}

	summaries, err := reader.ListJobSummaries(ctx, testCluster, testTenant, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("ListJobSummaries: %v", err)
	}
	for _, s := range summaries {
		if s.JobPath != jobPath {
			continue
		}
		if s.LastRun != nil {
			t.Fatal("a failed trigger must be no last run")
		}
		if len(s.RecentResults) != 0 {
			t.Fatalf("recent results = %v, want none", s.RecentResults)
		}
		if len(s.DurationTrendMs) != 0 {
			t.Fatalf("duration trend = %v, want none", s.DurationTrendMs)
		}
	}
}

// TestTriggerFailedIsStillListed asserts the run stays listable, and still
// answers a result filter naming TRIGGER_FAILED.
func TestTriggerFailedIsStillListed(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	jobPath := "acme/refused-listed"
	runID := seedTriggerFailedRun(t, db, jobPath, time.Now().UTC().Add(-time.Hour))

	page, err := reader.ListRuns(ctx, RunQuery{Cluster: testCluster, Tenant: testTenant, JobPath: jobPath, Limit: 10})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(page.Runs) != 1 || page.Runs[0].RunID != runID {
		t.Fatalf("ListRuns returned %d runs, want the failed trigger", len(page.Runs))
	}

	filtered, err := reader.ListRuns(ctx, RunQuery{
		Cluster: testCluster, Tenant: testTenant, JobPath: jobPath,
		Result: ResultTriggerFailed, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListRuns with a result filter: %v", err)
	}
	if len(filtered.Runs) != 1 || filtered.Runs[0].RunID != runID {
		t.Fatalf("the result filter returned %d runs, want 1", len(filtered.Runs))
	}
}
