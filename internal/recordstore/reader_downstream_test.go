package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// insertLinkedRun writes one varroa.runs row with the upstream columns
// the dependency and downstream tests read. upstreamRun is "" for a run
// that no run started.
func insertLinkedRun(t *testing.T, db *sql.DB, runID uuid.UUID, createdAt time.Time, tenant, job, status, result, upstreamRun, upTenant, upJob string, number int64) {
	t.Helper()
	var up any
	if upstreamRun != "" {
		up = upstreamRun
	}
	var resultArg any
	if result != "" {
		resultArg = result
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			build_number, status, result, upstream_run_id, upstream_tenant, upstream_job, upstream_number)
		VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, $8, $9, $10, $11::uuid, NULLIF($12, ''), NULLIF($13, ''), $14)
	`, runID, createdAt, "manual:"+runID.String(), testCluster, testNamespace, tenant, job,
		number, status, resultArg, up, upTenant, upJob, number); err != nil {
		t.Fatalf("insert linked run %s: %v", runID, err)
	}
}

// TestListDownstreamRuns_NewestFirstAndKeysetPaging asserts the downstream
// list returns the runs an upstream run started, newest first, and pages
// them by keyset.
func TestListDownstreamRuns_NewestFirstAndKeysetPaging(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	upstream := uuid.Must(uuid.NewV7())
	base := time.Now().UTC().Truncate(time.Microsecond)
	insertLinkedRun(t, db, upstream, base, testTenant, testJobPath, "COMPLETED", "SUCCESS", "", "", "", 1)
	children := make([]uuid.UUID, 3)
	for i := range children {
		children[i] = uuid.Must(uuid.NewV7())
		insertLinkedRun(t, db, children[i], base.Add(time.Duration(i+1)*time.Second), "beta", "beta/rebuild",
			"RUNNING", "", upstream.String(), testTenant, testJobPath, int64(i+1))
	}

	first, err := reader.ListDownstreamRuns(ctx, DownstreamRunQuery{UpstreamRunID: upstream.String(), Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Runs) != 2 || first.Runs[0].RunID != children[2].String() || first.Runs[1].RunID != children[1].String() {
		t.Fatalf("first page = %+v, want newest two", first.Runs)
	}
	if first.NextCursor == "" {
		t.Fatalf("first page has no next cursor")
	}

	second, err := reader.ListDownstreamRuns(ctx, DownstreamRunQuery{UpstreamRunID: upstream.String(), Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Runs) != 1 || second.Runs[0].RunID != children[0].String() || second.NextCursor != "" {
		t.Fatalf("second page = %+v cursor %q, want the oldest run and no cursor", second.Runs, second.NextCursor)
	}
	if second.Runs[0].UpstreamRunID == nil || second.Runs[0].UpstreamRunID.String() != upstream.String() {
		t.Fatalf("downstream run upstream id = %v, want %s", second.Runs[0].UpstreamRunID, upstream)
	}
}

// TestListDownstreamRuns_EmptyForNoChild asserts a run that started nothing
// answers an empty page with no error.
func TestListDownstreamRuns_EmptyForNoChild(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)

	lonely := uuid.Must(uuid.NewV7())
	insertLinkedRun(t, db, lonely, time.Now().UTC(), testTenant, testJobPath, "COMPLETED", "SUCCESS", "", "", "", 1)

	page, err := reader.ListDownstreamRuns(context.Background(), DownstreamRunQuery{UpstreamRunID: lonely.String()})
	if err != nil {
		t.Fatalf("ListDownstreamRuns: %v", err)
	}
	if len(page.Runs) != 0 || page.NextCursor != "" {
		t.Fatalf("page = %+v, want empty", page)
	}
}

// TestListDownstreamRuns_BadInputIsAnError asserts a malformed upstream run
// id and a malformed cursor both fail.
func TestListDownstreamRuns_BadInputIsAnError(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	if _, err := reader.ListDownstreamRuns(ctx, DownstreamRunQuery{UpstreamRunID: "not-a-uuid"}); err == nil {
		t.Fatalf("want an error for a malformed upstream run id")
	}
	valid := uuid.Must(uuid.NewV7()).String()
	if _, err := reader.ListDownstreamRuns(ctx, DownstreamRunQuery{UpstreamRunID: valid, Cursor: "%%%"}); err == nil {
		t.Fatalf("want an error for a malformed cursor")
	}
}

// TestGetRunScansUpstreamColumns asserts GetRun reads the four upstream
// columns into the Run.
func TestGetRunScansUpstreamColumns(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)

	upstream := uuid.Must(uuid.NewV7())
	insertLinkedRun(t, db, upstream, time.Now().UTC(), testTenant, testJobPath, "COMPLETED", "SUCCESS", "", "", "", 1)
	child := uuid.Must(uuid.NewV7())
	insertLinkedRun(t, db, child, time.Now().UTC(), "beta", "beta/rebuild", "RUNNING", "", upstream.String(), testTenant, testJobPath, 1)

	run, _, err := reader.GetRun(context.Background(), child.String())
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.UpstreamRunID == nil || run.UpstreamRunID.String() != upstream.String() {
		t.Fatalf("UpstreamRunID = %v, want %s", run.UpstreamRunID, upstream)
	}
	if run.UpstreamTenant == nil || *run.UpstreamTenant != testTenant || run.UpstreamJob == nil || *run.UpstreamJob != testJobPath {
		t.Fatalf("upstream tenant/job = %v/%v", run.UpstreamTenant, run.UpstreamJob)
	}
	if run.UpstreamNumber == nil || *run.UpstreamNumber != 1 {
		t.Fatalf("UpstreamNumber = %v, want 1", run.UpstreamNumber)
	}
}

// TestJobDependencies_LastRunOnEdges asserts an upstream edge carries the
// upstream run that last started the job, and a downstream edge carries the
// subscriber's newest run that the producer started.
func TestJobDependencies_LastRunOnEdges(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	seedGrantPlacement(t, db, testCluster, "alpha", "jenkins", "cell-alpha", 1,
		`{"exports":[{"job":"release","callers":["beta"]}]}`)
	seedGrantPlacement(t, db, testCluster, "beta", "jenkins", "cell-beta", 1, "{}")
	seedUpstreamTrigger(t, db, testCluster, "beta/rebuild")

	producerRun := uuid.Must(uuid.NewV7())
	now := time.Now().UTC()
	insertLinkedRun(t, db, producerRun, now, "alpha", "alpha/release", "COMPLETED", "SUCCESS", "", "", "", 1)
	subscriberRun := uuid.Must(uuid.NewV7())
	insertLinkedRun(t, db, subscriberRun, now.Add(time.Second), "beta", "beta/rebuild", "RUNNING", "",
		producerRun.String(), "alpha", "alpha/release", 1)

	subscriber, err := reader.JobDependencies(ctx, testCluster, "beta", "beta/rebuild")
	if err != nil {
		t.Fatalf("JobDependencies(beta/rebuild): %v", err)
	}
	if len(subscriber.Upstream) != 1 || subscriber.Upstream[0].LastRun == nil {
		t.Fatalf("subscriber upstream = %+v, want a last run", subscriber.Upstream)
	}
	if got := subscriber.Upstream[0].LastRun.RunID; got != producerRun.String() {
		t.Fatalf("upstream last run = %q, want %s", got, producerRun)
	}

	producer, err := reader.JobDependencies(ctx, testCluster, "alpha", "alpha/release")
	if err != nil {
		t.Fatalf("JobDependencies(alpha/release): %v", err)
	}
	if len(producer.Downstream) != 1 || producer.Downstream[0].LastRun == nil {
		t.Fatalf("producer downstream = %+v, want a last run", producer.Downstream)
	}
	last := producer.Downstream[0].LastRun
	if last.RunID != subscriberRun.String() || last.Status != "RUNNING" {
		t.Fatalf("downstream last run = %+v, want %s RUNNING", last, subscriberRun)
	}
}

// TestJobDependencies_NoLastRunOutsideWindow asserts a run that began before
// the window's start is never reported as the edge's last run.
func TestJobDependencies_NoLastRunOutsideWindow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	seedGrantPlacement(t, db, testCluster, "alpha", "jenkins", "cell-alpha", 1,
		`{"exports":[{"job":"release","callers":["beta"]}]}`)
	seedGrantPlacement(t, db, testCluster, "beta", "jenkins", "cell-beta", 1, "{}")
	seedUpstreamTrigger(t, db, testCluster, "beta/rebuild")

	producerRun := uuid.Must(uuid.NewV7())
	insertLinkedRun(t, db, producerRun, time.Now().UTC(), "alpha", "alpha/release", "COMPLETED", "SUCCESS", "", "", "", 1)
	subscriberRun := uuid.Must(uuid.NewV7())
	insertLinkedRun(t, db, subscriberRun, time.Now().UTC(), "beta", "beta/rebuild", "COMPLETED", "SUCCESS",
		producerRun.String(), "alpha", "alpha/release", 1)

	// The window is measured from now, and the test partitions cover only
	// the current month, so the boundary is checked with a future since.
	link, err := lastUpstreamRun(ctx, pool, testCluster, "beta", "beta/rebuild", "alpha", "alpha/release", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("lastUpstreamRun: %v", err)
	}
	if link != nil {
		t.Fatalf("lastUpstreamRun = %+v, want nil for a run older than since", link)
	}
	if _, err := reader.JobDependencies(ctx, testCluster, "beta", "beta/rebuild"); err != nil {
		t.Fatalf("JobDependencies(beta/rebuild): %v", err)
	}
}
