package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestUpstreamRunFromIntentKey covers the key forms the recorder reads
// for an upstream link. Only an upstream key with a UUIDv7 run id and a
// non-empty sequence answers true.
func TestUpstreamRunFromIntentKey(t *testing.T) {
	t.Parallel()

	id := newTestUUIDv7String()
	cases := []struct {
		name string
		key  string
		ok   bool
	}{
		{"upstream key", "upstream:" + id + ":1", true},
		{"varroaUpstream fan-out key", "upstream:" + id + ":tabc123", true},
		{"missing sequence", "upstream:" + id, false},
		{"empty sequence", "upstream:" + id + ":", false},
		{"not a uuid", "upstream:not-a-uuid:1", false},
		{"uuid v4", "upstream:" + uuid.NewString() + ":1", false},
		{"manual key", "manual:" + id, false},
		{"retry key", "retry:" + id, false},
		{"empty key", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, ok := upstreamRunFromIntentKey(tc.key)
			if ok != tc.ok {
				t.Fatalf("upstreamRunFromIntentKey(%q) ok = %v, want %v", tc.key, ok, tc.ok)
			}
		})
	}
}

// upstreamColumns is one run's stored upstream columns.
type upstreamColumns struct {
	runID, tenant, job sql.NullString
	number             sql.NullInt64
}

// readUpstreamColumns reads the four upstream columns of one run.
func readUpstreamColumns(t *testing.T, db *sql.DB, runID uuid.UUID) upstreamColumns {
	t.Helper()
	var got upstreamColumns
	if err := db.QueryRow(`
		SELECT upstream_run_id::text, upstream_tenant, upstream_job, upstream_number
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&got.runID, &got.tenant, &got.job, &got.number); err != nil {
		t.Fatalf("read upstream columns: %v", err)
	}
	return got
}

// mustApply applies env through g and fails the test on error.
func mustApply(t *testing.T, g *Ingester, env IngestEnvelope) {
	t.Helper()
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply %s: %v", env.Kind, err)
	}
}

// TestApplyRunStartedStampsUpstreamFromIntentKey asserts a run that an
// upstream intent key names stores the upstream run's id, tenant, job, and
// number.
func TestApplyRunStartedStampsUpstreamFromIntentKey(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	upstreamID := seedGrantedIntent(t, pool, "manual:up-stamp-1")
	upNumber := int64(7)
	mustApply(t, g, newTestEnvelope(t, upstreamID, "manual:up-stamp-1", KindRunStarted, RunStartedPayload{BuildNumber: &upNumber}))

	seedUpstreamOrigin(t, pool, "upstream:"+upstreamID.String()+":1")
	childID := seedGrantedIntent(t, pool, "upstream:"+upstreamID.String()+":1")
	childNumber := int64(1)
	mustApply(t, g, newTestEnvelope(t, childID, "upstream:"+upstreamID.String()+":1", KindRunStarted, RunStartedPayload{BuildNumber: &childNumber}))

	got := readUpstreamColumns(t, db, childID)
	if !got.runID.Valid || got.runID.String != upstreamID.String() {
		t.Fatalf("upstream_run_id = %+v, want %s", got.runID, upstreamID)
	}
	if got.tenant.String != testTenant || got.job.String != testJobPath || got.number.Int64 != upNumber {
		t.Fatalf("upstream tenant/job/number = %q/%q/%d, want %q/%q/%d",
			got.tenant.String, got.job.String, got.number.Int64, testTenant, testJobPath, upNumber)
	}
}

// TestApplyRunStartedStampsUpstreamFromNativeCause asserts a Jenkins
// upstream cause resolves to the run of the same tenant that holds the
// named job and number.
func TestApplyRunStartedStampsUpstreamFromNativeCause(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	upstreamID := seedGrantedIntent(t, pool, "manual:native-1")
	upNumber := int64(7)
	mustApply(t, g, newTestEnvelope(t, upstreamID, "manual:native-1", KindRunStarted, RunStartedPayload{BuildNumber: &upNumber}))

	childID := seedGrantedIntent(t, pool, "manual:native-2")
	childNumber := int64(1)
	jobPath := testJobPath
	mustApply(t, g, newTestEnvelope(t, childID, "manual:native-2", KindRunStarted, RunStartedPayload{
		BuildNumber:         &childNumber,
		UpstreamJobPath:     &jobPath,
		UpstreamBuildNumber: &upNumber,
	}))

	got := readUpstreamColumns(t, db, childID)
	if !got.runID.Valid || got.runID.String != upstreamID.String() {
		t.Fatalf("upstream_run_id = %+v, want %s", got.runID, upstreamID)
	}
	if got.tenant.String != testTenant || got.job.String != testJobPath || got.number.Int64 != upNumber {
		t.Fatalf("upstream tenant/job/number = %q/%q/%d", got.tenant.String, got.job.String, got.number.Int64)
	}
}

// TestApplyRunStartedNativeCauseNoRecordKeepsJobAndNumber asserts a native
// cause with no matching run keeps the tenant, job, and number, and leaves
// the run id NULL.
func TestApplyRunStartedNativeCauseNoRecordKeepsJobAndNumber(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	childID := seedGrantedIntent(t, pool, "manual:native-missing")
	childNumber := int64(1)
	jobPath := testJobPath
	missing := int64(99)
	mustApply(t, g, newTestEnvelope(t, childID, "manual:native-missing", KindRunStarted, RunStartedPayload{
		BuildNumber:         &childNumber,
		UpstreamJobPath:     &jobPath,
		UpstreamBuildNumber: &missing,
	}))

	got := readUpstreamColumns(t, db, childID)
	if got.runID.Valid {
		t.Fatalf("upstream_run_id = %q, want NULL when no run matches", got.runID.String)
	}
	if got.tenant.String != testTenant || got.job.String != testJobPath || got.number.Int64 != missing {
		t.Fatalf("upstream tenant/job/number = %q/%q/%d", got.tenant.String, got.job.String, got.number.Int64)
	}
}

// TestApplyRunStartedIgnoresNativeCauseOfAnotherTenant asserts a native
// cause never links to a run of another tenant, even when the job path and
// number match that run.
func TestApplyRunStartedIgnoresNativeCauseOfAnotherTenant(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	foreignID := uuid.Must(uuid.NewV7())
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, build_number, status)
		VALUES ($1, $2, $3, $4, $5, 'cell-a', 'zeta', 'zeta/app', 9, 'COMPLETED')
	`, foreignID, time.Now().UTC(), "manual:foreign", testCluster, testNamespace); err != nil {
		t.Fatalf("seed foreign run: %v", err)
	}

	childID := seedGrantedIntent(t, pool, "manual:foreign-child")
	childNumber := int64(1)
	foreignJob := "zeta/app"
	foreignNumber := int64(9)
	mustApply(t, g, newTestEnvelope(t, childID, "manual:foreign-child", KindRunStarted, RunStartedPayload{
		BuildNumber:         &childNumber,
		UpstreamJobPath:     &foreignJob,
		UpstreamBuildNumber: &foreignNumber,
	}))

	got := readUpstreamColumns(t, db, childID)
	if got.runID.Valid {
		t.Fatalf("upstream_run_id = %q, want NULL for a run of another tenant", got.runID.String)
	}
}

// TestApplyRunStartedLeavesUpstreamNull asserts a run with no upstream key
// and no native cause keeps all four columns NULL.
func TestApplyRunStartedLeavesUpstreamNull(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	runID := seedGrantedIntent(t, pool, "manual:plain-1")
	number := int64(1)
	mustApply(t, g, newTestEnvelope(t, runID, "manual:plain-1", KindRunStarted, RunStartedPayload{BuildNumber: &number}))

	got := readUpstreamColumns(t, db, runID)
	if got.runID.Valid || got.tenant.Valid || got.job.Valid || got.number.Valid {
		t.Fatalf("upstream columns = %+v, want all NULL", got)
	}
}

// TestRedeliveredStartKeepsUpstream asserts a later run.started with no
// upstream data keeps the stored upstream columns.
func TestRedeliveredStartKeepsUpstream(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	upstreamID := seedGrantedIntent(t, pool, "manual:redeliver-up")
	upNumber := int64(3)
	mustApply(t, g, newTestEnvelope(t, upstreamID, "manual:redeliver-up", KindRunStarted, RunStartedPayload{BuildNumber: &upNumber}))

	key := "upstream:" + upstreamID.String() + ":1"
	seedUpstreamOrigin(t, pool, key)
	childID := seedGrantedIntent(t, pool, key)
	childNumber := int64(1)
	mustApply(t, g, newTestEnvelope(t, childID, key, KindRunStarted, RunStartedPayload{BuildNumber: &childNumber}))
	mustApply(t, g, newTestEnvelope(t, childID, key, KindRunStarted, RunStartedPayload{BuildNumber: &childNumber}))

	got := readUpstreamColumns(t, db, childID)
	if !got.runID.Valid || got.runID.String != upstreamID.String() {
		t.Fatalf("upstream_run_id after redelivery = %+v, want %s", got.runID, upstreamID)
	}
}

// TestApplyRunStartedRetryInheritsUpstream asserts a retry run copies the
// four upstream columns of its source run.
func TestApplyRunStartedRetryInheritsUpstream(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	upstreamID := seedGrantedIntent(t, pool, "manual:retry-up")
	upNumber := int64(4)
	mustApply(t, g, newTestEnvelope(t, upstreamID, "manual:retry-up", KindRunStarted, RunStartedPayload{BuildNumber: &upNumber}))

	sourceKey := "upstream:" + upstreamID.String() + ":1"
	seedUpstreamOrigin(t, pool, sourceKey)
	sourceID := seedGrantedIntent(t, pool, sourceKey)
	sourceNumber := int64(1)
	mustApply(t, g, newTestEnvelope(t, sourceID, sourceKey, KindRunStarted, RunStartedPayload{BuildNumber: &sourceNumber}))

	retryKey := "retry:" + sourceID.String()
	retryID := seedGrantedIntent(t, pool, retryKey)
	retryNumber := int64(2)
	mustApply(t, g, newTestEnvelope(t, retryID, retryKey, KindRunStarted, RunStartedPayload{BuildNumber: &retryNumber}))

	got := readUpstreamColumns(t, db, retryID)
	if !got.runID.Valid || got.runID.String != upstreamID.String() || got.number.Int64 != upNumber {
		t.Fatalf("retry upstream = %+v, want run %s number %d", got, upstreamID, upNumber)
	}
}
