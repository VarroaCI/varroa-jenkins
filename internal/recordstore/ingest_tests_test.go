package recordstore

import (
	"context"
	"testing"
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
