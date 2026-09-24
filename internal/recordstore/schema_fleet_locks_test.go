package recordstore

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// insertFleetLock inserts one varroa.fleet_locks row directly, so a
// schema test can drive one state combination with no acquire
// transaction.
func insertFleetLock(ctx context.Context, db *sql.DB, name, runID, state string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO varroa.fleet_locks (name, owner_tenant, run_id, state, waiting_since, expires_at)
		VALUES ($1, split_part($1, '/', 1), $2, $3, now(), now() + interval '5 minutes')
	`, name, runID, state)
	return err
}

// TestFleetLocksOneHolderRejectsASecondHeldRow asserts the partial
// unique index refuses a second "held" row for one lock name.
func TestFleetLocksOneHolderRejectsASecondHeldRow(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	if err := insertFleetLock(ctx, db, "alpha/deploy-prod", "run-1", "held"); err != nil {
		t.Fatalf("first held row: %v", err)
	}
	if err := insertFleetLock(ctx, db, "alpha/deploy-prod", "run-2", "held"); err == nil {
		t.Fatal("want a unique violation for a second held row, got none")
	}
}

// TestFleetLocksHeldBesideReleasingIsRejected asserts the partial unique
// index covers "releasing" too, so a lock under cancellation admits no
// second holder.
func TestFleetLocksHeldBesideReleasingIsRejected(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	if err := insertFleetLock(ctx, db, "alpha/deploy-prod", "run-1", "releasing"); err != nil {
		t.Fatalf("releasing row: %v", err)
	}
	if err := insertFleetLock(ctx, db, "alpha/deploy-prod", "run-2", "held"); err == nil {
		t.Fatal("want a unique violation for a held row beside a releasing row, got none")
	}
}

// TestFleetLocksNameOverLongIsRejected asserts the 200-byte CHECK
// constraint refuses a 201-byte qualified name.
func TestFleetLocksNameOverLongIsRejected(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	name := "alpha/" + strings.Repeat("x", 195) // 6 + 195 bytes = 201 bytes.
	if len(name) != 201 {
		t.Fatalf("test setup: want a 201-byte name, got %d bytes", len(name))
	}
	if err := insertFleetLock(ctx, db, name, "run-1", "waiting"); err == nil {
		t.Fatal("want a check violation for a 201-byte name, got none")
	}
}

// TestFleetLocksDefaults asserts fence_token defaults to 0 and
// releasing_since and acquired_at stay null on an ordinary insert.
func TestFleetLocksDefaults(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	if err := insertFleetLock(ctx, db, "alpha/deploy-prod", "run-1", "waiting"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var (
		fenceToken         int64
		acquiredAtNull     bool
		releasingSinceNull bool
	)
	if err := db.QueryRowContext(ctx, `
		SELECT fence_token, acquired_at IS NULL, releasing_since IS NULL
		FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2
	`, "alpha/deploy-prod", "run-1").Scan(&fenceToken, &acquiredAtNull, &releasingSinceNull); err != nil {
		t.Fatalf("read defaults: %v", err)
	}
	if fenceToken != 0 {
		t.Fatalf("fence_token default: want 0, got %d", fenceToken)
	}
	if !acquiredAtNull {
		t.Fatal("acquired_at default: want NULL, got a value")
	}
	if !releasingSinceNull {
		t.Fatal("releasing_since default: want NULL, got a value")
	}
}

// TestFleetLockFenceSeqIsMonotonic asserts varroa.fleet_lock_fence_seq
// hands out a strictly increasing sequence, fleet-wide.
func TestFleetLockFenceSeqIsMonotonic(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	var first, second int64
	if err := db.QueryRowContext(ctx, `SELECT nextval('varroa.fleet_lock_fence_seq')`).Scan(&first); err != nil {
		t.Fatalf("first nextval: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT nextval('varroa.fleet_lock_fence_seq')`).Scan(&second); err != nil {
		t.Fatalf("second nextval: %v", err)
	}
	if second <= first {
		t.Fatalf("want a strictly increasing token, got %d then %d", first, second)
	}
}

// TestFleetLocksOwnerTenantHasNoDefault asserts an insert that omits
// owner_tenant fails. A row with an empty owner would hold its name
// against every caller and appear in no owner's read.
func TestFleetLocksOwnerTenantHasNoDefault(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO varroa.fleet_locks (name, run_id, state, waiting_since, expires_at)
		VALUES ('alpha/deploy-prod', 'run-1', 'waiting', now(), now() + interval '5 minutes')
	`); err == nil {
		t.Fatal("want a not-null violation for an insert with no owner_tenant, got none")
	}
}
