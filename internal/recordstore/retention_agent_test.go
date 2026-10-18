package recordstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedAgentSession inserts one agent session with two events and one
// approval. A nil ended leaves the session open. A skipped session takes
// ended equal to started, as the store writes it.
func seedAgentSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, team, state string, started time.Time, ended *time.Time) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	var skipReason any
	if state == "skipped" {
		skipReason = "budget_day"
		ended = &started
	}
	var endedAt any
	if ended != nil {
		endedAt = *ended
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO varroa.agent_sessions
		  (id, kind, principal, team, cluster, provider, model, key_source, data_sharing,
		   state, skip_reason, started_at, ended_at)
		VALUES ($1, 'team_agent', 'alice', $2, $3, 'anthropic', 'm', 'chart', 'logs',
		        $4, $5, $6, $7)`,
		id, team, "core", state, skipReason, started, endedAt,
	); err != nil {
		t.Fatalf("insert agent session: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO varroa.agent_session_events (session_id, seq, type, payload)
		VALUES ($1, 1, 'user', '{"text":"hi"}'), ($1, 2, 'done', '{"state":"completed"}')`,
		id,
	); err != nil {
		t.Fatalf("insert agent events: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO varroa.agent_approvals
		  (id, session_id, call_id, tool, arguments, target_kind, state, requested_at, expires_at)
		VALUES ($1, $2, 'call-1', 'run_pipeline', '{}', 'none', 'expired', $3, $3)`,
		uuid.Must(uuid.NewV7()), id, started,
	); err != nil {
		t.Fatalf("insert agent approval: %v", err)
	}
	return id
}

// agentRowCount counts the rows of one agent table that belong to session
// id, or every row when id is uuid.Nil.
func agentRowCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string, id uuid.UUID) int {
	t.Helper()
	column := "id"
	if table == "agent_session_events" || table == "agent_approvals" {
		column = "session_id"
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM varroa."+table+" WHERE "+column+" = $1 OR $1 = '00000000-0000-0000-0000-000000000000'::uuid", id).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func agentSessionExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	return agentRowCount(ctx, t, pool, "agent_sessions", id) == 1
}

// TestAgentRetentionDeletesOldSessionWithTranscript asserts that a final
// session past the store window goes with its events and approvals.
func TestAgentRetentionDeletesOldSessionWithTranscript(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	ended := now.Add(-40 * 24 * time.Hour)
	old := seedAgentSession(ctx, t, pool, "acme", "completed", ended.Add(-time.Hour), &ended)

	n, err := deleteExpiredAgentSessions(ctx, pool, now, 30*24*time.Hour, agentRetentionRoundBudget)
	if err != nil {
		t.Fatalf("deleteExpiredAgentSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d sessions, want 1", n)
	}
	if agentSessionExists(ctx, t, pool, old) {
		t.Fatal("expired session still exists")
	}
	for _, table := range []string{"agent_session_events", "agent_approvals"} {
		if got := agentRowCount(ctx, t, pool, table, old); got != 0 {
			t.Fatalf("%s rows left for the deleted session: %d", table, got)
		}
	}
}

// TestAgentRetentionTenantWindowShortensTeamSession asserts that a tenant
// window shorter than the store window deletes that tenant's session and
// keeps another tenant's session of the same age.
func TestAgentRetentionTenantWindowShortensTeamSession(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `
		INSERT INTO varroa.tenant_placements (cluster, tenant, policy)
		VALUES ('core', 'acme', '{"retention":{"runs":"30d"}}')`,
	); err != nil {
		t.Fatalf("insert tenant placement: %v", err)
	}
	ended := now.Add(-40 * 24 * time.Hour)
	acme := seedAgentSession(ctx, t, pool, "acme", "completed", ended.Add(-time.Hour), &ended)
	other := seedAgentSession(ctx, t, pool, "billing", "completed", ended.Add(-time.Hour), &ended)

	if _, err := deleteExpiredAgentSessions(ctx, pool, now, 365*24*time.Hour, agentRetentionRoundBudget); err != nil {
		t.Fatalf("deleteExpiredAgentSessions: %v", err)
	}
	if agentSessionExists(ctx, t, pool, acme) {
		t.Fatal("team session past the tenant window still exists")
	}
	if !agentSessionExists(ctx, t, pool, other) {
		t.Fatal("session of a tenant without a shorter window was deleted")
	}
}

// TestAgentRetentionKeepsWaitingSession asserts that a waiting or running
// session survives, however old it is.
func TestAgentRetentionKeepsWaitingSession(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-400 * 24 * time.Hour)
	waiting := seedAgentSession(ctx, t, pool, "acme", "waiting_approval", old, nil)
	running := seedAgentSession(ctx, t, pool, "acme", "running", old, nil)

	if _, err := deleteExpiredAgentSessions(ctx, pool, now, 30*24*time.Hour, agentRetentionRoundBudget); err != nil {
		t.Fatalf("deleteExpiredAgentSessions: %v", err)
	}
	if !agentSessionExists(ctx, t, pool, waiting) || !agentSessionExists(ctx, t, pool, running) {
		t.Fatal("a waiting or running session was deleted")
	}
}

// TestAgentRetentionSkippedCountsFromStart asserts that a skipped session
// ages from its start, which is also its end.
func TestAgentRetentionSkippedCountsFromStart(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	stale := seedAgentSession(ctx, t, pool, "acme", "skipped", now.Add(-40*24*time.Hour), nil)
	fresh := seedAgentSession(ctx, t, pool, "acme", "skipped", now.Add(-2*24*time.Hour), nil)

	if _, err := deleteExpiredAgentSessions(ctx, pool, now, 30*24*time.Hour, agentRetentionRoundBudget); err != nil {
		t.Fatalf("deleteExpiredAgentSessions: %v", err)
	}
	if agentSessionExists(ctx, t, pool, stale) {
		t.Fatal("skipped session past the window still exists")
	}
	if !agentSessionExists(ctx, t, pool, fresh) {
		t.Fatal("recent skipped session was deleted")
	}
}

// TestReaderAcceptsSchema37 asserts that the record-store reader still
// reads a schema at version 37, so a BFF without the assistant keeps
// working on an older record store.
func TestReaderAcceptsSchema37(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DELETE FROM varroa.goose_db_version WHERE version_id >= 38`); err != nil {
		t.Fatalf("remove migration 00038 record: %v", err)
	}
	version, err := NewReader(pool).SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion at 37: %v", err)
	}
	if version != 37 {
		t.Fatalf("SchemaVersion = %d, want 37", version)
	}
}
