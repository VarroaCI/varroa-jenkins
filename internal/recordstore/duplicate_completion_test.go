package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// duplicatePair is two runs of one revision of the test job. The duplicate
// backstop marked the newer run DUPLICATE. The older run stays live.
type duplicatePair struct {
	older, duplicate       uuid.UUID
	olderKey, duplicateKey string
}

// seedDuplicatePair grants and starts two runs of one revision through g.
// Both runs hold an api: key, which the duplicate backstop compares. The
// second run.started runs the backstop, which marks the newer run
// DUPLICATE. Each run.started names a repository, so a source-gone
// completion has the facts of an end_commit_status action.
func seedDuplicatePair(t *testing.T, pool *pgxpool.Pool, g *Ingester, name string) duplicatePair {
	t.Helper()
	p := duplicatePair{olderKey: "api:" + name + "-older", duplicateKey: "api:" + name + "-duplicate"}
	p.older = seedGrantedIntent(t, pool, p.olderKey)
	p.duplicate = seedGrantedIntent(t, pool, p.duplicateKey)
	runs := []struct {
		id  uuid.UUID
		key string
	}{{p.older, p.olderKey}, {p.duplicate, p.duplicateKey}}
	for i, run := range runs {
		env := newTestEnvelope(t, run.id, run.key, KindRunStarted, map[string]any{
			"build_number": i + 1, "scm_revision": "rev-" + name, "scm_url": "https://git.example.org/acme/app",
		})
		if _, err := g.Apply(context.Background(), env); err != nil {
			t.Fatalf("apply run.started %d: %v", i+1, err)
		}
	}
	if got := runStatus(t, g, p.duplicate); got != StatusDuplicate {
		t.Fatalf("newer run status = %q, want %q", got, StatusDuplicate)
	}
	return p
}

// completeTestRun applies the owner run.completed of one run with result
// and durationMs. extra adds payload fields.
func completeTestRun(t *testing.T, g *Ingester, runID uuid.UUID, key, result string, durationMs int64, extra map[string]any) ApplyResult {
	t.Helper()
	payload := map[string]any{"result": result, "duration_ms": durationMs, "seq": 2}
	for k, v := range extra {
		payload[k] = v
	}
	res, err := g.Apply(context.Background(), newTestEnvelope(t, runID, key, KindRunCompleted, payload))
	if err != nil {
		t.Fatalf("apply run.completed %s: %v", result, err)
	}
	if !res.CompletedOwner {
		t.Fatalf("run.completed %s reports a foreign write, want the owner write", result)
	}
	return res
}

// countDuplicateRows answers the row count of one query with one argument.
func countDuplicateRows(t *testing.T, db *sql.DB, query string, arg any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, arg).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// testRunScopes is the scope list of the whole test tenant.
var testRunScopes = []RunScope{{Cluster: testCluster, Tenant: testTenant}}

// TestDuplicateRunStaysOutOfTheJobAggregates asserts that an owner-completed
// DUPLICATE run counts in no job or fleet aggregate: the Last 30 days stats,
// the health window, the duration trend, and the overview totals.
func TestDuplicateRunStaysOutOfTheJobAggregates(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	p := seedDuplicatePair(t, pool, g, "dup-aggregates")
	completeTestRun(t, g, p.duplicate, p.duplicateKey, "ABORTED", 50, nil)
	completeTestRun(t, g, p.older, p.olderKey, "SUCCESS", 1000, nil)

	ctx := context.Background()
	r := NewReader(pool)

	stats, err := r.JobWindowStats(ctx, testCluster, testTenant, testJobPath, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("JobWindowStats: %v", err)
	}
	if stats.Builds != 1 || stats.PassRate == nil || *stats.PassRate != 1 {
		t.Fatalf("stats = builds %d, pass rate %v; want 1 build at pass rate 1", stats.Builds, stats.PassRate)
	}
	if stats.MedianDurationMs == nil || *stats.MedianDurationMs != 1000 {
		t.Fatalf("median duration = %v, want 1000", stats.MedianDurationMs)
	}

	summary, err := r.GetJobSummary(ctx, testCluster, testTenant, testJobPath)
	if err != nil {
		t.Fatalf("GetJobSummary: %v", err)
	}
	if len(summary.RecentRuns) != 1 || summary.RecentRuns[0].RunID != p.older.String() {
		t.Fatalf("health window = %+v, want the older run alone", summary.RecentRuns)
	}
	if len(summary.DurationTrendMs) != 1 || summary.DurationTrendMs[0] != 1000 {
		t.Fatalf("duration trend = %v, want [1000]", summary.DurationTrendMs)
	}
	if summary.LastRun == nil || summary.LastRun.RunID != p.older.String() {
		t.Fatalf("last run = %+v, want the older run", summary.LastRun)
	}

	ov, err := r.RunsOverview(ctx, RunsOverviewQuery{Scopes: testRunScopes})
	if err != nil {
		t.Fatalf("RunsOverview: %v", err)
	}
	if got := ov.Totals; got.Runs24h != 1 || got.Finished != 1 || got.Counted != 1 || got.Passed != 1 {
		t.Fatalf("overview totals = %+v, want one finished, counted, passed run", got)
	}
	if len(ov.Scopes) != 1 || len(ov.Scopes[0].Recent) != 1 || ov.Scopes[0].Recent[0].RunID != p.older.String() {
		t.Fatalf("overview scopes = %+v, want the older run as the one recent run", ov.Scopes)
	}

	page, err := r.ListRuns(ctx, RunQuery{Cluster: testCluster, Tenant: testTenant, JobPath: testJobPath})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(page.Runs) != 2 {
		t.Fatalf("run list holds %d runs, want both runs: a run list keeps the DUPLICATE run", len(page.Runs))
	}
}

// TestDuplicateRunIsNeverTheNewestCountedRun asserts that an owner-completed
// DUPLICATE run never hides the failure of the run it duplicates. The
// failing-jobs read lists the job, and the run groups hold the failure.
func TestDuplicateRunIsNeverTheNewestCountedRun(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	p := seedDuplicatePair(t, pool, g, "dup-newest")
	completeTestRun(t, g, p.older, p.olderKey, "FAILURE", 1000, nil)
	completeTestRun(t, g, p.duplicate, p.duplicateKey, "ABORTED", 50, nil)

	ctx := context.Background()
	r := NewReader(pool)
	now := time.Now().UTC()

	failing, err := r.FailingJobs(ctx, FailingJobsQuery{Scopes: testRunScopes, Since: now.Add(-time.Hour), Limit: 10})
	if err != nil {
		t.Fatalf("FailingJobs: %v", err)
	}
	if len(failing.Jobs) != 1 || failing.Jobs[0].Failed.RunID != p.older.String() {
		t.Fatalf("failing jobs = %+v, want the older run's failure", failing.Jobs)
	}

	groups, err := r.RunGroups(ctx, RunGroupsQuery{Scopes: testRunScopes, Since: now.Add(-time.Hour), Now: now.Add(time.Minute)})
	if err != nil {
		t.Fatalf("RunGroups: %v", err)
	}
	if groups.Held != 1 || len(groups.Groups) != 0 {
		t.Fatalf("run groups = held %d, %d groups; want the failure held and no group", groups.Held, len(groups.Groups))
	}
}

// TestOwnerCompletionOfADuplicateRunRunsNoFanout asserts that an owner
// completion of a DUPLICATE run arms no fan-out round, even with a result
// that meets the subscriber's threshold. The older run's completion still
// fans out.
func TestOwnerCompletionOfADuplicateRunRunsNoFanout(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedGrantPlacement(t, db, testCluster, testTenant, testNamespace, "cell-a", 1, "{}")
	conn := newTestActionConn(t)
	metrics, err := NewUpstreamFanoutMetrics(noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("new fan-out metrics: %v", err)
	}
	g := NewIngester(pool, conn, nil)
	g.SetUpstreamFanout(NewGrantEvaluator(pool), metrics)

	subscriber := testTenant + "/consumer"
	if _, err := db.Exec(`
		INSERT INTO varroa.job_schedules
			(cluster, namespace, cell, tenant, job_path, trigger_type, observed_at, payload, spec,
			 upstream_tenant, upstream_job, upstream_result, disabled)
		VALUES ($1, $2, 'cell-a', $3, $4, 'upstream', now(), '{}', $5, '', '', 'SUCCESS', false)
	`, testCluster, testNamespace, testTenant, subscriber, testJobPath); err != nil {
		t.Fatalf("seed native subscriber: %v", err)
	}

	p := seedDuplicatePair(t, pool, g, "dup-fanout")
	received := subscribeCancelActions(t, conn, 8)
	nextTrigger := func(wait time.Duration) *LedgerAction {
		deadline := time.After(wait)
		for {
			select {
			case a := <-received:
				if a.Kind == LedgerActionTriggerRun {
					return &a
				}
			case <-deadline:
				return nil
			}
		}
	}

	completeTestRun(t, g, p.duplicate, p.duplicateKey, "SUCCESS", 50, nil)
	if a := nextTrigger(500 * time.Millisecond); a != nil {
		t.Fatalf("the DUPLICATE run's completion published %+v, want no trigger_run", a)
	}
	if n := countDuplicateRows(t, db, `SELECT count(*) FROM varroa.runs WHERE run_id = $1 AND payload ? 'fanout_at'`, p.duplicate); n != 0 {
		t.Fatal("the DUPLICATE run holds a fan-out marker, want none")
	}

	completeTestRun(t, g, p.older, p.olderKey, "SUCCESS", 1000, nil)
	a := nextTrigger(3 * time.Second)
	if a == nil || a.JobPath != subscriber {
		t.Fatalf("the older run's completion published %+v, want one trigger_run for %s", a, subscriber)
	}
}

// TestOwnerCompletionOfADuplicateRunEnqueuesNoWebhookAndNoBuildFailed
// asserts that an owner completion of a DUPLICATE run writes no completion
// webhook delivery and no build.failed activity row. The older run's
// completion still writes both.
func TestOwnerCompletionOfADuplicateRunEnqueuesNoWebhookAndNoBuildFailed(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	g.SetWebhooks("https://varroa.example.test", nil)
	p := seedDuplicatePair(t, pool, g, "dup-webhook")
	stampRunIntentIncarnation(t, db, p.older)
	stampRunIntentIncarnation(t, db, p.duplicate)
	seedTenantPlacement(t, db, testTenant, testNamespace, "cell-a", 0, "pod-a", "", nil)
	stampWebhookIncarnation(t, db)
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET notification_webhook = $1::jsonb
		WHERE cluster = $2 AND tenant = $3
	`, `{"url":"https://receiver.example.test/hook","key":"whsec-test"}`, testCluster, testTenant); err != nil {
		t.Fatalf("store tenant webhook: %v", err)
	}

	const deliveries = `SELECT count(*) FROM varroa.webhook_deliveries WHERE run_id = $1`
	const buildFailed = `SELECT count(*) FROM varroa.activity WHERE type = 'build.failed' AND item_path = $1`

	completeTestRun(t, g, p.duplicate, p.duplicateKey, "ABORTED", 50, nil)
	if n := countDuplicateRows(t, db, deliveries, p.duplicate); n != 0 {
		t.Fatalf("the DUPLICATE run holds %d webhook deliveries, want 0", n)
	}
	if n := countDuplicateRows(t, db, buildFailed, testJobPath); n != 0 {
		t.Fatalf("the DUPLICATE run wrote %d build.failed rows, want 0", n)
	}

	completeTestRun(t, g, p.older, p.olderKey, "FAILURE", 1000, nil)
	if n := countDuplicateRows(t, db, deliveries, p.older); n != 1 {
		t.Fatalf("the older run holds %d webhook deliveries, want 1", n)
	}
	if n := countDuplicateRows(t, db, buildFailed, testJobPath); n != 1 {
		t.Fatalf("the older run wrote %d build.failed rows, want 1", n)
	}
}

// TestOwnerCompletionOfADuplicateRunKeepsTheCleanup asserts that an owner
// completion of a DUPLICATE run still releases the run's fleet locks,
// deletes its run state, and ends the pending commit status of a branch job
// that lost its source.
func TestOwnerCompletionOfADuplicateRunKeepsTheCleanup(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	s := startDuplicateTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	g := NewIngester(pool, conn, nil)
	deleter := &prefixRecordingStore{}
	g.SetStateDeleter(deleter)

	p := seedDuplicatePair(t, pool, g, "dup-cleanup")
	now := time.Now().UTC()
	insertFleetLockRow(t, db, "alpha/deploy-prod", p.duplicate.String(), "held", now, now.Add(time.Hour))
	received := subscribeEndStatusForTest(t, conn)

	res := completeTestRun(t, g, p.duplicate, p.duplicateKey, "ABORTED", 50, map[string]any{"source_gone": true})
	if len(res.ReleasedFleetLocks) != 1 {
		t.Fatalf("released %d fleet locks, want 1", len(res.ReleasedFleetLocks))
	}
	if n := countDuplicateRows(t, db, `SELECT count(*) FROM varroa.fleet_locks WHERE run_id = $1`, p.duplicate.String()); n != 0 {
		t.Fatalf("the DUPLICATE run keeps %d fleet lock rows, want 0", n)
	}
	if len(deleter.prefixes) != 1 {
		t.Fatalf("run-state prefix deletes = %v, want one for the DUPLICATE run", deleter.prefixes)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case a := <-received:
			if a.Kind != LedgerActionEndCommitStatus {
				continue
			}
			if a.RunID != p.duplicate.String() || a.Result != "ABORTED" {
				t.Fatalf("end_commit_status = %+v, want the DUPLICATE run's ABORTED end", a)
			}
			return
		case <-deadline:
			t.Fatal("no end_commit_status action for the DUPLICATE run")
		}
	}
}
