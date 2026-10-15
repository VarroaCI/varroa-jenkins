package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"
)

// newJobBranchesEnvelope builds a job.branches IngestEnvelope for
// jsTestCluster/jsTestNS/cell. Like job.schedules it carries no run_id.
func newJobBranchesEnvelope(t *testing.T, cell string, occurredAt time.Time, payload JobBranchesPayload) IngestEnvelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal job branches payload: %v", err)
	}
	return IngestEnvelope{
		SchemaVersion: 1,
		Kind:          KindJobBranches,
		Cluster:       jsTestCluster,
		Namespace:     jsTestNS,
		Cell:          cell,
		OccurredAt:    occurredAt.Truncate(time.Microsecond),
		Payload:       raw,
	}
}

// oneBranchProject is a project with one branch head and no scan.
func oneBranchProject(jobPath string) JobBranchProjectInput {
	return JobBranchProjectInput{
		Tenant:              "acme",
		PlacementGeneration: 1,
		JobPath:             jobPath,
		IndexedAt:           "2026-10-12T10:00:00Z",
		IndexResult:         "SUCCESS",
		Heads: []JobBranchHeadInput{
			{Name: "main", JobPath: jobPath + "/main", Kind: "branch", Revision: "abc"},
			{Name: "PR-7", JobPath: jobPath + "/PR-7", Kind: "changeRequest", Revision: "def",
				ChangeRequest: &ChangeRequestBranchInput{ID: "7", Title: "Fix", SourceBranch: "fix", TargetBranch: "main"}},
		},
	}
}

// countJobBranchRows counts every stored project row of the test cluster.
// Each test owns its database, so the count is the test's own rows.
func countJobBranchRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`
		SELECT count(*) FROM varroa.job_branch_projects WHERE cluster = $1
	`, jsTestCluster).Scan(&n); err != nil {
		t.Fatalf("count job_branch_projects: %v", err)
	}
	return n
}

func TestApplyJobBranchesUpsertAndDelete(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()
	t1 := time.Now().UTC()

	if _, err := g.Apply(ctx, newJobBranchesEnvelope(t, jsTestCell, t1, JobBranchesPayload{
		Hash:     "h1",
		Total:    1,
		Projects: []JobBranchProjectInput{oneBranchProject("acme/app")},
	})); err != nil {
		t.Fatalf("apply job.branches: %v", err)
	}

	var heads int
	if err := db.QueryRow(`
		SELECT jsonb_array_length(heads) FROM varroa.job_branch_projects WHERE job_path = 'acme/app'
	`).Scan(&heads); err != nil {
		t.Fatalf("read heads: %v", err)
	}
	if heads != 2 {
		t.Fatalf("heads = %d, want 2", heads)
	}

	// A newer, empty, untruncated set deletes the project.
	if _, err := g.Apply(ctx, newJobBranchesEnvelope(t, jsTestCell, t1.Add(time.Minute), JobBranchesPayload{Hash: "h2"})); err != nil {
		t.Fatalf("apply empty job.branches: %v", err)
	}
	if n := countJobBranchRows(t, db); n != 0 {
		t.Fatalf("rows = %d, want 0 after an empty set", n)
	}
}

func TestApplyJobBranchesStaleEnvelopeRejected(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()
	t2 := time.Now().UTC()

	if _, err := g.Apply(ctx, newJobBranchesEnvelope(t, jsTestCell, t2, JobBranchesPayload{
		Hash: "new", Projects: []JobBranchProjectInput{oneBranchProject("acme/app")},
	})); err != nil {
		t.Fatalf("apply newer job.branches: %v", err)
	}

	result, err := g.Apply(ctx, newJobBranchesEnvelope(t, jsTestCell, t2.Add(-time.Minute), JobBranchesPayload{
		Hash: "old", Projects: []JobBranchProjectInput{oneBranchProject("acme/other")},
	}))
	if err != nil {
		t.Fatalf("apply stale job.branches: %v", err)
	}
	if !result.BranchesStale {
		t.Fatalf("result = %+v, want BranchesStale", result)
	}

	var jobPath string
	if err := db.QueryRow(`SELECT job_path FROM varroa.job_branch_projects`).Scan(&jobPath); err != nil {
		t.Fatalf("read job_path: %v", err)
	}
	if jobPath != "acme/app" {
		t.Fatalf("job_path = %q, want the newer set's acme/app", jobPath)
	}
}

func TestApplyJobBranchesTruncatedKeepsRows(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()
	t1 := time.Now().UTC()

	if _, err := g.Apply(ctx, newJobBranchesEnvelope(t, jsTestCell, t1, JobBranchesPayload{
		Hash: "h1", Projects: []JobBranchProjectInput{oneBranchProject("acme/app")},
	})); err != nil {
		t.Fatalf("apply job.branches: %v", err)
	}
	if _, err := g.Apply(ctx, newJobBranchesEnvelope(t, jsTestCell, t1.Add(time.Minute), JobBranchesPayload{
		Hash: "h2", Truncated: true,
	})); err != nil {
		t.Fatalf("apply truncated job.branches: %v", err)
	}
	if n := countJobBranchRows(t, db); n != 1 {
		t.Fatalf("rows = %d, want the omitted project kept on a truncated set", n)
	}
}

func TestApplyJobBranchesHomeGateSkipsNonHome(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()
	// The home of acme/app is slot 1, which cell-b holds.
	seedScheduleMembers(t, db, "acme", map[string]int32{"acme/app": 1})
	now := time.Now().UTC()

	if _, err := g.Apply(ctx, newJobBranchesEnvelope(t, "cell-a", now, JobBranchesPayload{
		Hash: "h1", Projects: []JobBranchProjectInput{oneBranchProject("acme/app")},
	})); err != nil {
		t.Fatalf("apply from the standby cell: %v", err)
	}
	if n := countJobBranchRows(t, db); n != 0 {
		t.Fatalf("rows = %d from a non-home cell, want 0", n)
	}

	// A later envelope from the home cell: an equal time is not newer than
	// the watermark the standby envelope raised.
	if _, err := g.Apply(ctx, newJobBranchesEnvelope(t, "cell-b", now.Add(time.Second), JobBranchesPayload{
		Hash: "h2", Projects: []JobBranchProjectInput{oneBranchProject("acme/app")},
	})); err != nil {
		t.Fatalf("apply from the home cell: %v", err)
	}
	if n := countJobBranchRows(t, db); n != 1 {
		t.Fatalf("rows = %d from the home cell, want 1", n)
	}
}

func TestApplyJobBranchesKeepsLastScanWhenAbsent(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()
	t1 := time.Now().UTC()

	scanned := oneBranchProject("acme/app")
	scanned.LastScan = &BranchScanInput{ScanID: "scan-1", State: "completed", Result: "SUCCESS"}
	if _, err := g.Apply(ctx, newJobBranchesEnvelope(t, jsTestCell, t1, JobBranchesPayload{
		Hash: "h1", Projects: []JobBranchProjectInput{scanned},
	})); err != nil {
		t.Fatalf("apply scanned job.branches: %v", err)
	}

	// A later export carries last_scan as null, as the export does before
	// the scan path fills it.
	if _, err := g.Apply(ctx, newJobBranchesEnvelope(t, jsTestCell, t1.Add(time.Minute), JobBranchesPayload{
		Hash: "h2", Projects: []JobBranchProjectInput{oneBranchProject("acme/app")},
	})); err != nil {
		t.Fatalf("apply unscanned job.branches: %v", err)
	}

	var scanID string
	if err := db.QueryRow(`SELECT last_scan->>'scan_id' FROM varroa.job_branch_projects`).Scan(&scanID); err != nil {
		t.Fatalf("read last scan: %v", err)
	}
	if scanID != "scan-1" {
		t.Fatalf("scan_id = %q, want the stored scan-1 kept", scanID)
	}
}

// TestApplyJobBranchesStoresScanHeadPaths asserts each head of a scan keeps
// its job path through the stored row, so a read can test the head on its
// own job.
func TestApplyJobBranchesStoresScanHeadPaths(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	reader := NewReader(pool)
	ctx := context.Background()

	scanned := oneBranchProject("acme/app")
	scanned.LastScan = &BranchScanInput{
		ScanID: "scan-1", State: "completed", Result: "SUCCESS",
		OrphanedHeadRefs: []BranchScanHeadInput{{Name: "gone", JobPath: "acme/app/gone"}},
	}
	if _, err := g.Apply(ctx, newJobBranchesEnvelope(t, jsTestCell, time.Now().UTC(), JobBranchesPayload{
		Hash: "h1", Projects: []JobBranchProjectInput{scanned},
	})); err != nil {
		t.Fatalf("apply scanned job.branches: %v", err)
	}

	project, found, err := reader.GetJobBranches(ctx, jsTestCluster, "acme", "acme/app")
	if err != nil {
		t.Fatalf("get job branches: %v", err)
	}
	if !found || project.LastScan == nil || len(project.LastScan.OrphanedHeadRefs) != 1 {
		t.Fatalf("found = %v, last scan = %+v, want one orphaned ref", found, project.LastScan)
	}
	if ref := project.LastScan.OrphanedHeadRefs[0]; ref.Name != "gone" || ref.JobPath != "acme/app/gone" {
		t.Fatalf("orphaned ref = %+v, want gone at acme/app/gone", ref)
	}
}

// TestRehomeJobBranchesMovesRowToHome asserts a home move carries the
// stored row to the new home cell, so the old cell's next export cannot
// delete it.
func TestRehomeJobBranchesMovesRowToHome(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	// No placement row exists yet, so cell-a stores the project.
	if _, err := g.Apply(ctx, newJobBranchesEnvelope(t, "cell-a", time.Now().UTC(), JobBranchesPayload{
		Hash: "h1", Projects: []JobBranchProjectInput{oneBranchProject("acme/app")},
	})); err != nil {
		t.Fatalf("apply job.branches: %v", err)
	}

	// The home of acme/app moves to slot 1, which cell-b holds.
	seedScheduleMembers(t, db, "acme", map[string]int32{"acme/app": 1})
	view, exists, err := readTenantPlacementMembers(ctx, pool, jsTestCluster, "acme")
	if err != nil || !exists {
		t.Fatalf("read placement: exists=%v err=%v", exists, err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := rehomeJobBranches(ctx, tx, jsTestCluster, "acme", view, 5); err != nil {
		t.Fatalf("rehome job branches: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var cell string
	if err := db.QueryRow(`SELECT cell FROM varroa.job_branch_projects WHERE job_path = 'acme/app'`).Scan(&cell); err != nil {
		t.Fatalf("read cell: %v", err)
	}
	if cell != "cell-b" {
		t.Fatalf("cell = %q, want the new home cell-b", cell)
	}
}

func TestPurgeTenantJobBranchesDeletesTheTenantRows(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	if _, err := g.Apply(ctx, newJobBranchesEnvelope(t, jsTestCell, time.Now().UTC(), JobBranchesPayload{
		Hash: "h1", Projects: []JobBranchProjectInput{oneBranchProject("acme/app")},
	})); err != nil {
		t.Fatalf("apply job.branches: %v", err)
	}
	if err := purgeTenantJobBranches(ctx, pool, TenantPurgeRequest{
		Cluster: jsTestCluster, Tenant: "acme", PlacementGeneration: 5,
	}); err != nil {
		t.Fatalf("purge tenant job branches: %v", err)
	}
	if n := countJobBranchRows(t, db); n != 0 {
		t.Fatalf("rows = %d after the purge, want 0", n)
	}
}
