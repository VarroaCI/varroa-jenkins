package recordstore

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// TestMigration00041RenameTeamToTenant seeds rows at version 40, then
// applies migration 00041, reverses it, and applies it again. It asserts
// the renamed columns, table, and index, the rewritten stored values, and
// that the stored data survives each step.
func TestMigration00041RenameTeamToTenant(t *testing.T) {
	t.Parallel()

	dsn := emptyTestDB(t)
	ctx := context.Background()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test connection: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(ctx, "CREATE SCHEMA varroa"); err != nil {
		t.Fatalf("create schema varroa: %v", err)
	}
	migrateTo(t, db, 40)
	seedMigration00041(ctx, t, db)

	migrateTo(t, db, 41)
	assertMigration00041Up(ctx, t, db)

	goosePackageMu.Lock()
	goose.SetTableName("varroa.goose_db_version")
	if err := goose.DownTo(db, migrationsDir, 40); err != nil {
		goosePackageMu.Unlock()
		t.Fatalf("reverse migration 00041: %v", err)
	}
	goosePackageMu.Unlock()
	assertMigration00041Down(ctx, t, db)

	migrateTo(t, db, 41)
	assertMigration00041Up(ctx, t, db)
}

// seedMigration00041 inserts one row of each table that migration 00041
// changes. The rows use the version 40 names.
func seedMigration00041(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	mustExec(`
		INSERT INTO varroa.agent_sessions
		  (id, kind, principal, team, cluster, provider, model, key_source, data_sharing,
		   team_agent_generation, state, started_at)
		VALUES ($1, 'team_agent', 'alice', 'alpha', 'core', 'anthropic', 'm', 'chart', 'logs',
		        7, 'completed', now())`, uuid.New())
	mustExec(`INSERT INTO varroa.team_agent_spend (team, day, tokens) VALUES ('alpha', DATE '2026-10-18', 42)`)
	mustExec(`
		INSERT INTO varroa.run_notes (id, cluster, team, job_path, principal, body)
		VALUES ($1, 'core', 'alpha', 'build/app', 'alice', 'a note')`, uuid.New())
	mustExec(`
		INSERT INTO varroa.activity (ts, event_key, type, payload)
		VALUES (now(), 'seed-note', 'team_agent.note_posted', '{}'),
		       (now(), 'seed-run', 'teamagent.run_requested', '{}'),
		       (now(), 'seed-other', 'run.completed', '{}')`)
	mustExec(`
		UPDATE varroa.agent_settings
		SET settings = '{"team_agent_unattended": {"ns/one": "granted"}}'
		WHERE scope = 'global'`)
}

// assertMigration00041Up checks the state after migration 00041 applies.
func assertMigration00041Up(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	assertInt(ctx, t, db, 2, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'varroa' AND table_name = 'agent_sessions'
		  AND column_name IN ('tenant', 'tenant_agent_generation')`, "renamed agent_sessions columns")
	assertInt(ctx, t, db, 0, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'varroa' AND table_name = 'agent_sessions'
		  AND column_name IN ('team', 'team_agent_generation')`, "old agent_sessions columns")
	assertText(ctx, t, db, "tenant_agent", `
		SELECT kind FROM varroa.agent_sessions WHERE principal = 'alice'`, "rewritten kind")
	assertText(ctx, t, db, "alpha", `
		SELECT tenant FROM varroa.agent_sessions WHERE principal = 'alice'`, "agent_sessions.tenant value")
	assertInt(ctx, t, db, 1, `
		SELECT count(*) FROM pg_indexes
		WHERE schemaname = 'varroa' AND indexname = 'agent_sessions_tenant_started_idx'`, "renamed index")
	assertInt(ctx, t, db, 0, `
		SELECT count(*) FROM pg_indexes
		WHERE schemaname = 'varroa' AND indexname = 'agent_sessions_team_started_idx'`, "old index")
	assertInt(ctx, t, db, 1, `
		SELECT count(*) FROM pg_constraint
		WHERE conname = 'agent_sessions_kind_check' AND conrelid = 'varroa.agent_sessions'::regclass
		  AND pg_get_constraintdef(oid) LIKE '%tenant_agent%'`, "kind check value list")
	assertRejects(ctx, t, db, `
		INSERT INTO varroa.agent_sessions
		  (id, kind, principal, cluster, provider, model, key_source, data_sharing, state, started_at)
		VALUES ($1, 'team_agent', 'bob', 'core', 'anthropic', 'm', 'chart', 'logs', 'completed', now())`,
		"kind check still accepts team_agent")
	assertInt(ctx, t, db, 1, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'varroa' AND table_name = 'tenant_agent_spend'`, "renamed spend table")
	assertInt(ctx, t, db, 0, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'varroa' AND table_name = 'team_agent_spend'`, "old spend table")
	assertInt(ctx, t, db, 42, `
		SELECT tokens::int FROM varroa.tenant_agent_spend WHERE tenant = 'alpha'`, "spend row kept")
	assertInt(ctx, t, db, 1, `
		SELECT count(*) FROM pg_constraint
		WHERE conname = 'tenant_agent_spend_pkey' AND conrelid = 'varroa.tenant_agent_spend'::regclass`,
		"renamed spend primary key")
	assertText(ctx, t, db, "alpha", `SELECT tenant FROM varroa.run_notes`, "run_notes.tenant value")
	assertInt(ctx, t, db, 1, `
		SELECT count(*) FROM varroa.activity WHERE type = 'tenant_agent.note_posted'`, "rewritten note type")
	assertInt(ctx, t, db, 1, `
		SELECT count(*) FROM varroa.activity WHERE type = 'tenantagent.run_requested'`, "rewritten run type")
	assertInt(ctx, t, db, 1, `
		SELECT count(*) FROM varroa.activity WHERE type = 'run.completed'`, "untouched activity type")
	assertInt(ctx, t, db, 1, `
		SELECT count(*) FROM varroa.agent_settings
		WHERE scope = 'global' AND settings ? 'tenant_agent_unattended'
		  AND NOT settings ? 'team_agent_unattended'
		  AND settings->'tenant_agent_unattended'->>'ns/one' = 'granted'`, "rewritten settings key")
}

// assertMigration00041Down checks the state after migration 00041 reverses.
func assertMigration00041Down(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	assertInt(ctx, t, db, 2, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'varroa' AND table_name = 'agent_sessions'
		  AND column_name IN ('team', 'team_agent_generation')`, "restored agent_sessions columns")
	assertText(ctx, t, db, "team_agent", `
		SELECT kind FROM varroa.agent_sessions WHERE principal = 'alice'`, "restored kind")
	assertInt(ctx, t, db, 1, `
		SELECT count(*) FROM pg_indexes
		WHERE schemaname = 'varroa' AND indexname = 'agent_sessions_team_started_idx'`, "restored index")
	assertInt(ctx, t, db, 1, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'varroa' AND table_name = 'team_agent_spend'`, "restored spend table")
	assertInt(ctx, t, db, 1, `
		SELECT count(*) FROM pg_constraint
		WHERE conname = 'team_agent_spend_pkey' AND conrelid = 'varroa.team_agent_spend'::regclass`,
		"restored spend primary key")
	assertText(ctx, t, db, "alpha", `SELECT team FROM varroa.run_notes`, "restored run_notes.team")
	assertInt(ctx, t, db, 1, `
		SELECT count(*) FROM varroa.activity WHERE type = 'team_agent.note_posted'`, "restored note type")
	assertInt(ctx, t, db, 1, `
		SELECT count(*) FROM varroa.agent_settings
		WHERE scope = 'global' AND settings ? 'team_agent_unattended'
		  AND settings->'team_agent_unattended'->>'ns/one' = 'granted'`, "restored settings key")
}

// assertInt runs a query that returns one integer and fails when it differs
// from want.
func assertInt(ctx context.Context, t *testing.T, db *sql.DB, want int, query, what string) {
	t.Helper()
	var got int
	if err := db.QueryRowContext(ctx, query).Scan(&got); err != nil {
		t.Fatalf("%s: query: %v", what, err)
	}
	if got != want {
		t.Fatalf("%s = %d, want %d", what, got, want)
	}
}

// assertText runs a query that returns one text value and fails when it
// differs from want.
func assertText(ctx context.Context, t *testing.T, db *sql.DB, want, query, what string) {
	t.Helper()
	var got string
	if err := db.QueryRowContext(ctx, query).Scan(&got); err != nil {
		t.Fatalf("%s: query: %v", what, err)
	}
	if got != want {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
}

// assertRejects runs a statement that the schema must refuse and fails when
// the statement succeeds.
func assertRejects(ctx context.Context, t *testing.T, db *sql.DB, query, what string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, query, uuid.New()); err == nil {
		t.Fatalf("%s: statement succeeded", what)
	}
}
