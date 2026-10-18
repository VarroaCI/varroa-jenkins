package recordstore

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestMigration00038AgentTables asserts migration 00038 creates the agent
// tables, their six indexes, and the one global settings row. It also
// asserts the skip-reason CHECK.
func TestMigration00038AgentTables(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	ctx := context.Background()

	var version int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version_id), 0) FROM varroa.goose_db_version WHERE is_applied`,
	).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version < 38 {
		t.Fatalf("schema version = %d, want at least 38", version)
	}

	var indexes int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_indexes
		WHERE schemaname = 'varroa' AND indexname = ANY($1)`,
		[]string{
			"agent_sessions_principal_started_idx", "agent_sessions_team_started_idx",
			"agent_sessions_lease_idx", "agent_approvals_session_idx",
			"agent_approvals_pending_idx", "agent_approvals_target_idx",
		},
	).Scan(&indexes); err != nil {
		t.Fatalf("count agent indexes: %v", err)
	}
	if indexes != 6 {
		t.Fatalf("agent indexes = %d, want 6", indexes)
	}

	var global int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM varroa.agent_settings WHERE scope = 'global'`,
	).Scan(&global); err != nil {
		t.Fatalf("count global settings: %v", err)
	}
	if global != 1 {
		t.Fatalf("global settings rows = %d, want 1", global)
	}

	_, err := db.ExecContext(ctx, `
		INSERT INTO varroa.agent_sessions
		  (id, kind, principal, cluster, provider, model, key_source, data_sharing, state, started_at)
		VALUES ($1, 'assistant', 'alice', 'core', 'anthropic', 'm', 'chart', 'logs', 'skipped', now())`,
		uuid.Must(uuid.NewV7()),
	)
	if err == nil {
		t.Fatal("a skipped session without a skip_reason was accepted")
	}
}
