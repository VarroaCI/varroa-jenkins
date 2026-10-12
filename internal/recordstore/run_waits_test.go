package recordstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// insertRunWait writes one wait row with the given first and last refresh.
func insertRunWait(t *testing.T, seed func(string, ...any) error, runID uuid.UUID, first, last time.Time) {
	t.Helper()
	if err := seed(`
		INSERT INTO varroa.run_waits
			(run_id, cluster, namespace, tenant, job_path, cell, cap_kind, cap_name, cap_max, running,
			 first_seen_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, 'cell-a', 'tenant', 'builds', 1, 1, $6, $7)`,
		runID, testCluster, testNamespace, testTenant, testTenant+"/app/build", first, last); err != nil {
		t.Fatalf("insert wait row: %v", err)
	}
}

func TestSweepRunWaits_DeletesStaleRows(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	now := time.Now().UTC()
	fresh := uuid.Must(uuid.NewV7())
	stale := uuid.Must(uuid.NewV7())
	insertRunWait(t, func(q string, args ...any) error { _, err := db.Exec(q, args...); return err },
		fresh, now, now)
	insertRunWait(t, func(q string, args ...any) error { _, err := db.Exec(q, args...); return err },
		stale, now.Add(-2*time.Hour), now.Add(-2*time.Hour))

	n, err := sweepRunWaits(context.Background(), pool, now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted = %d, want 1", n)
	}
	if waitRowCount(t, db, fresh.String()) != 1 || waitRowCount(t, db, stale.String()) != 0 {
		t.Fatal("the sweep kept the wrong rows")
	}
}

func TestGetRunWait_LiveOnly(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	reader := NewReader(pool)
	now := time.Now().UTC()
	live := uuid.Must(uuid.NewV7())
	dead := uuid.Must(uuid.NewV7())
	seed := func(q string, args ...any) error { _, err := db.Exec(q, args...); return err }
	insertRunWait(t, seed, live, now, now)
	insertRunWait(t, seed, dead, now.Add(-time.Hour), now.Add(-time.Hour))

	got, ok, err := reader.GetRunWait(ctx, live.String())
	if err != nil || !ok || got.Kind != "tenant" || got.Limit != "builds" {
		t.Fatalf("live wait = %+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, err := reader.GetRunWait(ctx, dead.String()); err != nil || ok {
		t.Fatalf("stale wait ok=%v err=%v, want no row", ok, err)
	}
}

func TestListJobRunWaits_OrderedBySince(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	reader := NewReader(pool)
	now := time.Now().UTC()
	newer := uuid.Must(uuid.NewV7())
	older := uuid.Must(uuid.NewV7())
	seed := func(q string, args ...any) error { _, err := db.Exec(q, args...); return err }
	insertRunWait(t, seed, newer, now.Add(-time.Minute), now)
	insertRunWait(t, seed, older, now.Add(-10*time.Minute), now)

	rows, err := reader.ListJobRunWaits(ctx, testCluster, testTenant, testTenant+"/app/build")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 || rows[0].RunID != older.String() {
		t.Fatalf("rows = %+v, want the longest waiting first", rows)
	}
}
