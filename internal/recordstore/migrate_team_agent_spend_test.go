package recordstore

import (
	"context"
	"testing"
)

// TestMigration00040TeamAgentSpend asserts migration 00040 creates the
// team_agent_spend table with its primary key.
func TestMigration00040TeamAgentSpend(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	ctx := context.Background()

	var pk int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage k
		  ON k.constraint_name = tc.constraint_name AND k.table_schema = tc.table_schema
		WHERE tc.table_schema = 'varroa' AND tc.table_name = 'team_agent_spend'
		  AND tc.constraint_type = 'PRIMARY KEY' AND k.column_name IN ('team', 'day')
	`).Scan(&pk); err != nil {
		t.Fatalf("read primary key: %v", err)
	}
	if pk != 2 {
		t.Fatalf("team_agent_spend primary key columns = %d, want 2 (team, day)", pk)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO varroa.team_agent_spend (team, day, tokens) VALUES ('t', DATE '2026-10-18', 5)`); err != nil {
		t.Fatalf("insert a spend row: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO varroa.team_agent_spend (team, day) VALUES ('t', DATE '2026-10-18')`); err == nil {
		t.Fatal("a second row for one Team and day succeeded")
	}
}

// TestTeamAgentSpendClientCannotDelete checks the grants: varroa_client may
// insert and update the spend table, and may not delete from it.
func TestTeamAgentSpendClientCannotDelete(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	ensureTestRole(ctx, t, db, "varroa_client")
	if err := EnsureGrants(ctx, db); err != nil {
		t.Fatalf("EnsureGrants: %v", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("open connection: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET ROLE varroa_client"); err != nil {
		t.Fatalf("SET ROLE varroa_client: %v", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "RESET ROLE")
	}()

	if _, err := conn.ExecContext(ctx, `INSERT INTO varroa.team_agent_spend (team, day) VALUES ('grant-t', DATE '2026-10-17')`); err != nil {
		t.Fatalf("insert as varroa_client: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE varroa.team_agent_spend SET tokens = 9 WHERE team = 'grant-t'`); err != nil {
		t.Fatalf("update as varroa_client: %v", err)
	}
	_, err = conn.ExecContext(ctx, `DELETE FROM varroa.team_agent_spend`)
	if err == nil {
		t.Fatal("delete on team_agent_spend succeeded as varroa_client")
	}
	if _, rerr := conn.ExecContext(ctx, `RESET ROLE`); rerr != nil {
		t.Fatalf("RESET ROLE: %v", rerr)
	}
}
