package recordstore

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedCrossTenantRun claims a fresh intent for testCluster/tenant/jobPath
// through an allow-all ledger, so a cross-tenant status or artifact test
// can name a run_intents row with coordinates the test controls.
func seedCrossTenantRun(t *testing.T, pool *pgxpool.Pool, tenant, jobPath string) string {
	t.Helper()
	ledger := NewLedger(pool, nil)
	req := ClaimRequest{
		IntentKey:   "manual:" + tenant + "-" + jobPath,
		RunID:       newTestUUIDv7String(),
		Cluster:     testCluster,
		Namespace:   "ns-" + tenant,
		Cell:        "cell-" + tenant,
		Tenant:      tenant,
		JobPath:     jobPath,
		Actor:       "tester",
		RequestedAt: time.Now().UTC(),
	}
	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionGranted {
		t.Fatalf("want granted claim, got %+v", resp)
	}
	return resp.RunID
}

// seedCrossTenantSuccessRun inserts one COMPLETED, SUCCESS varroa.runs
// row directly at qualifiedJobPath, the form the stored column holds,
// so a last_successful test can find it.
func seedCrossTenantSuccessRun(t *testing.T, db *sql.DB, cluster, tenant, qualifiedJobPath string) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status, result)
		VALUES ($1, now(), $2, $3, $4, $5, $6, $7, 'COMPLETED', 'SUCCESS')
	`, runID, "manual:"+runID.String(), cluster, "ns-"+tenant, "cell-"+tenant, tenant, qualifiedJobPath); err != nil {
		t.Fatalf("seed cross-tenant success run: %v", err)
	}
	return runID
}

// statusTestDeps opens a fresh schema and a pgxpool.Pool for a status or
// artifact test, with no bus connection: neither kind publishes an
// action.
func statusTestDeps(t *testing.T) (CrossTenantDeps, *sql.DB) {
	t.Helper()
	pool, db := setupLedgerPool(t)
	return CrossTenantDeps{
		Pool:      pool,
		Evaluator: NewGrantEvaluator(pool),
		Reader:    NewReader(pool),
	}, db
}

// TestCrossTenantStatus_NoGrantAnswersCrossTenant covers task 3.4's
// first acceptance: a caller with no grant gets CROSS_TENANT.
func TestCrossTenantStatus_NoGrantAnswersCrossTenant(t *testing.T) {
	t.Parallel()

	deps, db := statusTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)
	seedTenantPlacement(t, db, "beta", "ns-beta", "cell-beta", 0, "pod-b", "pod-b", nil)
	runID := seedCrossTenantRun(t, deps.Pool, "beta", "beta/release")

	resp, err := handleCrossTenantStatus(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		RunID:        runID,
	})
	if err != nil {
		t.Fatalf("handleCrossTenantStatus: %v", err)
	}
	if resp.ReasonCode != ReasonCrossTenant {
		t.Fatalf("reason_code: got %q, want %q (message %q)", resp.ReasonCode, ReasonCrossTenant, resp.Message)
	}
}

// TestCrossTenantStatus_OwnTenantNeedsNoGrant covers task 3.4's second
// acceptance: a caller inside its own tenant needs no grant.
func TestCrossTenantStatus_OwnTenantNeedsNoGrant(t *testing.T) {
	t.Parallel()

	deps, db := statusTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)
	runID := seedCrossTenantRun(t, deps.Pool, "alpha", "alpha/release")

	resp, err := handleCrossTenantStatus(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		RunID:        runID,
	})
	if err != nil {
		t.Fatalf("handleCrossTenantStatus: %v", err)
	}
	if resp.ReasonCode != "" {
		t.Fatalf("reason_code: got %q, want none (message %q)", resp.ReasonCode, resp.Message)
	}
	if resp.RunID != runID {
		t.Fatalf("run_id: got %q, want %q", resp.RunID, runID)
	}
	if resp.State != CrossTenantStateQueued {
		t.Fatalf("state: got %q, want %q", resp.State, CrossTenantStateQueued)
	}
}

// TestCrossTenantStatus_UnknownIntentKeyAnswersPending covers task 3.4's
// third acceptance: an intent_key with no row answers pending.
func TestCrossTenantStatus_UnknownIntentKeyAnswersPending(t *testing.T) {
	t.Parallel()

	deps, _ := statusTestDeps(t)

	resp, err := handleCrossTenantStatus(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		CallerTenant: "alpha",
		IntentKey:    "upstream:no-such-run:s1",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantStatus: %v", err)
	}
	if resp.State != CrossTenantStatePending {
		t.Fatalf("state: got %q, want %q", resp.State, CrossTenantStatePending)
	}
	if resp.RunID != "" {
		t.Fatalf("run_id: got %q, want empty", resp.RunID)
	}
}

// TestCrossTenantStatus_UnknownRunIDAnswersNotFound asserts a run_id with
// no record answers NOT_FOUND with an error that names the identifier.
func TestCrossTenantStatus_UnknownRunIDAnswersNotFound(t *testing.T) {
	t.Parallel()

	deps, _ := statusTestDeps(t)
	runID := newTestUUIDv7String()

	resp, err := handleCrossTenantStatus(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		CallerTenant: "alpha",
		RunID:        runID,
	})
	if err != nil {
		t.Fatalf("handleCrossTenantStatus: %v", err)
	}
	if resp.Code != CodeNotFound {
		t.Fatalf("code: got %q, want %q", resp.Code, CodeNotFound)
	}
	if !strings.Contains(resp.Error, runID) {
		t.Fatalf("error: got %q, want it to name %s", resp.Error, runID)
	}
}

// TestCrossTenantStatus_BothIdentifiersRefused covers task 3.4's fourth
// acceptance: a body carrying both identifiers is refused.
func TestCrossTenantStatus_BothIdentifiersRefused(t *testing.T) {
	t.Parallel()

	deps, _ := statusTestDeps(t)

	resp, err := handleCrossTenantStatus(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		CallerTenant: "alpha",
		RunID:        newTestUUIDv7String(),
		IntentKey:    "upstream:some-run:s1",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantStatus: %v", err)
	}
	if resp.Code != CodeInternal {
		t.Fatalf("code: got %q, want %q", resp.Code, CodeInternal)
	}
	if resp.Error == "" {
		t.Fatal("error: got empty, want a message naming the conflict")
	}
}

// TestCrossTenantStatus_NeitherIdentifierRefused asserts a body naming
// neither run_id nor intent_key is refused the same way.
func TestCrossTenantStatus_NeitherIdentifierRefused(t *testing.T) {
	t.Parallel()

	deps, _ := statusTestDeps(t)

	resp, err := handleCrossTenantStatus(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		CallerTenant: "alpha",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantStatus: %v", err)
	}
	if resp.Code != CodeInternal {
		t.Fatalf("code: got %q, want %q", resp.Code, CodeInternal)
	}
}

// TestCrossTenantLastSuccessful_NoSuccessfulRunAnswersEmpty covers task
// 3.5's first acceptance: a job with no successful run answers an empty
// result.
func TestCrossTenantLastSuccessful_NoSuccessfulRunAnswersEmpty(t *testing.T) {
	t.Parallel()

	deps, db := statusTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)

	resp, err := handleCrossTenantLastSuccessful(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		TargetTenant: "alpha",
		JobPath:      "release",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLastSuccessful: %v", err)
	}
	if resp.ReasonCode != "" {
		t.Fatalf("reason_code: got %q, want none", resp.ReasonCode)
	}
	if resp.RunID != "" {
		t.Fatalf("run_id: got %q, want empty", resp.RunID)
	}
}

// TestCrossTenantLastSuccessful_UngrantedCallerAnswersCrossTenant covers
// task 3.5's second acceptance: an ungranted caller gets CROSS_TENANT.
func TestCrossTenantLastSuccessful_UngrantedCallerAnswersCrossTenant(t *testing.T) {
	t.Parallel()

	deps, db := statusTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)
	seedTenantPlacement(t, db, "beta", "ns-beta", "cell-beta", 0, "pod-b", "pod-b", nil)

	resp, err := handleCrossTenantLastSuccessful(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		TargetTenant: "beta",
		JobPath:      "release",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLastSuccessful: %v", err)
	}
	if resp.ReasonCode != ReasonCrossTenant {
		t.Fatalf("reason_code: got %q, want %q (message %q)", resp.ReasonCode, ReasonCrossTenant, resp.Message)
	}
}

// TestCrossTenantLastSuccessful_RelativeJobPathReachesExportedGrant
// covers the documented step call, varroa.lastSuccessful(tenant:
// 'platform', job: 'release'): the wire job_path arrives folder
// relative to target_tenant, and the recorder must qualify it before
// the grant check and before it reads the stored, qualified
// runs.job_path.
func TestCrossTenantLastSuccessful_RelativeJobPathReachesExportedGrant(t *testing.T) {
	t.Parallel()

	deps, db := statusTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)
	seedGrantPlacement(t, db, testCluster, "platform", "ns-platform", "cell-platform", 0,
		`{"exports":[{"job":"release","callers":["alpha"]}]}`)
	runID := seedCrossTenantSuccessRun(t, db, testCluster, "platform", "platform/release")

	resp, err := handleCrossTenantLastSuccessful(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		TargetTenant: "platform",
		JobPath:      "release",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLastSuccessful: %v", err)
	}
	if resp.ReasonCode != "" {
		t.Fatalf("reason_code: got %q, want none (the relative job path must reach the grant)", resp.ReasonCode)
	}
	if resp.RunID != runID.String() {
		t.Fatalf("run_id: got %q, want the seeded run %q", resp.RunID, runID.String())
	}
}
