package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// migrateTo applies every migration up to version, against a fresh schema
// "varroa" on the test database. setupTestDB applies the whole set, so a
// test that needs the state before one migration builds its own schema.
//
// goose keeps its dialect and its table name in package variables, so the
// caller holds goosePackageMu across the change and the apply.
func migrateTo(t *testing.T, db *sql.DB, version int64) {
	t.Helper()
	goosePackageMu.Lock()
	defer goosePackageMu.Unlock()
	goose.SetTableName("varroa.goose_db_version")
	if err := goose.UpTo(db, migrationsDir, version); err != nil {
		t.Fatalf("apply migrations up to %d: %v", version, err)
	}
}

// TestMigrationBackfillsTheReceiveStampForALiveRun asserts migration 00012
// copies last_heartbeat_at into heartbeat_received_at for a run that was
// already RUNNING before the upgrade.
//
// A live run whose new column stays null loses its liveness anchor, so the
// first sweep after the upgrade marks the whole fleet interrupted. This test
// is the guard on that outcome.
func TestMigrationBackfillsTheReceiveStampForALiveRun(t *testing.T) {
	dsn := requireTestDSN(t)
	ctx := context.Background()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test connection: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(ctx, "DROP SCHEMA IF EXISTS varroa CASCADE"); err != nil {
		t.Fatalf("drop schema varroa: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA varroa"); err != nil {
		t.Fatalf("create schema varroa: %v", err)
	}
	migrateTo(t, db, 11)

	heartbeat := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	live := uuid.New()
	done := uuid.New()
	insert := `
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, result, ended_at, last_heartbeat_at
		) VALUES ($1, $2, $3, 'core', 'ns', 'cell-a', 'payments', 'team/job', $4, $5, $6, $7)`
	if _, err := db.ExecContext(ctx, insert, live, heartbeat, "manual:live",
		StatusRunning, nil, nil, heartbeat); err != nil {
		t.Fatalf("seed running run: %v", err)
	}
	if _, err := db.ExecContext(ctx, insert, done, heartbeat, "manual:done",
		StatusCompleted, "SUCCESS", heartbeat, heartbeat); err != nil {
		t.Fatalf("seed completed run: %v", err)
	}

	migrateTo(t, db, 12)

	var got *time.Time
	if err := db.QueryRowContext(ctx,
		`SELECT heartbeat_received_at FROM varroa.runs WHERE run_id = $1`, live).Scan(&got); err != nil {
		t.Fatalf("read the running run: %v", err)
	}
	if got == nil {
		t.Fatal("heartbeat_received_at is null for a live run, want the backfilled value")
	}
	if !got.Equal(heartbeat) {
		t.Fatalf("heartbeat_received_at = %v, want %v", *got, heartbeat)
	}

	// A terminal row needs no backfill. The finalize anchor prefers
	// ended_at, which a completed run always holds.
	var terminal *time.Time
	if err := db.QueryRowContext(ctx,
		`SELECT heartbeat_received_at FROM varroa.runs WHERE run_id = $1`, done).Scan(&terminal); err != nil {
		t.Fatalf("read the completed run: %v", err)
	}
	if terminal != nil {
		t.Fatalf("heartbeat_received_at = %v for a completed run, want null", *terminal)
	}
}

// TestMigrationBackfillsTheFleetLockOwner asserts migration 00019 fills
// owner_tenant from the first segment of each stored lock name. Every row
// that exists before the migration belongs to a cell tenant.
func TestMigrationBackfillsTheFleetLockOwner(t *testing.T) {
	dsn := requireTestDSN(t)
	ctx := context.Background()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test connection: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(ctx, "DROP SCHEMA IF EXISTS varroa CASCADE"); err != nil {
		t.Fatalf("drop schema varroa: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA varroa"); err != nil {
		t.Fatalf("create schema varroa: %v", err)
	}
	migrateTo(t, db, 18)

	if _, err := db.ExecContext(ctx, `
		INSERT INTO varroa.fleet_locks (name, run_id, cluster, tenant, state, waiting_since, expires_at)
		VALUES ('alpha/deploy-prod', 'run-1', 'core', 'alpha', 'held', now(), now() + interval '5 minutes'),
		       ('beta/deploy-prod', 'run-2', 'core', 'alpha', 'waiting', now(), now() + interval '5 minutes')
	`); err != nil {
		t.Fatalf("seed fleet locks: %v", err)
	}

	migrateTo(t, db, 19)

	want := map[string]string{"run-1": "alpha", "run-2": "beta"}
	for runID, owner := range want {
		var got string
		if err := db.QueryRowContext(ctx,
			`SELECT owner_tenant FROM varroa.fleet_locks WHERE run_id = $1`, runID).Scan(&got); err != nil {
			t.Fatalf("read the owner of %s: %v", runID, err)
		}
		if got != owner {
			t.Fatalf("owner_tenant of %s = %q, want %q", runID, got, owner)
		}
	}
}
