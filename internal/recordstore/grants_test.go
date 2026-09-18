package recordstore

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// roleExists reports whether role is present anywhere on the Postgres
// instance db connects to. A role is instance-wide, so this answer does
// not depend on which database issued the query.
func roleExists(ctx context.Context, t *testing.T, db *sql.DB, role string) bool {
	t.Helper()

	var exists bool
	if err := db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role,
	).Scan(&exists); err != nil {
		t.Fatalf("check role %s: %v", role, err)
	}
	return exists
}

// ensureTestRole creates role for a grants test, but only when it does not
// already exist. A Postgres role is instance-wide, so another database on
// a shared instance may have created it first. Cleanup revokes only the
// privileges this database granted and never drops the role: DROP ROLE
// fails when another database on the same instance still holds a grant to
// the role.
func ensureTestRole(ctx context.Context, t *testing.T, db *sql.DB, role string) {
	t.Helper()

	if !roleExists(ctx, t, db, role) {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE ROLE %s", role)); err != nil {
			// Another database on the shared instance may have created
			// the role between the check above and this statement.
			if !roleExists(ctx, t, db, role) {
				t.Fatalf("create role %s: %v", role, err)
			}
		}
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = db.ExecContext(cleanupCtx, fmt.Sprintf("DROP OWNED BY %s", role))
	})
}

// TestEnsureGrantsIdempotent asserts EnsureGrants runs with no error when
// the client role is absent, and grants the client role its privileges
// once the role exists. The role-absent assertions run only when the
// role does not already exist on the instance: checking is safe, but
// clearing it first is not — a concurrent worktree's own test can observe
// the role between its own check and its own grant call, so this test
// never runs DROP ROLE. CI always exercises the role-absent path for
// real, because each job starts from a fresh Postgres instance; a local
// shared instance that already carries the role skips the check and logs
// why instead of asserting nothing.
func TestEnsureGrantsIdempotent(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	if roleExists(ctx, t, db, "varroa_client") {
		t.Log("varroa_client already exists on this Postgres instance; skipping the role-absent no-op check")
	} else {
		if err := EnsureGrants(ctx, db); err != nil {
			t.Fatalf("first call with no role: %v", err)
		}
		if err := EnsureGrants(ctx, db); err != nil {
			t.Fatalf("second call with no role: %v", err)
		}
	}

	ensureTestRole(ctx, t, db, "varroa_client")

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

	ensureTestRole(ctx, t, db, "varroa_gateway")

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
