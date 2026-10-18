package recordstore

import (
	"context"
	"testing"
)

// TestEnsureGrantsClientAgentTables asserts the varroa_client grants on
// the agent tables: insert and update succeed, delete and an insert on
// "runs" fail, and the role holds no DELETE grant. It stays serial because
// it creates the cluster-wide role varroa_client.
func TestEnsureGrantsClientAgentTables(t *testing.T) {
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

	if _, err := conn.ExecContext(ctx, `INSERT INTO varroa.agent_settings (scope) VALUES ('user:alice')`); err != nil {
		t.Fatalf("insert agent settings as varroa_client: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE varroa.agent_settings SET paused = true WHERE scope = 'user:alice'`); err != nil {
		t.Fatalf("update agent settings as varroa_client: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM varroa.agent_session_events`); err == nil {
		t.Fatal("delete on agent_session_events succeeded as varroa_client")
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO varroa.runs DEFAULT VALUES`); err == nil {
		t.Fatal("insert on runs succeeded as varroa_client")
	}
	if _, err := conn.ExecContext(ctx, `RESET ROLE`); err != nil {
		t.Fatalf("RESET ROLE: %v", err)
	}

	var deletes int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.role_table_grants
		WHERE grantee = 'varroa_client' AND privilege_type = 'DELETE'`,
	).Scan(&deletes); err != nil {
		t.Fatalf("count DELETE grants: %v", err)
	}
	if deletes != 0 {
		t.Fatalf("varroa_client holds %d DELETE grants, want 0", deletes)
	}
}
