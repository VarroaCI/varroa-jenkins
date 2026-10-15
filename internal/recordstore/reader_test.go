package recordstore

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// parseTestUUID parses s as a uuid.UUID, failing the test on error.
func parseTestUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return id
}

// TestListRunsPagesWithNoGapOrRepeat seeds 30 runs and pages through
// them with a small limit, asserting the pages together cover every run
// exactly once.
func TestListRunsPagesWithNoGapOrRepeat(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	ingester := NewIngester(pool, nil, nil)
	reader := NewReader(pool)
	ctx := context.Background()

	const total = 30
	wantRunIDs := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		intentKey := fmt.Sprintf("manual:paging-%d", i)
		resp := mustGrant(t, ledger, newTestClaimRequest(intentKey, 0))
		env := newTestEnvelope(t, parseTestUUID(t, resp.RunID), intentKey, KindRunStarted, RunStartedPayload{})
		if _, err := ingester.Apply(ctx, env); err != nil {
			t.Fatalf("apply run.started for %s: %v", intentKey, err)
		}
		wantRunIDs[resp.RunID] = true
	}

	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := reader.ListRuns(ctx, RunQuery{Cluster: testCluster, Tenant: testTenant, Limit: 7, Cursor: cursor})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		for _, run := range page.Runs {
			if seen[run.RunID] {
				t.Fatalf("run %s returned twice across pages", run.RunID)
			}
			seen[run.RunID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if len(seen) != total {
		t.Fatalf("saw %d distinct runs across pages, want %d", len(seen), total)
	}
	for runID := range wantRunIDs {
		if !seen[runID] {
			t.Fatalf("run %s never appeared in any page", runID)
		}
	}
}

// TestListRunsFiltersStatusAndBranchAcrossTheSameCursor asserts a
// status=RUNNING filter combined with a branch filter returns only
// matching runs, and pages through them with the same cursor an
// unfiltered listing uses.
func TestListRunsFiltersStatusAndBranchAcrossTheSameCursor(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	ingester := NewIngester(pool, nil, nil)
	reader := NewReader(pool)
	ctx := context.Background()

	branchMain := "main"
	branchDev := "dev"
	wantRunIDs := map[string]bool{}

	seedOne := func(intentKey string, branch *string) string {
		resp := mustGrant(t, ledger, newTestClaimRequest(intentKey, 0))
		env := newTestEnvelope(t, parseTestUUID(t, resp.RunID), intentKey, KindRunStarted, RunStartedPayload{Branch: branch})
		if _, err := ingester.Apply(ctx, env); err != nil {
			t.Fatalf("apply run.started for %s: %v", intentKey, err)
		}
		return resp.RunID
	}

	for i := 0; i < 4; i++ {
		runID := seedOne(fmt.Sprintf("manual:running-main-%d", i), &branchMain)
		wantRunIDs[runID] = true
	}
	// Runs that must not appear: a different branch, and a completed run
	// on the same branch.
	seedOne("manual:running-dev-1", &branchDev)
	completedKey := "manual:completed-main-1"
	completedRunID := seedOne(completedKey, &branchMain)
	completedEnv := newTestEnvelope(t, parseTestUUID(t, completedRunID), completedKey, KindRunCompleted, RunCompletedPayload{})
	if _, err := ingester.Apply(ctx, completedEnv); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := reader.ListRuns(ctx, RunQuery{
			Cluster: testCluster, Tenant: testTenant, Status: StatusRunning, Branch: branchMain,
			Limit: 2, Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		for _, run := range page.Runs {
			if run.Status != StatusRunning {
				t.Fatalf("run %s has status %s, want RUNNING", run.RunID, run.Status)
			}
			if run.Branch == nil || *run.Branch != branchMain {
				t.Fatalf("run %s has branch %v, want %s", run.RunID, run.Branch, branchMain)
			}
			seen[run.RunID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if len(seen) != len(wantRunIDs) {
		t.Fatalf("saw %d filtered runs, want %d", len(seen), len(wantRunIDs))
	}
	for runID := range wantRunIDs {
		if !seen[runID] {
			t.Fatalf("expected run %s missing from the filtered pages", runID)
		}
	}
}

// TestGetIntentByKeyKnownAndUnknown asserts a known intent key returns
// its run id, and an unknown key returns found false with no error.
func TestGetIntentByKeyKnownAndUnknown(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	reader := NewReader(pool)
	ctx := context.Background()

	resp := mustGrant(t, ledger, newTestClaimRequest("manual:intent-lookup-1", 0))

	it, found, err := reader.GetIntentByKey(ctx, "manual:intent-lookup-1")
	if err != nil {
		t.Fatalf("GetIntentByKey: %v", err)
	}
	if !found {
		t.Fatal("known intent key returned found=false")
	}
	if it.RunID != resp.RunID {
		t.Fatalf("RunID = %s, want %s", it.RunID, resp.RunID)
	}
	if it.State != stateGranted {
		t.Fatalf("State = %s, want %s", it.State, stateGranted)
	}

	_, found2, err := reader.GetIntentByKey(ctx, "manual:no-such-intent")
	if err != nil {
		t.Fatalf("GetIntentByKey (unknown): %v", err)
	}
	if found2 {
		t.Fatal("unknown intent key returned found=true")
	}
}

// partitionNamePattern extracts a runs partition table name from one
// line of EXPLAIN output.
var partitionNamePattern = regexp.MustCompile(`runs_p\d{6}`)

// TestGetRunScansOnePartition asserts the query plan GetRun issues on
// varroa.runs touches exactly one partition.
func TestGetRunScansOnePartition(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	ingester := NewIngester(pool, nil, nil)
	reader := NewReader(pool)
	ctx := context.Background()

	resp := mustGrant(t, ledger, newTestClaimRequest("manual:explain-1", 0))
	env := newTestEnvelope(t, parseTestUUID(t, resp.RunID), "manual:explain-1", KindRunStarted, RunStartedPayload{})
	if _, err := ingester.Apply(ctx, env); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	run, _, err := reader.GetRun(ctx, resp.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.RunID != resp.RunID {
		t.Fatalf("RunID = %s, want %s", run.RunID, resp.RunID)
	}

	rows, err := db.Query(`EXPLAIN SELECT `+runColumns+` FROM varroa.runs WHERE run_id = $1 AND created_at = $2`,
		resp.RunID, run.CreatedAt)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()

	scannedPartitions := map[string]bool{}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan EXPLAIN line: %v", err)
		}
		for _, name := range partitionNamePattern.FindAllString(line, -1) {
			scannedPartitions[name] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	if len(scannedPartitions) != 1 {
		t.Fatalf("EXPLAIN touched %d runs_p* partitions (%v), want exactly 1", len(scannedPartitions), scannedPartitions)
	}
}

// TestSchemaVersionBelowMinimumFailsEveryMethod asserts every Reader
// method returns ErrSchemaTooOld once the applied schema version is set
// below the compiled minimum.
func TestSchemaVersionBelowMinimumFailsEveryMethod(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	// Lower every applied migration's version_id below the compiled
	// minimum, not just migration 00001's: a later migration (00002,
	// 00003, ...) leaves its own row applied, and MAX(version_id) would
	// otherwise still clear minGooseSchemaVersion.
	if _, err := db.Exec(`UPDATE varroa.goose_db_version SET version_id = 0 WHERE is_applied`); err != nil {
		t.Fatalf("lower schema version: %v", err)
	}

	if _, err := reader.ListJobSummaries(ctx, testCluster, testTenant, time.Now().Add(-30*24*time.Hour)); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("ListJobSummaries error = %v, want ErrSchemaTooOld", err)
	}
	if _, err := reader.GetJobSummary(ctx, testCluster, testTenant, testJobPath); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("GetJobSummary error = %v, want ErrSchemaTooOld", err)
	}
	if _, err := reader.ListRuns(ctx, RunQuery{Cluster: testCluster, Tenant: testTenant}); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("ListRuns error = %v, want ErrSchemaTooOld", err)
	}
	if _, _, err := reader.GetRun(ctx, "00000000-0000-7000-8000-000000000000"); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("GetRun error = %v, want ErrSchemaTooOld", err)
	}
	if _, err := reader.ListArtifacts(ctx, "00000000-0000-7000-8000-000000000000"); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("ListArtifacts error = %v, want ErrSchemaTooOld", err)
	}
	if _, err := reader.ListTests(ctx, "00000000-0000-7000-8000-000000000000"); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("ListTests error = %v, want ErrSchemaTooOld", err)
	}
	if _, err := reader.ListTestCases(ctx, "00000000-0000-7000-8000-000000000000"); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("ListTestCases error = %v, want ErrSchemaTooOld", err)
	}
	if _, err := reader.ListTestCaseHistory(ctx, TestCaseHistoryQuery{
		Cluster: testCluster, Tenant: testTenant, JobPath: testJobPath, ClassName: "c", Name: "n", Limit: 50,
	}); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("ListTestCaseHistory error = %v, want ErrSchemaTooOld", err)
	}
	if _, _, err := reader.GetIntentByKey(ctx, "manual:whatever"); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("GetIntentByKey error = %v, want ErrSchemaTooOld", err)
	}
	if _, err := reader.NextBuildNumber(ctx, testCluster, testTenant, testJobPath); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("NextBuildNumber error = %v, want ErrSchemaTooOld", err)
	}
	if _, err := reader.SchemaVersion(ctx); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("SchemaVersion error = %v, want ErrSchemaTooOld", err)
	}
}

// TestSchemaVersionMissingTableFails asserts SchemaVersion returns
// ErrSchemaTooOld when varroa.goose_db_version does not exist.
func TestSchemaVersionMissingTableFails(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	if _, err := db.Exec(`DROP TABLE varroa.goose_db_version`); err != nil {
		t.Fatalf("drop goose_db_version: %v", err)
	}

	if _, err := reader.SchemaVersion(ctx); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("SchemaVersion error = %v, want ErrSchemaTooOld", err)
	}
}

// TestSchemaVersionDatabaseErrorFails asserts SchemaVersion returns
// ErrSchemaTooOld when the query itself fails, not only when the
// version is readable but too low.
func TestSchemaVersionDatabaseErrorFails(t *testing.T) {
	t.Parallel()

	dsn := requireTestDSN(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	pool.Close()

	reader := NewReader(pool)
	if _, err := reader.SchemaVersion(context.Background()); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("SchemaVersion error = %v, want ErrSchemaTooOld", err)
	}
}
