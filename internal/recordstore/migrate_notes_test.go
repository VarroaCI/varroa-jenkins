package recordstore

import (
	"context"
	"testing"
)

// TestMigration00039RunNotes asserts migration 00039 creates the run_notes
// indexes and the constraints the writer relies on.
func TestMigration00039RunNotes(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	ctx := context.Background()

	var version int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM varroa.goose_db_version WHERE is_applied`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version < 39 {
		t.Fatalf("schema version = %d, want at least 39", version)
	}

	var indexes int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_indexes
		WHERE schemaname = 'varroa' AND tablename = 'run_notes'
		  AND indexname IN ('run_notes_run_idx', 'run_notes_job_idx', 'run_notes_run_created_idx', 'run_notes_session_idx')
	`).Scan(&indexes); err != nil {
		t.Fatalf("count indexes: %v", err)
	}
	if indexes != 4 {
		t.Fatalf("run_notes indexes = %d, want 4", indexes)
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO varroa.run_notes (id, cluster, tenant, job_path, principal, body)
		VALUES (gen_random_uuid(), 'c', 't', 'j', 'p', 'a job note')`); err != nil {
		t.Fatalf("a job note must insert: %v", err)
	}
	sessionID := "0190a1f0-0000-7000-8000-000000000001"
	if _, err := db.ExecContext(ctx, `INSERT INTO varroa.run_notes (id, cluster, tenant, job_path, principal, session_id, body)
		VALUES (gen_random_uuid(), 'c', 't', 'j', 'p', $1, 'first')`, sessionID); err != nil {
		t.Fatalf("first session note: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO varroa.run_notes (id, cluster, tenant, job_path, principal, session_id, body)
		VALUES (gen_random_uuid(), 'c', 't', 'j', 'p', $1, 'second')`, sessionID); err == nil {
		t.Fatal("a second note for one session succeeded")
	}
}
