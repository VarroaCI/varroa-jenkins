package recordstore

import (
	"context"
	"testing"
)

// TestEnsureGrantsClientRunNotesSelectOnly asserts that varroa_client reads
// varroa.run_notes and cannot write it. The recorder is the only writer of
// the table. It stays serial because it creates the cluster-wide role
// varroa_client.
func TestEnsureGrantsClientRunNotesSelectOnly(t *testing.T) {
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

	var n int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM varroa.run_notes`).Scan(&n); err != nil {
		t.Fatalf("select run_notes as varroa_client: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO varroa.run_notes (id, cluster, team, job_path, principal, body)
		VALUES (gen_random_uuid(), 'c', 't', 'j', 'p', 'x')`); err == nil {
		t.Fatal("insert on run_notes succeeded as varroa_client")
	}
}
