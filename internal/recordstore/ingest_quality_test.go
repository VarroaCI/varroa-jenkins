package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// intSeed is a small helper for test payloads that take an int pointer.
func intSeed(v int) *int { return &v }

// qualitySeedPayload returns a run.quality payload with one coverage and
// one warnings report, used to seed both tables before a clear or an
// absent-key test.
func qualitySeedPayload() RunQualityPayload {
	return RunQualityPayload{
		Coverage: []RunCoveragePayload{{ID: "jacoco", Name: "Coverage"}},
		Warnings: []RunWarningsPayload{{ID: "checkstyle", Total: 3}},
	}
}

// TestApplyRunQualityWritesRows asserts that the rows copy their scope and
// created_at from the intent, and that counters, metrics, and gate status
// map onto their columns.
func TestApplyRunQualityWritesRows(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:quality-write-1")
	g := NewIngester(pool, nil, nil)

	env := newTestEnvelope(t, runID, "manual:quality-write-1", KindRunQuality, RunQualityPayload{
		Coverage: []RunCoveragePayload{{
			ID:         "jacoco",
			Name:       "Coverage",
			Line:       &RunCoverageCounterPayload{Covered: 812, Total: 1000},
			GateStatus: "PASSED",
			Metrics:    []RunCoverageCounterPayload{{Metric: "INSTRUCTION", Covered: 9, Total: 11}},
		}},
		Warnings: []RunWarningsPayload{},
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.quality: %v", err)
	}

	var cluster, tenant, jobPath string
	var createdMatch, metricsMatch bool
	var branchCovered, branchTotal *int
	var gate *string
	if err := db.QueryRow(`
		SELECT c.cluster, c.tenant, c.job_path,
		       c.created_at = i.created_at,
		       c.metrics = '{"INSTRUCTION":{"covered":9,"total":11}}'::jsonb,
		       c.branch_covered, c.branch_total, c.gate_status
		FROM varroa.run_coverage c
		JOIN varroa.run_intents i ON i.run_id = c.run_id
		WHERE c.run_id = $1 AND c.report_id = 'jacoco'`,
		runID,
	).Scan(&cluster, &tenant, &jobPath, &createdMatch, &metricsMatch, &branchCovered, &branchTotal, &gate); err != nil {
		t.Fatalf("read run_coverage: %v", err)
	}
	if cluster != testCluster || tenant != testTenant || jobPath != testJobPath {
		t.Fatalf("scope = (%s,%s,%s), want (%s,%s,%s)", cluster, tenant, jobPath, testCluster, testTenant, testJobPath)
	}
	if !createdMatch {
		t.Fatalf("created_at differs from run_intents.created_at")
	}
	if !metricsMatch {
		t.Fatalf("metrics jsonb differs from the expected object")
	}
	if branchCovered != nil || branchTotal != nil {
		t.Fatalf("absent branch stored %v/%v, want NULL/NULL", branchCovered, branchTotal)
	}
	if gate == nil || *gate != "PASSED" {
		t.Fatalf("gate_status = %v, want PASSED", gate)
	}
}

// TestApplyRunQualityRedeliveryKeepsOneSet delivers the same envelope
// twice and asserts one row per report.
func TestApplyRunQualityRedeliveryKeepsOneSet(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:quality-replay-1")
	g := NewIngester(pool, nil, nil)

	env := newTestEnvelope(t, runID, "manual:quality-replay-1", KindRunQuality, qualitySeedPayload())
	for i := 0; i < 2; i++ {
		if _, err := g.Apply(context.Background(), env); err != nil {
			t.Fatalf("apply run.quality delivery %d: %v", i, err)
		}
	}
	if n := countQualityRows(t, db, "run_coverage", runID); n != 1 {
		t.Fatalf("run_coverage rows = %d, want 1", n)
	}
	if n := countQualityRows(t, db, "run_warnings", runID); n != 1 {
		t.Fatalf("run_warnings rows = %d, want 1", n)
	}
}

// TestApplyRunQualityAbsentKeyKeepsRows seeds both tables, then applies a
// payload that carries only the coverage key. The warnings rows stay.
func TestApplyRunQualityAbsentKeyKeepsRows(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:quality-absent-1")
	g := NewIngester(pool, nil, nil)

	seed := newTestEnvelope(t, runID, "manual:quality-absent-1", KindRunQuality, qualitySeedPayload())
	if _, err := g.Apply(context.Background(), seed); err != nil {
		t.Fatalf("seed run.quality: %v", err)
	}

	raw := json.RawMessage(`{"coverage":[{"id":"cobertura"}]}`)
	env := newTestEnvelope(t, runID, "manual:quality-absent-1", KindRunQuality, raw)
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply coverage-only run.quality: %v", err)
	}
	if n := countQualityRows(t, db, "run_warnings", runID); n != 1 {
		t.Fatalf("run_warnings rows = %d, want 1 kept", n)
	}
	var id string
	if err := db.QueryRow(`SELECT report_id FROM varroa.run_coverage WHERE run_id = $1`, runID).Scan(&id); err != nil {
		t.Fatalf("read run_coverage: %v", err)
	}
	if id != "cobertura" {
		t.Fatalf("run_coverage report_id = %s, want cobertura", id)
	}
}

// TestApplyRunQualityEmptyListClearsRows seeds both tables, then applies
// empty lists. Both tables lose the run's rows.
func TestApplyRunQualityEmptyListClearsRows(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:quality-clear-1")
	g := NewIngester(pool, nil, nil)

	seed := newTestEnvelope(t, runID, "manual:quality-clear-1", KindRunQuality, qualitySeedPayload())
	if _, err := g.Apply(context.Background(), seed); err != nil {
		t.Fatalf("seed run.quality: %v", err)
	}

	env := newTestEnvelope(t, runID, "manual:quality-clear-1", KindRunQuality, RunQualityPayload{
		Coverage: []RunCoveragePayload{},
		Warnings: []RunWarningsPayload{},
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply empty run.quality: %v", err)
	}
	if n := countQualityRows(t, db, "run_coverage", runID); n != 0 {
		t.Fatalf("run_coverage rows = %d, want 0", n)
	}
	if n := countQualityRows(t, db, "run_warnings", runID); n != 0 {
		t.Fatalf("run_warnings rows = %d, want 0", n)
	}
}

// TestApplyRunQualityKeepsZeroValues asserts that a zero total and a zero
// counter pair are stored as zero, not as NULL.
func TestApplyRunQualityKeepsZeroValues(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:quality-zero-1")
	g := NewIngester(pool, nil, nil)

	env := newTestEnvelope(t, runID, "manual:quality-zero-1", KindRunQuality, RunQualityPayload{
		Coverage: []RunCoveragePayload{{
			ID:   "jacoco",
			Line: &RunCoverageCounterPayload{Covered: 0, Total: 0},
		}},
		Warnings: []RunWarningsPayload{{ID: "checkstyle", Total: 0}},
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.quality: %v", err)
	}

	var lineCovered, lineTotal, warnTotal int
	if err := db.QueryRow(`SELECT line_covered, line_total FROM varroa.run_coverage WHERE run_id = $1`,
		runID).Scan(&lineCovered, &lineTotal); err != nil {
		t.Fatalf("read line counters: %v", err)
	}
	if lineCovered != 0 || lineTotal != 0 {
		t.Fatalf("line = (%d,%d), want (0,0)", lineCovered, lineTotal)
	}
	if err := db.QueryRow(`SELECT total FROM varroa.run_warnings WHERE run_id = $1`, runID).Scan(&warnTotal); err != nil {
		t.Fatalf("read warnings total: %v", err)
	}
	if warnTotal != 0 {
		t.Fatalf("warnings total = %d, want 0", warnTotal)
	}
}

// TestApplyRunQualityNewFixedNullWithoutReference asserts that absent
// new and fixed keys store NULL, and a present zero stores 0.
func TestApplyRunQualityNewFixedNullWithoutReference(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:quality-newfixed-1")
	g := NewIngester(pool, nil, nil)

	env := newTestEnvelope(t, runID, "manual:quality-newfixed-1", KindRunQuality, RunQualityPayload{
		Warnings: []RunWarningsPayload{
			{ID: "absent", Total: 4},
			{ID: "zero", Total: 4, NewTotal: intSeed(0), FixedTotal: intSeed(0)},
		},
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.quality: %v", err)
	}

	var newTotal, fixedTotal *int
	if err := db.QueryRow(`SELECT new_total, fixed_total FROM varroa.run_warnings WHERE run_id = $1 AND report_id = 'absent'`,
		runID).Scan(&newTotal, &fixedTotal); err != nil {
		t.Fatalf("read absent row: %v", err)
	}
	if newTotal != nil || fixedTotal != nil {
		t.Fatalf("absent new/fixed = %v/%v, want NULL/NULL", newTotal, fixedTotal)
	}
	if err := db.QueryRow(`SELECT new_total, fixed_total FROM varroa.run_warnings WHERE run_id = $1 AND report_id = 'zero'`,
		runID).Scan(&newTotal, &fixedTotal); err != nil {
		t.Fatalf("read zero row: %v", err)
	}
	if newTotal == nil || fixedTotal == nil || *newTotal != 0 || *fixedTotal != 0 {
		t.Fatalf("zero new/fixed = %v/%v, want 0/0", newTotal, fixedTotal)
	}
}

// TestApplyRunQualityDedupesReportIDs asserts that a duplicate id keeps
// the first report, an empty id is skipped, and the transaction succeeds.
func TestApplyRunQualityDedupesReportIDs(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:quality-dedupe-1")
	g := NewIngester(pool, nil, nil)

	env := newTestEnvelope(t, runID, "manual:quality-dedupe-1", KindRunQuality, RunQualityPayload{
		Coverage: []RunCoveragePayload{
			{ID: "", Name: "no id"},
			{ID: "jacoco", Name: "first"},
			{ID: "jacoco", Name: "second"},
		},
		Warnings: []RunWarningsPayload{
			{ID: "checkstyle", Name: "first", Total: 1},
			{ID: "checkstyle", Name: "second", Total: 2},
		},
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.quality with duplicate ids: %v", err)
	}

	var name string
	if err := db.QueryRow(`SELECT name FROM varroa.run_coverage WHERE run_id = $1 AND report_id = 'jacoco'`,
		runID).Scan(&name); err != nil {
		t.Fatalf("read run_coverage: %v", err)
	}
	if name != "first" {
		t.Fatalf("coverage name = %s, want first", name)
	}
	if n := countQualityRows(t, db, "run_coverage", runID); n != 1 {
		t.Fatalf("run_coverage rows = %d, want 1", n)
	}
	var total int
	if err := db.QueryRow(`SELECT total FROM varroa.run_warnings WHERE run_id = $1`, runID).Scan(&total); err != nil {
		t.Fatalf("read run_warnings: %v", err)
	}
	if total != 1 {
		t.Fatalf("warnings total = %d, want 1 (first wins)", total)
	}
}

// TestApplyRunQualityStoresDroppedCounts asserts that the plugin's and the
// gateway's dropped counts land on the rows. The report count is copied to
// every coverage row and to the warnings row. The metrics count is per row.
func TestApplyRunQualityStoresDroppedCounts(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:quality-dropped-1")
	g := NewIngester(pool, nil, nil)

	env := newTestEnvelope(t, runID, "manual:quality-dropped-1", KindRunQuality, RunQualityPayload{
		Coverage: []RunCoveragePayload{
			{ID: "jacoco", Name: "Coverage", MetricsDropped: 4},
			{ID: "cobertura", Name: "Cobertura"},
		},
		Warnings:        []RunWarningsPayload{{ID: "checkstyle", Total: 3}},
		CoverageDropped: 5,
		WarningsDropped: 2,
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.quality with dropped counts: %v", err)
	}

	rows, err := db.Query(`SELECT report_id, reports_dropped, metrics_dropped FROM varroa.run_coverage
		WHERE run_id = $1 ORDER BY report_id`, runID)
	if err != nil {
		t.Fatalf("read run_coverage: %v", err)
	}
	defer rows.Close()
	got := map[string][2]int{}
	for rows.Next() {
		var id string
		var reports, metrics int
		if err := rows.Scan(&id, &reports, &metrics); err != nil {
			t.Fatalf("scan run_coverage: %v", err)
		}
		got[id] = [2]int{reports, metrics}
	}
	if got["jacoco"] != [2]int{5, 4} || got["cobertura"] != [2]int{5, 0} {
		t.Fatalf("run_coverage (reports, metrics) = %v, want jacoco 5/4 and cobertura 5/0", got)
	}

	var warnDropped int
	if err := db.QueryRow(`SELECT reports_dropped FROM varroa.run_warnings WHERE run_id = $1`,
		runID).Scan(&warnDropped); err != nil {
		t.Fatalf("read run_warnings: %v", err)
	}
	if warnDropped != 2 {
		t.Fatalf("run_warnings reports_dropped = %d, want 2", warnDropped)
	}
}

// TestApplyRunQualityCountsDedupedReports asserts that a report the dedupe
// removes counts as dropped, on top of the count the payload sends. A
// negative payload count stores as zero.
func TestApplyRunQualityCountsDedupedReports(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:quality-dedupe-count-1")
	g := NewIngester(pool, nil, nil)

	env := newTestEnvelope(t, runID, "manual:quality-dedupe-count-1", KindRunQuality, RunQualityPayload{
		Coverage: []RunCoveragePayload{
			{ID: "", Name: "no id"},
			{ID: "jacoco", Name: "first"},
			{ID: "jacoco", Name: "second"},
		},
		Warnings: []RunWarningsPayload{
			{ID: "checkstyle", Total: 1},
			{ID: "checkstyle", Total: 2},
		},
		CoverageDropped: -4,
		WarningsDropped: -1,
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.quality with deduped reports: %v", err)
	}

	var coverageDropped, warnDropped int
	if err := db.QueryRow(`SELECT reports_dropped FROM varroa.run_coverage WHERE run_id = $1`,
		runID).Scan(&coverageDropped); err != nil {
		t.Fatalf("read run_coverage: %v", err)
	}
	if coverageDropped != 2 {
		t.Fatalf("coverage reports_dropped = %d, want 2 (the empty id and the duplicate)", coverageDropped)
	}
	if err := db.QueryRow(`SELECT reports_dropped FROM varroa.run_warnings WHERE run_id = $1`,
		runID).Scan(&warnDropped); err != nil {
		t.Fatalf("read run_warnings: %v", err)
	}
	if warnDropped != 1 {
		t.Fatalf("warnings reports_dropped = %d, want 1 (the duplicate alone)", warnDropped)
	}
}

// TestApplyRunQualityMalformedPayload asserts that a payload of the wrong
// shape is refused as a malformed envelope.
func TestApplyRunQualityMalformedPayload(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:quality-bad-1")
	g := NewIngester(pool, nil, nil)

	env := newTestEnvelope(t, runID, "manual:quality-bad-1", KindRunQuality, json.RawMessage(`{"coverage": 5}`))
	_, err := g.Apply(context.Background(), env)
	if !errors.Is(err, ErrMalformedEnvelope) {
		t.Fatalf("err = %v, want ErrMalformedEnvelope", err)
	}
}

// countQualityRows counts the rows of one quality table for one run.
func countQualityRows(t *testing.T, db *sql.DB, table string, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.`+table+` WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
