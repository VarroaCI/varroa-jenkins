package recordstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestApplyRunTestsReplaysToOneRow delivers a run.tests envelope twice
// and asserts one row per (run_id, suite).
func TestApplyRunTestsReplaysToOneRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:tests-replay-1")
	g := NewIngester(pool, nil, nil)

	env := newTestEnvelope(t, runID, "manual:tests-replay-1", KindRunTests, RunTestsPayload{
		Suite: "unit", Total: 10, Failed: 1, Skipped: 2,
	})
	for i := 0; i < 2; i++ {
		if _, err := g.Apply(context.Background(), env); err != nil {
			t.Fatalf("apply run.tests delivery %d: %v", i, err)
		}
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_tests WHERE run_id = $1 AND suite = $2`,
		runID, "unit").Scan(&count); err != nil {
		t.Fatalf("count run_tests: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}

	var total, failed, skipped int
	if err := db.QueryRow(`SELECT tests_total, tests_failed, tests_skipped FROM varroa.runs WHERE run_id = $1`,
		runID).Scan(&total, &failed, &skipped); err != nil {
		t.Fatalf("read runs counters: %v", err)
	}
	if total != 10 || failed != 1 || skipped != 2 {
		t.Fatalf("counters = (%d,%d,%d), want (10,1,2)", total, failed, skipped)
	}
}

// TestApplyRunTestsSumsAcrossSuites asserts runs.tests_total sums every
// suite the run has reported, so a replay never double counts one.
func TestApplyRunTestsSumsAcrossSuites(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:tests-sum-1")
	g := NewIngester(pool, nil, nil)

	unit := newTestEnvelope(t, runID, "manual:tests-sum-1", KindRunTests, RunTestsPayload{
		Suite: "unit", Total: 10, Failed: 1, Skipped: 0,
	})
	integration := newTestEnvelope(t, runID, "manual:tests-sum-1", KindRunTests, RunTestsPayload{
		Suite: "integration", Total: 5, Failed: 0, Skipped: 1,
	})
	if _, err := g.Apply(context.Background(), unit); err != nil {
		t.Fatalf("apply unit: %v", err)
	}
	if _, err := g.Apply(context.Background(), integration); err != nil {
		t.Fatalf("apply integration: %v", err)
	}
	// Replay unit; the sum must not double count it.
	if _, err := g.Apply(context.Background(), unit); err != nil {
		t.Fatalf("replay unit: %v", err)
	}

	var total, failed, skipped int
	if err := db.QueryRow(`SELECT tests_total, tests_failed, tests_skipped FROM varroa.runs WHERE run_id = $1`,
		runID).Scan(&total, &failed, &skipped); err != nil {
		t.Fatalf("read runs counters: %v", err)
	}
	if total != 15 || failed != 1 || skipped != 1 {
		t.Fatalf("counters = (%d,%d,%d), want (15,1,1)", total, failed, skipped)
	}
}

// TestApplyRunArtifactsReplaysToOneRow delivers a run.artifacts envelope
// twice and asserts one row per (run_id, path).
func TestApplyRunArtifactsReplaysToOneRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:artifacts-replay-1")
	g := NewIngester(pool, nil, nil)

	env := newTestEnvelope(t, runID, "manual:artifacts-replay-1", KindRunArtifacts, RunArtifactsPayload{
		Path: "build/out.jar", SizeBytes: 1024, BlobKey: "artifacts/core/team-a/cell-1/out.jar",
	})
	for i := 0; i < 2; i++ {
		if _, err := g.Apply(context.Background(), env); err != nil {
			t.Fatalf("apply run.artifacts delivery %d: %v", i, err)
		}
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_artifacts WHERE run_id = $1 AND path = $2`,
		runID, "build/out.jar").Scan(&count); err != nil {
		t.Fatalf("count run_artifacts: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

// TestApplyRunChangesReplaysToOneRow delivers a run.changes envelope
// twice and asserts the changes column is set once and unchanged.
func TestApplyRunChangesReplaysToOneRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:changes-replay-1")
	g := NewIngester(pool, nil, nil)

	env := newTestEnvelope(t, runID, "manual:changes-replay-1", KindRunChanges, RunChangesPayload{
		Changes: []byte(`[{"commit":"abc123","author":"alice"}]`),
	})
	for i := 0; i < 2; i++ {
		if _, err := g.Apply(context.Background(), env); err != nil {
			t.Fatalf("apply run.changes delivery %d: %v", i, err)
		}
	}

	var commit string
	if err := db.QueryRow(`SELECT changes->0->>'commit' FROM varroa.runs WHERE run_id = $1`, runID).Scan(&commit); err != nil {
		t.Fatalf("read changes: %v", err)
	}
	if commit != "abc123" {
		t.Fatalf("commit = %q, want abc123", commit)
	}
}

// storedCaseNames answers the run's run_test_cases names in name order.
func storedCaseNames(t *testing.T, db *sql.DB, runID uuid.UUID) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM varroa.run_test_cases WHERE run_id = $1 ORDER BY name`, runID)
	if err != nil {
		t.Fatalf("list run_test_cases: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan run_test_cases: %v", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate run_test_cases: %v", err)
	}
	return names
}

func failedCase(name string) RunTestCasePayload {
	d := int64(1250)
	return RunTestCasePayload{
		Suite: "unit", ClassName: "com.example.FooTest", Name: name,
		Status: "FAILED", DurationMs: &d, ErrorDetails: "boom",
	}
}

// TestApplyRunTestsReplacesCaseSet asserts that a case envelope writes one
// row per case with the run's own scope, that a replay changes nothing, and
// that a later envelope replaces the stored set.
func TestApplyRunTestsReplacesCaseSet(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:cases-replace-1")
	g := NewIngester(pool, nil, nil)

	first := newTestEnvelope(t, runID, "manual:cases-replace-1", KindRunTests, RunTestsPayload{
		Suite: "_total", Total: 3, Failed: 3,
		Cases: []RunTestCasePayload{failedCase("a"), failedCase("b"), failedCase("c")},
	})
	for i := 0; i < 2; i++ {
		if _, err := g.Apply(context.Background(), first); err != nil {
			t.Fatalf("apply delivery %d: %v", i, err)
		}
	}
	if got := storedCaseNames(t, db, runID); fmt.Sprint(got) != "[a b c]" {
		t.Fatalf("stored cases after two deliveries = %v, want [a b c]", got)
	}

	var cluster, tenant, jobPath, status, details string
	var duration int64
	var caseCreated, runCreated time.Time
	if err := db.QueryRow(`
		SELECT c.cluster, c.tenant, c.job_path, c.status, c.duration_ms, c.error_details, c.created_at, r.created_at
		FROM varroa.run_test_cases c JOIN varroa.runs r ON r.run_id = c.run_id
		WHERE c.run_id = $1 AND c.name = 'a'`, runID,
	).Scan(&cluster, &tenant, &jobPath, &status, &duration, &details, &caseCreated, &runCreated); err != nil {
		t.Fatalf("read case row: %v", err)
	}
	if cluster != testCluster || tenant != testTenant || jobPath != testJobPath {
		t.Fatalf("scope = (%q,%q,%q), want the run's own", cluster, tenant, jobPath)
	}
	if status != "FAILED" || duration != 1250 || details != "boom" {
		t.Fatalf("case = (%q,%d,%q)", status, duration, details)
	}
	if !caseCreated.Equal(runCreated) {
		t.Fatalf("case created_at %v, want the run's %v", caseCreated, runCreated)
	}

	second := newTestEnvelope(t, runID, "manual:cases-replace-1", KindRunTests, RunTestsPayload{
		Suite: "_total", Total: 1, Failed: 1,
		Cases: []RunTestCasePayload{failedCase("z")},
	})
	if _, err := g.Apply(context.Background(), second); err != nil {
		t.Fatalf("apply second: %v", err)
	}
	if got := storedCaseNames(t, db, runID); fmt.Sprint(got) != "[z]" {
		t.Fatalf("stored cases after replace = %v, want [z]", got)
	}
}

// TestApplyRunTestsAbsentCasesKeepsSet asserts that a payload with no
// cases key changes no case row, and that an empty list clears the set.
func TestApplyRunTestsAbsentCasesKeepsSet(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:cases-absent-1")
	g := NewIngester(pool, nil, nil)

	seed := newTestEnvelope(t, runID, "manual:cases-absent-1", KindRunTests, RunTestsPayload{
		Suite: "_total", Total: 1, Failed: 1, Cases: []RunTestCasePayload{failedCase("a")},
	})
	if _, err := g.Apply(context.Background(), seed); err != nil {
		t.Fatalf("apply seed: %v", err)
	}

	absent := newTestEnvelope(t, runID, "manual:cases-absent-1", KindRunTests, map[string]any{
		"suite": "_total", "total": 1, "failed": 1, "skipped": 0,
	})
	if _, err := g.Apply(context.Background(), absent); err != nil {
		t.Fatalf("apply absent: %v", err)
	}
	if got := storedCaseNames(t, db, runID); fmt.Sprint(got) != "[a]" {
		t.Fatalf("stored cases after an absent key = %v, want [a]", got)
	}

	empty := newTestEnvelope(t, runID, "manual:cases-absent-1", KindRunTests, map[string]any{
		"suite": "_total", "total": 1, "failed": 0, "skipped": 0, "cases": []any{},
	})
	if _, err := g.Apply(context.Background(), empty); err != nil {
		t.Fatalf("apply empty: %v", err)
	}
	if got := storedCaseNames(t, db, runID); len(got) != 0 {
		t.Fatalf("stored cases after an empty list = %v, want none", got)
	}
}

// TestApplyRunTestsDedupesCases asserts that a payload naming one case
// twice writes one row, keeps the first, and fails no transaction.
func TestApplyRunTestsDedupesCases(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:cases-dup-1")
	g := NewIngester(pool, nil, nil)

	dup := failedCase("a")
	dup.Status = "REGRESSION"
	env := newTestEnvelope(t, runID, "manual:cases-dup-1", KindRunTests, RunTestsPayload{
		Suite: "_total", Total: 2, Failed: 2,
		Cases: []RunTestCasePayload{failedCase("a"), dup, failedCase("b")},
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := storedCaseNames(t, db, runID); fmt.Sprint(got) != "[a b]" {
		t.Fatalf("stored cases = %v, want [a b]", got)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM varroa.run_test_cases WHERE run_id = $1 AND name = 'a'`, runID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "FAILED" {
		t.Fatalf("status = %q, want the first case's FAILED", status)
	}
}

// TestApplyRunTestsStoresReport asserts the report key and the payload
// merge, and that a later envelope with no report keeps the stored key.
func TestApplyRunTestsStoresReport(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:report-1")
	g := NewIngester(pool, nil, nil)

	withReport := newTestEnvelope(t, runID, "manual:report-1", KindRunTests, RunTestsPayload{
		Suite: "_total", Total: 4, Failed: 1,
		TestReport: &RunTestReportPayload{BlobKey: "artifacts/core/job/1/artifacts/.varroa/tests.json", SizeBytes: 2048, CaseCount: 4},
	})
	if _, err := g.Apply(context.Background(), withReport); err != nil {
		t.Fatalf("apply with report: %v", err)
	}
	assertReport := func(wantKey string, wantSize, wantCount int64) {
		t.Helper()
		var key sql.NullString
		var size, count sql.NullInt64
		if err := db.QueryRow(`
			SELECT test_report_key, (payload->'test_report'->>'size_bytes')::bigint,
			       (payload->'test_report'->>'case_count')::bigint
			FROM varroa.runs WHERE run_id = $1`, runID).Scan(&key, &size, &count); err != nil {
			t.Fatalf("read report: %v", err)
		}
		if key.String != wantKey || size.Int64 != wantSize || count.Int64 != wantCount {
			t.Fatalf("report = (%q,%d,%d), want (%q,%d,%d)", key.String, size.Int64, count.Int64, wantKey, wantSize, wantCount)
		}
	}
	assertReport("artifacts/core/job/1/artifacts/.varroa/tests.json", 2048, 4)

	noReport := newTestEnvelope(t, runID, "manual:report-1", KindRunTests, RunTestsPayload{
		Suite: "_total", Total: 4, Failed: 1,
	})
	if _, err := g.Apply(context.Background(), noReport); err != nil {
		t.Fatalf("apply without report: %v", err)
	}
	assertReport("artifacts/core/job/1/artifacts/.varroa/tests.json", 2048, 4)

	emptyKey := newTestEnvelope(t, runID, "manual:report-1", KindRunTests, RunTestsPayload{
		Suite: "_total", Total: 4, Failed: 1,
		TestReport: &RunTestReportPayload{SizeBytes: 4096, CaseCount: 5},
	})
	if _, err := g.Apply(context.Background(), emptyKey); err != nil {
		t.Fatalf("apply empty key: %v", err)
	}
	assertReport("artifacts/core/job/1/artifacts/.varroa/tests.json", 4096, 5)
}

// TestApplyRunTestsMergesReportOntoNullPayload asserts the report reaches a
// runs row whose payload is NULL, the state a heartbeat-first placeholder
// row holds.
func TestApplyRunTestsMergesReportOntoNullPayload(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:report-null-1")
	g := NewIngester(pool, nil, nil)

	seed := newTestEnvelope(t, runID, "manual:report-null-1", KindRunTests, RunTestsPayload{Suite: "_total", Total: 1})
	if _, err := g.Apply(context.Background(), seed); err != nil {
		t.Fatalf("apply seed: %v", err)
	}
	if _, err := db.Exec(`UPDATE varroa.runs SET payload = NULL WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("clear payload: %v", err)
	}

	env := newTestEnvelope(t, runID, "manual:report-null-1", KindRunTests, RunTestsPayload{
		Suite: "_total", Total: 1,
		TestReport: &RunTestReportPayload{SizeBytes: 100, CaseCount: 0},
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply report: %v", err)
	}
	var report sql.NullString
	if err := db.QueryRow(`SELECT payload->>'test_report' FROM varroa.runs WHERE run_id = $1`, runID).Scan(&report); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !report.Valid || !strings.Contains(report.String, `"case_count": 0`) || !strings.Contains(report.String, `"size_bytes": 100`) {
		t.Fatalf("payload test_report = %+v, want the merged object", report)
	}
}
