package recordstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestEnsurePartitionsIdempotent asserts calling EnsurePartitions twice
// for the same instant errors neither time.
func TestEnsurePartitionsIdempotent(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

	if err := EnsurePartitions(context.Background(), pool, now, 30*24*time.Hour); err != nil {
		t.Fatalf("first EnsurePartitions call: %v", err)
	}
	if err := EnsurePartitions(context.Background(), pool, now, 30*24*time.Hour); err != nil {
		t.Fatalf("second EnsurePartitions call: %v", err)
	}
}

// TestEnsurePartitionsActivityReachesBackToRetention asserts a 90-day
// activity retention creates every month's partition back to the
// retention window, not just the four-month window runs and log_chunks
// share.
func TestEnsurePartitionsActivityReachesBackToRetention(t *testing.T) {
	pool, db := setupLedgerPool(t)
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

	if err := EnsurePartitions(context.Background(), pool, now, 90*24*time.Hour); err != nil {
		t.Fatalf("EnsurePartitions: %v", err)
	}

	// 90 days before 2026-09-15 is 2026-06-17, so June, July, August,
	// September, October, and November partitions must all exist.
	for _, suffix := range []string{"202606", "202607", "202608", "202609", "202610", "202611"} {
		var exists bool
		if err := db.QueryRowContext(context.Background(),
			"SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1 AND relnamespace = 'varroa'::regnamespace)",
			"activity_p"+suffix,
		).Scan(&exists); err != nil {
			t.Fatalf("check partition %s: %v", suffix, err)
		}
		if !exists {
			t.Fatalf("want partition activity_p%s to exist for a 90-day retention", suffix)
		}
	}
}

// TestEnsurePartitionsAcceptsPreviousMonthInsert asserts a run whose
// created_at falls in the previous month inserts successfully, because
// EnsurePartitions creates that month's partition.
func TestEnsurePartitionsAcceptsPreviousMonthInsert(t *testing.T) {
	pool, db := setupLedgerPool(t)
	now := time.Date(2026, 9, 1, 0, 30, 0, 0, time.UTC)

	if err := EnsurePartitions(context.Background(), pool, now, 30*24*time.Hour); err != nil {
		t.Fatalf("EnsurePartitions: %v", err)
	}

	previousMonth := time.Date(2026, 8, 31, 23, 0, 0, 0, time.UTC)
	runID := uuid.Must(uuid.NewV7())
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status)
		VALUES ($1, $2, 'manual:previous-month', $3, $4, 'cell-a', $5, $6, 'QUEUED')
	`, runID, previousMonth, testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("insert into previous month's partition: %v", err)
	}
}
