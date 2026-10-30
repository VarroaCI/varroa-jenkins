package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// qualityTestRun inserts a runs row for one run id, so the base join can
// read its build number. The row sits at createdAt, which the quality rows
// of the run copy in production.
func qualityTestRun(t *testing.T, db *sql.DB, runID uuid.UUID, createdAt time.Time, buildNumber int64) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, build_number, payload)
		VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, 'RUNNING', $8, '{}'::jsonb)
	`, runID, createdAt, "manual:quality-"+runID.String(),
		testCluster, testNamespace, testTenant, testJobPath, buildNumber); err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

// qualityTestCoverage inserts one run_coverage row with a line counter.
func qualityTestCoverage(t *testing.T, db *sql.DB, runID uuid.UUID, jobPath, reportID string, createdAt time.Time, covered int) {
	t.Helper()
	const total = 1000
	if _, err := db.Exec(`
		INSERT INTO varroa.run_coverage (run_id, report_id, cluster, tenant, job_path, name,
			line_covered, line_total, metrics, created_at)
		VALUES ($1, $2, $3, $4, $5, $2, $6, $7, '{}'::jsonb, $8)
	`, runID, reportID, testCluster, testTenant, jobPath, covered, total, createdAt); err != nil {
		t.Fatalf("seed run_coverage: %v", err)
	}
}

// qualityTestWarnings inserts one checkstyle run_warnings row with a total.
func qualityTestWarnings(t *testing.T, db *sql.DB, runID uuid.UUID, createdAt time.Time, total int) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.run_warnings (run_id, report_id, cluster, tenant, job_path, name, total, created_at)
		VALUES ($1, 'checkstyle', $2, $3, $4, 'checkstyle', $5, $6)
	`, runID, testCluster, testTenant, testJobPath, total, createdAt); err != nil {
		t.Fatalf("seed run_warnings: %v", err)
	}
}

// qualityTestTimes returns n instants one hour apart, oldest first, in
// the current month so every runs partition exists.
func qualityTestTimes(n int) []time.Time {
	start := time.Now().UTC().Add(-time.Duration(n+1) * time.Hour).Truncate(time.Second)
	out := make([]time.Time, n)
	for i := range out {
		out[i] = start.Add(time.Duration(i) * time.Hour)
	}
	return out
}

// TestRunQualityBaseIsPreviousRunOfSameReport checks that each row takes
// the newest earlier row of its own report, even when a run between them
// holds no row of that report.
func TestRunQualityBaseIsPreviousRunOfSameReport(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	ts := qualityTestTimes(3)
	r1, r2, r3 := uuid.New(), uuid.New(), uuid.New()
	qualityTestRun(t, db, r1, ts[0], 1)
	qualityTestRun(t, db, r2, ts[1], 2)
	qualityTestRun(t, db, r3, ts[2], 3)

	qualityTestCoverage(t, db, r1, testJobPath, "jacoco", ts[0], 800)
	qualityTestWarnings(t, db, r1, ts[0], 5)
	qualityTestWarnings(t, db, r2, ts[1], 3)
	qualityTestCoverage(t, db, r3, testJobPath, "jacoco", ts[2], 812)
	qualityTestWarnings(t, db, r3, ts[2], 1)

	q, err := r.RunQuality(context.Background(), r3.String())
	if err != nil {
		t.Fatalf("RunQuality: %v", err)
	}
	if len(q.Coverage) != 1 || q.Coverage[0].ReportID != "jacoco" {
		t.Fatalf("coverage = %+v, want one jacoco row", q.Coverage)
	}
	base := q.Coverage[0].Base
	if base == nil || base.RunID != r1.String() || base.BuildNumber == nil || *base.BuildNumber != 1 {
		t.Fatalf("coverage base = %+v, want run r1 build 1", base)
	}
	if base.Line == nil || base.Line.Covered != 800 || base.Line.Total != 1000 {
		t.Fatalf("coverage base line = %+v, want 800/1000", base.Line)
	}
	if len(q.Warnings) != 1 || q.Warnings[0].Base == nil {
		t.Fatalf("warnings = %+v, want one checkstyle row with a base", q.Warnings)
	}
	wb := q.Warnings[0].Base
	if wb.RunID != r2.String() || wb.Total != 3 || wb.BuildNumber == nil || *wb.BuildNumber != 2 {
		t.Fatalf("warnings base = %+v, want run r2 total 3 build 2", wb)
	}
}

// TestRunQualityFirstRunHasNoBase checks that the oldest run of a job
// carries no base.
func TestRunQualityFirstRunHasNoBase(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ts := qualityTestTimes(1)
	r1 := uuid.New()
	qualityTestRun(t, db, r1, ts[0], 1)
	qualityTestCoverage(t, db, r1, testJobPath, "jacoco", ts[0], 800)
	qualityTestWarnings(t, db, r1, ts[0], 5)

	q, err := NewReader(pool).RunQuality(context.Background(), r1.String())
	if err != nil {
		t.Fatalf("RunQuality: %v", err)
	}
	if len(q.Coverage) != 1 || q.Coverage[0].Base != nil {
		t.Fatalf("coverage = %+v, want one row with no base", q.Coverage)
	}
	if len(q.Warnings) != 1 || q.Warnings[0].Base != nil {
		t.Fatalf("warnings = %+v, want one row with no base", q.Warnings)
	}
}

// TestRunQualityIgnoresOtherJobAndReport checks that a newer-than-base row
// of another job path, or of another report, is never the base.
func TestRunQualityIgnoresOtherJobAndReport(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ts := qualityTestTimes(3)
	r1, r2, r3 := uuid.New(), uuid.New(), uuid.New()
	qualityTestRun(t, db, r1, ts[0], 1)
	qualityTestRun(t, db, r2, ts[1], 2)
	qualityTestRun(t, db, r3, ts[2], 3)
	qualityTestCoverage(t, db, r1, testJobPath, "jacoco", ts[0], 800)
	qualityTestCoverage(t, db, r2, "acme/other", "jacoco", ts[1], 100)
	qualityTestCoverage(t, db, r2, testJobPath, "branches", ts[1], 100)
	qualityTestCoverage(t, db, r3, testJobPath, "jacoco", ts[2], 812)

	q, err := NewReader(pool).RunQuality(context.Background(), r3.String())
	if err != nil {
		t.Fatalf("RunQuality: %v", err)
	}
	if len(q.Coverage) != 1 || q.Coverage[0].Base == nil || q.Coverage[0].Base.RunID != r1.String() {
		t.Fatalf("coverage = %+v, want base run r1", q.Coverage)
	}
}

// TestRunQualityNoRowsAnswersEmpty checks that a run with no quality row
// answers two empty lists and no error.
func TestRunQualityNoRowsAnswersEmpty(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	q, err := NewReader(pool).RunQuality(context.Background(), uuid.New().String())
	if err != nil {
		t.Fatalf("RunQuality: %v", err)
	}
	if len(q.Coverage) != 0 || len(q.Warnings) != 0 {
		t.Fatalf("RunQuality = %+v, want empty", q)
	}
	if q.CoverageDropped != 0 || q.WarningsDropped != 0 {
		t.Fatalf("dropped = %d/%d, want 0/0", q.CoverageDropped, q.WarningsDropped)
	}
}

// TestRunQualityAnswersDroppedCounts checks that RunQuality answers the
// stored dropped counts. The report count is the largest of the rows, and
// the metrics count is per row.
func TestRunQualityAnswersDroppedCounts(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	ts := qualityTestTimes(1)
	run := uuid.New()
	qualityTestRun(t, db, run, ts[0], 1)
	qualityTestCoverage(t, db, run, testJobPath, "jacoco", ts[0], 800)
	qualityTestCoverage(t, db, run, testJobPath, "cobertura", ts[0], 700)
	qualityTestWarnings(t, db, run, ts[0], 5)
	for _, stmt := range []string{
		`UPDATE varroa.run_coverage SET reports_dropped = 3 WHERE run_id = $1`,
		`UPDATE varroa.run_coverage SET metrics_dropped = 2 WHERE run_id = $1 AND report_id = 'jacoco'`,
		`UPDATE varroa.run_warnings SET reports_dropped = 1 WHERE run_id = $1`,
	} {
		if _, err := db.Exec(stmt, run); err != nil {
			t.Fatalf("seed dropped counts: %v", err)
		}
	}

	q, err := r.RunQuality(context.Background(), run.String())
	if err != nil {
		t.Fatalf("RunQuality: %v", err)
	}
	if q.CoverageDropped != 3 || q.WarningsDropped != 1 {
		t.Fatalf("dropped = %d/%d, want 3/1", q.CoverageDropped, q.WarningsDropped)
	}
	for _, c := range q.Coverage {
		want := int32(0)
		if c.ReportID == "jacoco" {
			want = 2
		}
		if c.MetricsDropped != want {
			t.Fatalf("%s metricsDropped = %d, want %d", c.ReportID, c.MetricsDropped, want)
		}
	}
}

// TestJobQualityTrendWindowWithGaps checks that the trend read answers the
// rows of the listed runs only, and that no row carries a base.
func TestJobQualityTrendWindowWithGaps(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ts := qualityTestTimes(4)
	r1, r2, r3, r4 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	qualityTestRun(t, db, r1, ts[0], 1)
	qualityTestRun(t, db, r2, ts[1], 2)
	qualityTestRun(t, db, r3, ts[2], 3)
	qualityTestRun(t, db, r4, ts[3], 4)
	qualityTestCoverage(t, db, r1, testJobPath, "jacoco", ts[0], 800)
	qualityTestCoverage(t, db, r3, testJobPath, "jacoco", ts[2], 812)
	qualityTestCoverage(t, db, r4, testJobPath, "jacoco", ts[3], 900)

	window := []string{r3.String(), r2.String(), r1.String()}
	q, err := NewReader(pool).JobQualityTrend(context.Background(), testCluster, testTenant, testJobPath, window)
	if err != nil {
		t.Fatalf("JobQualityTrend: %v", err)
	}
	if len(q.Coverage) != 2 {
		t.Fatalf("coverage rows = %d, want 2 (r3 and r1)", len(q.Coverage))
	}
	if q.Coverage[0].RunID != r3.String() || q.Coverage[1].RunID != r1.String() {
		t.Fatalf("coverage run order = %s, %s; want r3 then r1", q.Coverage[0].RunID, q.Coverage[1].RunID)
	}
	for _, row := range q.Coverage {
		if row.Base != nil {
			t.Fatalf("trend row %s carries a base", row.RunID)
		}
	}
}

// TestJobQualityTrendNoRows checks that an empty id list and a window with
// no rows both answer empty.
func TestJobQualityTrendNoRows(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	r := NewReader(pool)
	q, err := r.JobQualityTrend(context.Background(), testCluster, testTenant, testJobPath, nil)
	if err != nil || len(q.Coverage) != 0 || len(q.Warnings) != 0 {
		t.Fatalf("nil ids: q = %+v, err = %v; want empty", q, err)
	}
	q, err = r.JobQualityTrend(context.Background(), testCluster, testTenant, testJobPath, []string{uuid.New().String()})
	if err != nil || len(q.Coverage) != 0 || len(q.Warnings) != 0 {
		t.Fatalf("no-row ids: q = %+v, err = %v; want empty", q, err)
	}
}

// TestRunQualityBaseBreaksCreatedAtTies checks that two runs created in
// the same instant order by run id, so the later run takes the earlier
// one as its base.
func TestRunQualityBaseBreaksCreatedAtTies(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ts := qualityTestTimes(1)
	lo, hi := uuid.New(), uuid.New()
	if hi.String() < lo.String() {
		lo, hi = hi, lo
	}
	qualityTestRun(t, db, lo, ts[0], 1)
	qualityTestRun(t, db, hi, ts[0], 2)
	qualityTestCoverage(t, db, lo, testJobPath, "jacoco", ts[0], 800)
	qualityTestCoverage(t, db, hi, testJobPath, "jacoco", ts[0], 812)

	q, err := NewReader(pool).RunQuality(context.Background(), hi.String())
	if err != nil {
		t.Fatalf("RunQuality: %v", err)
	}
	if len(q.Coverage) != 1 || q.Coverage[0].Base == nil || q.Coverage[0].Base.RunID != lo.String() {
		t.Fatalf("coverage = %+v, want base run %s", q.Coverage, lo)
	}
}
