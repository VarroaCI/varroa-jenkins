package recordstore

import (
	"context"
	"database/sql"
	"sync"
	"testing"
)

// TestRunMigrationsConcurrent asserts that two runners racing against one
// empty database both succeed, and that exactly one of them applies
// migration 1.
func TestRunMigrationsConcurrent(t *testing.T) {
	dsn := requireTestDSN(t)
	ctx := context.Background()

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() {
		_ = admin.Close()
	})
	if _, err := admin.ExecContext(ctx, "DROP SCHEMA IF EXISTS varroa CASCADE"); err != nil {
		t.Fatalf("drop schema varroa: %v", err)
	}

	const runners = 2
	errs := make([]error, runners)
	var wg sync.WaitGroup
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = RunMigrations(ctx, dsn)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("runner %d: %v", i, err)
		}
	}

	var applied int
	row := admin.QueryRowContext(ctx, `
		SELECT count(*) FROM varroa.goose_db_version
		WHERE version_id = 1 AND is_applied = true
	`)
	if err := row.Scan(&applied); err != nil {
		t.Fatalf("count applied migration rows: %v", err)
	}
	if applied != 1 {
		t.Fatalf("want migration 1 applied exactly once, got %d rows", applied)
	}
}
