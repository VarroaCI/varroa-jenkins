package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// insertBranchTestRun inserts one runs row for jsTestCluster with the
// given tenant and job path, and answers its run id.
func insertBranchTestRun(t *testing.T, db *sql.DB, tenant, jobPath string) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status)
		VALUES ($1, now(), $2, $3, $4, $5, $6, $7, 'COMPLETED')
	`, runID, "manual:"+runID.String(), jsTestCluster, jsTestNS, jsTestCell, tenant, jobPath); err != nil {
		t.Fatalf("insert runs row: %v", err)
	}
	return runID
}

func TestGetJobBranches(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	reader := NewReader(pool)
	ctx := context.Background()

	if _, err := g.Apply(ctx, newJobBranchesEnvelope(t, jsTestCell, time.Now().UTC(), JobBranchesPayload{
		Hash: "h1", Projects: []JobBranchProjectInput{oneBranchProject("acme/app")},
	})); err != nil {
		t.Fatalf("apply job.branches: %v", err)
	}

	project, found, err := reader.GetJobBranches(ctx, jsTestCluster, "acme", "acme/app")
	if err != nil {
		t.Fatalf("get job branches: %v", err)
	}
	if !found {
		t.Fatalf("found = false, want the stored inventory")
	}
	if len(project.Heads) != 2 || project.Heads[1].ChangeRequest == nil || project.Heads[1].ChangeRequest.ID != "7" {
		t.Fatalf("heads = %+v, want the branch and the change request 7", project.Heads)
	}
	if project.IndexedAt == nil || project.LastScan != nil {
		t.Fatalf("indexed_at = %v, last_scan = %v, want a time and no scan", project.IndexedAt, project.LastScan)
	}

	_, found, err = reader.GetJobBranches(ctx, jsTestCluster, "acme", "acme/none")
	if err != nil {
		t.Fatalf("get absent job branches: %v", err)
	}
	if found {
		t.Fatalf("found = true for a job with no row")
	}
}

func TestLatestRunsForJobs(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	insertBranchTestRun(t, db, "acme", "acme/app")
	newest := insertBranchTestRun(t, db, "acme", "acme/app")
	insertBranchTestRun(t, db, "acme", "acme/other")

	runs, err := reader.LatestRunsForJobs(ctx, jsTestCluster, "acme", []string{"acme/app", "acme/none"})
	if err != nil {
		t.Fatalf("latest runs: %v", err)
	}
	app, ok := runs["acme/app"]
	if !ok || app.RunID != newest.String() {
		t.Fatalf("acme/app latest = %v (ok=%v), want the newest run %s", app.RunID, ok, newest)
	}
	if _, ok := runs["acme/none"]; ok {
		t.Fatalf("acme/none has a run entry, want none")
	}
	if _, ok := runs["acme/other"]; ok {
		t.Fatalf("acme/other has a run entry, want none: it was not listed")
	}

	empty, err := reader.LatestRunsForJobs(ctx, jsTestCluster, "acme", nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty list = %v, %v, want an empty map and no error", empty, err)
	}
}
