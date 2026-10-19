package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// This file covers the recorder's refusal of job inventory sets taken
// ahead of its clock, for both job.schedules and job.branches.

// jobScheduleCellWatermark reads the cell's schedule watermark.
func jobScheduleCellWatermark(t *testing.T, db *sql.DB, cell string) time.Time {
	t.Helper()
	var at time.Time
	if err := db.QueryRow(`
		SELECT observed_at FROM varroa.job_schedule_cells
		WHERE cluster = $1 AND namespace = $2 AND cell = $3
	`, jsTestCluster, jsTestNS, cell).Scan(&at); err != nil {
		t.Fatalf("read job schedule cell watermark: %v", err)
	}
	return at
}

// jobBranchCellWatermark reads the cell's branch watermark.
func jobBranchCellWatermark(t *testing.T, db *sql.DB, cell string) time.Time {
	t.Helper()
	var at time.Time
	if err := db.QueryRow(`
		SELECT branches_observed_at FROM varroa.job_schedule_cells
		WHERE cluster = $1 AND namespace = $2 AND cell = $3
	`, jsTestCluster, jsTestNS, cell).Scan(&at); err != nil {
		t.Fatalf("read job branch cell watermark: %v", err)
	}
	return at
}

// skewTestSchedulePayload is a one-row job.schedules payload for tenant acme.
func skewTestSchedulePayload(hash, jobPath string) JobSchedulesPayload {
	return JobSchedulesPayload{
		Hash: hash,
		Rows: []JobScheduleRowInput{{Tenant: "acme", JobPath: jobPath, TriggerType: "cron", Spec: "H * * * *"}},
	}
}

// TestApplyJobSchedules_TakenTimeAheadRefused asserts that a set taken
// past the recorder clock's skew limit changes no row and keeps the
// watermark of the last set a correct clock took.
func TestApplyJobSchedules_TakenTimeAheadRefused(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	first := time.Now().UTC().Add(-time.Minute)
	if _, err := g.Apply(ctx, newJobScheduleEnvelope(t, jsTestCell, first, skewTestSchedulePayload("a", "acme/a"))); err != nil {
		t.Fatalf("apply first set: %v", err)
	}

	result, err := g.Apply(ctx, newJobScheduleEnvelope(t, jsTestCell, time.Now().UTC().Add(10*time.Minute), skewTestSchedulePayload("b", "acme/b")))
	if err != nil {
		t.Fatalf("apply ahead set: %v", err)
	}
	if !result.InventoryAhead || result.ScheduleStale {
		t.Fatalf("result = %+v, want InventoryAhead and not ScheduleStale", result)
	}
	if result.InventoryAheadBy <= 9*time.Minute {
		t.Errorf("InventoryAheadBy = %v, want more than 9m", result.InventoryAheadBy)
	}
	if got := jobScheduleCellWatermark(t, db, jsTestCell); !got.Equal(first.Truncate(time.Microsecond)) {
		t.Errorf("watermark = %v, want the first set's %v", got, first)
	}
	var jobPath string
	if err := db.QueryRow(`SELECT job_path FROM varroa.job_schedules`).Scan(&jobPath); err != nil {
		t.Fatalf("read job_path: %v", err)
	}
	if n := countJobScheduleRows(t, db); n != 1 || jobPath != "acme/a" {
		t.Errorf("rows = %d (%q), want only acme/a", n, jobPath)
	}
}

// TestApplyJobSchedules_TakenTimeAheadFirstSetWritesNoWatermark asserts
// that a refused first set creates no watermark row for the cell.
func TestApplyJobSchedules_TakenTimeAheadFirstSetWritesNoWatermark(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	result, err := g.Apply(ctx, newJobScheduleEnvelope(t, jsTestCell, time.Now().UTC().Add(10*time.Minute), skewTestSchedulePayload("ahead", "acme/a")))
	if err != nil {
		t.Fatalf("apply ahead first set: %v", err)
	}
	if !result.InventoryAhead {
		t.Fatalf("result = %+v, want InventoryAhead", result)
	}
	var cells int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.job_schedule_cells WHERE cluster = $1`, jsTestCluster).Scan(&cells); err != nil {
		t.Fatalf("count watermark rows: %v", err)
	}
	if cells != 0 {
		t.Errorf("watermark rows = %d, want 0", cells)
	}
}

// TestApplyJobSchedules_TakenTimeWithinSkewAccepted asserts that a set
// taken a minute ahead of the recorder clock is applied.
func TestApplyJobSchedules_TakenTimeWithinSkewAccepted(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	at := time.Now().UTC().Add(time.Minute)
	result, err := g.Apply(ctx, newJobScheduleEnvelope(t, jsTestCell, at, skewTestSchedulePayload("near", "acme/a")))
	if err != nil {
		t.Fatalf("apply near-future set: %v", err)
	}
	if result.InventoryAhead {
		t.Fatalf("result = %+v, want the set accepted", result)
	}
	if got := jobScheduleCellWatermark(t, db, jsTestCell); !got.Equal(at.Truncate(time.Microsecond)) {
		t.Errorf("watermark = %v, want %v", got, at)
	}
	if n := countJobScheduleRows(t, db); n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

// TestApplyJobSchedules_FutureWatermarkReplaced asserts that a stored
// watermark past the skew limit, left by a clock that was ahead, is
// replaced by a set from a correct clock, and that the rows it stamped
// are removed.
func TestApplyJobSchedules_FutureWatermarkReplaced(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	if _, err := g.Apply(ctx, newJobScheduleEnvelope(t, jsTestCell, time.Now().UTC().Add(-time.Minute), skewTestSchedulePayload("a", "acme/a"))); err != nil {
		t.Fatalf("apply first set: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE varroa.job_schedule_cells SET observed_at = now() + interval '1 hour'
		WHERE cluster = $1 AND namespace = $2 AND cell = $3
	`, jsTestCluster, jsTestNS, jsTestCell); err != nil {
		t.Fatalf("stamp future watermark: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE varroa.job_schedules SET observed_at = now() + interval '1 hour'
		WHERE cluster = $1 AND cell = $2
	`, jsTestCluster, jsTestCell); err != nil {
		t.Fatalf("stamp future row: %v", err)
	}

	now := time.Now().UTC()
	result, err := g.Apply(ctx, newJobScheduleEnvelope(t, jsTestCell, now, skewTestSchedulePayload("b", "acme/b")))
	if err != nil {
		t.Fatalf("apply correct-clock set: %v", err)
	}
	if result.ScheduleStale || result.InventoryAhead {
		t.Fatalf("result = %+v, want the set accepted", result)
	}
	if got := jobScheduleCellWatermark(t, db, jsTestCell); !got.Equal(now.Truncate(time.Microsecond)) {
		t.Errorf("watermark = %v, want %v", got, now)
	}
	var jobPath string
	if err := db.QueryRow(`SELECT job_path FROM varroa.job_schedules`).Scan(&jobPath); err != nil {
		t.Fatalf("read job_path: %v", err)
	}
	if n := countJobScheduleRows(t, db); n != 1 || jobPath != "acme/b" {
		t.Errorf("rows = %d (%q), want only acme/b", n, jobPath)
	}
}

// TestApplyJobSchedules_FutureRowRemovedByTruncatedSet asserts that rows
// stamped past the skew limit are removed even when the replacing set is
// truncated, so a poisoned row does not outlive its watermark.
func TestApplyJobSchedules_FutureRowRemovedByTruncatedSet(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	if _, err := g.Apply(ctx, newJobScheduleEnvelope(t, jsTestCell, time.Now().UTC().Add(-time.Minute), skewTestSchedulePayload("a", "acme/a"))); err != nil {
		t.Fatalf("apply first set: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE varroa.job_schedules SET observed_at = now() + interval '1 hour'
		WHERE cluster = $1 AND cell = $2
	`, jsTestCluster, jsTestCell); err != nil {
		t.Fatalf("stamp future row: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE varroa.job_schedule_cells SET observed_at = now() + interval '1 hour'
		WHERE cluster = $1 AND namespace = $2 AND cell = $3
	`, jsTestCluster, jsTestNS, jsTestCell); err != nil {
		t.Fatalf("stamp future watermark: %v", err)
	}

	truncated := skewTestSchedulePayload("b", "acme/b")
	truncated.Truncated = true
	now := time.Now().UTC()
	result, err := g.Apply(ctx, newJobScheduleEnvelope(t, jsTestCell, now, truncated))
	if err != nil {
		t.Fatalf("apply truncated correct-clock set: %v", err)
	}
	if result.InventoryAhead || !result.Truncated {
		t.Fatalf("result = %+v, want an accepted truncated set", result)
	}
	var jobPath string
	if err := db.QueryRow(`SELECT job_path FROM varroa.job_schedules`).Scan(&jobPath); err != nil {
		t.Fatalf("read job_path: %v", err)
	}
	if n := countJobScheduleRows(t, db); n != 1 || jobPath != "acme/b" {
		t.Errorf("rows = %d (%q), want only acme/b", n, jobPath)
	}
}

// TestApplyJobBranches_TakenTimeAheadRefused asserts that a branch set
// taken past the recorder clock's skew limit changes no row.
func TestApplyJobBranches_TakenTimeAheadRefused(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	first := time.Now().UTC().Add(-time.Minute)
	if _, err := g.Apply(ctx, newJobBranchesEnvelope(t, jsTestCell, first, JobBranchesPayload{
		Hash: "a", Projects: []JobBranchProjectInput{oneBranchProject("acme/app")},
	})); err != nil {
		t.Fatalf("apply first set: %v", err)
	}

	result, err := g.Apply(ctx, newJobBranchesEnvelope(t, jsTestCell, time.Now().UTC().Add(10*time.Minute), JobBranchesPayload{
		Hash: "b", Projects: []JobBranchProjectInput{oneBranchProject("acme/other")},
	}))
	if err != nil {
		t.Fatalf("apply ahead set: %v", err)
	}
	if !result.InventoryAhead || result.BranchesStale {
		t.Fatalf("result = %+v, want InventoryAhead and not BranchesStale", result)
	}
	if result.InventoryAheadBy <= 9*time.Minute {
		t.Errorf("InventoryAheadBy = %v, want more than 9m", result.InventoryAheadBy)
	}
	if got := jobBranchCellWatermark(t, db, jsTestCell); !got.Equal(first.Truncate(time.Microsecond)) {
		t.Errorf("branch watermark = %v, want the first set's %v", got, first)
	}
	var jobPath string
	if err := db.QueryRow(`SELECT job_path FROM varroa.job_branch_projects`).Scan(&jobPath); err != nil {
		t.Fatalf("read job_path: %v", err)
	}
	if n := countJobBranchRows(t, db); n != 1 || jobPath != "acme/app" {
		t.Errorf("rows = %d (%q), want only acme/app", n, jobPath)
	}
}

// TestApplyJobBranches_TakenTimeWithinSkewAccepted asserts that a branch
// set taken a minute ahead of the recorder clock is applied.
func TestApplyJobBranches_TakenTimeWithinSkewAccepted(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	at := time.Now().UTC().Add(time.Minute)
	result, err := g.Apply(ctx, newJobBranchesEnvelope(t, jsTestCell, at, JobBranchesPayload{
		Hash: "near", Projects: []JobBranchProjectInput{oneBranchProject("acme/app")},
	}))
	if err != nil {
		t.Fatalf("apply near-future set: %v", err)
	}
	if result.InventoryAhead {
		t.Fatalf("result = %+v, want the set accepted", result)
	}
	if got := jobBranchCellWatermark(t, db, jsTestCell); !got.Equal(at.Truncate(time.Microsecond)) {
		t.Errorf("branch watermark = %v, want %v", got, at)
	}
	if n := countJobBranchRows(t, db); n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

// TestApplyJobBranches_FutureWatermarkReplaced asserts that a stored branch
// watermark past the skew limit is replaced by a correct-clock set, and
// that the rows it stamped are removed.
func TestApplyJobBranches_FutureWatermarkReplaced(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	if _, err := g.Apply(ctx, newJobBranchesEnvelope(t, jsTestCell, time.Now().UTC().Add(-time.Minute), JobBranchesPayload{
		Hash: "a", Projects: []JobBranchProjectInput{oneBranchProject("acme/app")},
	})); err != nil {
		t.Fatalf("apply first set: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE varroa.job_schedule_cells SET branches_observed_at = now() + interval '1 hour'
		WHERE cluster = $1 AND namespace = $2 AND cell = $3
	`, jsTestCluster, jsTestNS, jsTestCell); err != nil {
		t.Fatalf("stamp future branch watermark: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE varroa.job_branch_projects SET observed_at = now() + interval '1 hour'
		WHERE cluster = $1
	`, jsTestCluster); err != nil {
		t.Fatalf("stamp future project row: %v", err)
	}

	now := time.Now().UTC()
	result, err := g.Apply(ctx, newJobBranchesEnvelope(t, jsTestCell, now, JobBranchesPayload{
		Hash: "b", Projects: []JobBranchProjectInput{oneBranchProject("acme/other")},
	}))
	if err != nil {
		t.Fatalf("apply correct-clock set: %v", err)
	}
	if result.BranchesStale || result.InventoryAhead {
		t.Fatalf("result = %+v, want the set accepted", result)
	}
	if got := jobBranchCellWatermark(t, db, jsTestCell); !got.Equal(now.Truncate(time.Microsecond)) {
		t.Errorf("branch watermark = %v, want %v", got, now)
	}
	var jobPath string
	if err := db.QueryRow(`SELECT job_path FROM varroa.job_branch_projects`).Scan(&jobPath); err != nil {
		t.Fatalf("read job_path: %v", err)
	}
	if n := countJobBranchRows(t, db); n != 1 || jobPath != "acme/other" {
		t.Errorf("rows = %d (%q), want only acme/other", n, jobPath)
	}
}
