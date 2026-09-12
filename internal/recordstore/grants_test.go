package recordstore

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestEnsureGrantsIdempotent asserts EnsureGrants runs with no error when
// the client role is absent, and grants the client role its privileges
// once the role exists.
func TestEnsureGrantsIdempotent(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	if err := EnsureGrants(ctx, db); err != nil {
		t.Fatalf("first call with no role: %v", err)
	}
	if err := EnsureGrants(ctx, db); err != nil {
		t.Fatalf("second call with no role: %v", err)
	}

	if _, err := db.ExecContext(ctx, "DROP ROLE IF EXISTS varroa_client"); err != nil {
		t.Fatalf("drop role before create: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE ROLE varroa_client"); err != nil {
		t.Fatalf("create role varroa_client: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = db.ExecContext(cleanupCtx, "DROP OWNED BY varroa_client")
		_, _ = db.ExecContext(cleanupCtx, "DROP ROLE IF EXISTS varroa_client")
	})

	if err := EnsureGrants(ctx, db); err != nil {
		t.Fatalf("call once the role exists: %v", err)
	}

	var canSelect bool
	if err := db.QueryRowContext(ctx,
		"SELECT has_table_privilege('varroa_client', 'varroa.runs', 'SELECT')",
	).Scan(&canSelect); err != nil {
		t.Fatalf("check select privilege: %v", err)
	}
	if !canSelect {
		t.Fatal("want varroa_client to hold SELECT on varroa.runs")
	}

	var canInsertLogChunks bool
	if err := db.QueryRowContext(ctx,
		"SELECT has_table_privilege('varroa_client', 'varroa.log_chunks', 'INSERT')",
	).Scan(&canInsertLogChunks); err != nil {
		t.Fatalf("check insert privilege: %v", err)
	}
	if !canInsertLogChunks {
		t.Fatal("want varroa_client to hold INSERT on varroa.log_chunks")
	}

	var canUseSchema bool
	if err := db.QueryRowContext(ctx,
		"SELECT has_schema_privilege('varroa_client', 'varroa', 'USAGE')",
	).Scan(&canUseSchema); err != nil {
		t.Fatalf("check schema usage privilege: %v", err)
	}
	if !canUseSchema {
		t.Fatal("want varroa_client to hold USAGE on schema varroa")
	}
}

// TestEnsureGrantsGatewayRole asserts varroa_gateway can insert into a
// varroa.log_chunks partition created after EnsureGrants ran, through the
// parent, and cannot UPDATE or DELETE (design D8.2, F8 in the common
// brief). The INSERT-on-the-parent grant carries forward to any later
// partition automatically; the SELECT default privilege does too. Neither
// UPDATE nor DELETE is ever granted, on the parent or by default: the
// recorder stays the record tables' only writer of those verbs.
func TestEnsureGrantsGatewayRole(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	dsn := requireTestDSN(t)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pgx pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := db.ExecContext(ctx, "DROP ROLE IF EXISTS varroa_gateway"); err != nil {
		t.Fatalf("drop role before create: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE ROLE varroa_gateway"); err != nil {
		t.Fatalf("create role varroa_gateway: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = db.ExecContext(cleanupCtx, "DROP OWNED BY varroa_gateway")
		_, _ = db.ExecContext(cleanupCtx, "DROP ROLE IF EXISTS varroa_gateway")
	})

	if err := EnsureGrants(ctx, db); err != nil {
		t.Fatalf("EnsureGrants once the role exists: %v", err)
	}

	// A partition five months out does not exist yet: neither migration
	// 00001 nor the four-month window above covers it. EnsurePartitions
	// creates it after the grant ran, so the assertions below prove the
	// grant reaches a partition it never saw.
	future := time.Now().UTC().AddDate(0, 5, 0)
	if err := EnsurePartitions(ctx, pool, future, 30*24*time.Hour); err != nil {
		t.Fatalf("EnsurePartitions for a future partition: %v", err)
	}

	insertStmt := `INSERT INTO varroa.log_chunks
		(object_key, chunk_seq, created_at, object_class, digest, bytes)
		VALUES ($1, 0, $2, 'log', 'sha256:x', '\x00')`
	createdAt := future

	// database/sql pins a *sql.Conn to one physical connection, so SET
	// ROLE on it stays in effect for every later Exec on the same conn —
	// the insert and the two refusal checks below all run as
	// varroa_gateway, not as the test's admin role.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SET ROLE varroa_gateway"); err != nil {
		t.Fatalf("SET ROLE varroa_gateway: %v", err)
	}
	if _, err := conn.ExecContext(ctx, insertStmt, "gateway-role-test", createdAt); err != nil {
		t.Fatalf("varroa_gateway insert through the parent: %v", err)
	}

	if _, err := conn.ExecContext(ctx,
		"UPDATE varroa.log_chunks SET digest = 'sha256:y' WHERE object_key = $1", "gateway-role-test",
	); err == nil {
		t.Fatal("want varroa_gateway UPDATE on varroa.log_chunks to be refused")
	}
	if _, err := conn.ExecContext(ctx,
		"DELETE FROM varroa.log_chunks WHERE object_key = $1", "gateway-role-test",
	); err == nil {
		t.Fatal("want varroa_gateway DELETE on varroa.log_chunks to be refused")
	}

	if _, err := conn.ExecContext(ctx, "RESET ROLE"); err != nil {
		t.Fatalf("RESET ROLE: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM varroa.log_chunks WHERE object_key = $1", "gateway-role-test",
	).Scan(&count); err != nil {
		t.Fatalf("count inserted row: %v", err)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want 1 (the gateway role's insert)", count)
	}
}
