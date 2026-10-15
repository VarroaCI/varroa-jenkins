package recordstore

import (
	"context"
	"testing"
	"time"
)

// TestSweepIntentsLeavesLiveClaim asserts a claim granted two minutes ago,
// with no run.started yet, survives the sweep.
func TestSweepIntentsLeavesLiveClaim(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:live-claim-1", 0)
	resp := mustGrant(t, ledger, req)

	if err := EnsurePartitions(ctx, pool, time.Now().UTC(), 30*24*time.Hour); err != nil {
		t.Fatalf("EnsurePartitions: %v", err)
	}

	if _, _, err := sweepIntents(ctx, pool, time.Now().UTC()); err != nil {
		t.Fatalf("sweepIntents: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_intents WHERE run_id = $1`, resp.RunID).Scan(&count); err != nil {
		t.Fatalf("count run_intents: %v", err)
	}
	if count != 1 {
		t.Fatalf("live claim's intent was swept; count = %d, want 1", count)
	}
}

// TestSweepIntentsOutlivesRunUnderMonthlyRounding asserts step 7's lower
// bound comes from the oldest surviving runs partition, so an intent
// whose run monthly rounding still keeps is not deleted.
func TestSweepIntentsOutlivesRunUnderMonthlyRounding(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	// A single old partition stands in for "monthly rounding keeps this
	// run longer than retention.runs alone would."
	oldMonth := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := createPartition(ctx, pool, "runs", oldMonth.Format("200601"), oldMonth, oldMonth.AddDate(0, 1, 0)); err != nil {
		t.Fatalf("create old runs partition: %v", err)
	}

	oldIntentCreatedAt := oldMonth.Add(12 * time.Hour)
	runID := newTestUUIDv7String()
	if _, err := db.Exec(`
		INSERT INTO varroa.run_intents
			(intent_key, run_id, created_at, cluster, namespace, tenant, job_path, state)
		VALUES ('manual:old-but-live-1', $1, $2, $3, $4, $5, $6, 'CONSUMED')
	`, runID, oldIntentCreatedAt, testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("seed old intent: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status)
		VALUES ($1, $2, 'manual:old-but-live-1', $3, $4, 'cell-a', $5, $6, 'COMPLETED')
	`, runID, oldIntentCreatedAt, testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("seed old run: %v", err)
	}

	if _, _, err := sweepIntents(ctx, pool, time.Now().UTC()); err != nil {
		t.Fatalf("sweepIntents: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_intents WHERE run_id = $1`, runID).Scan(&count); err != nil {
		t.Fatalf("count run_intents: %v", err)
	}
	if count != 1 {
		t.Fatalf("intent outlived by its own surviving partition was swept; count = %d, want 1", count)
	}
}

// TestSweepIntentsLeavesJobNumbers asserts a full sweep of a job's runs
// and intents leaves the job's job_numbers row unchanged.
func TestSweepIntentsLeavesJobNumbers(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:watermark-1", 0)
	resp := mustGrant(t, ledger, req)

	var before int64
	if err := db.QueryRow(`
		SELECT next_number FROM varroa.job_numbers WHERE cluster = $1 AND tenant = $2 AND job_path = $3
	`, testCluster, testTenant, testJobPath).Scan(&before); err != nil {
		t.Fatalf("read job_numbers before sweep: %v", err)
	}

	// Delete every run and intent of the job directly, standing in for a
	// full sweep of steps 4 through 8 against the job's own data.
	if _, err := db.Exec(`DELETE FROM varroa.runs WHERE job_path = $1`, testJobPath); err != nil {
		t.Fatalf("delete runs: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM varroa.run_intents WHERE job_path = $1`, testJobPath); err != nil {
		t.Fatalf("delete run_intents: %v", err)
	}

	// The full round, not just sweepIntents, so the assertion covers
	// every step's chance to touch job_numbers, not only steps 7 and 8.
	if _, err := RunMaintenanceRound(ctx, pool, MaintenanceConfig{
		RunRetention:      365 * 24 * time.Hour,
		LogRetention:      90 * 24 * time.Hour,
		ArtifactRetention: 30 * 24 * time.Hour,
		FinalizeWait:      time.Minute,
	}); err != nil {
		t.Fatalf("RunMaintenanceRound: %v", err)
	}

	var after int64
	if err := db.QueryRow(`
		SELECT next_number FROM varroa.job_numbers WHERE cluster = $1 AND tenant = $2 AND job_path = $3
	`, testCluster, testTenant, testJobPath).Scan(&after); err != nil {
		t.Fatalf("read job_numbers after sweep: %v", err)
	}
	if after != before {
		t.Fatalf("job_numbers.next_number changed from %d to %d after a full sweep", before, after)
	}
	if resp.BuildNumber == 0 {
		t.Fatalf("sanity check: expected a granted build number")
	}
}
