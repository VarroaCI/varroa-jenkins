package recordstore

import (
	"context"
	"testing"
	"time"
)

// backstopTestGrace is the grace window every candidate test shares,
// unless a test overrides it.
const backstopTestGrace = 5 * time.Minute

// TestIsBackstopCandidateRequiresPastGrace asserts an occurrence inside
// the grace window is not a candidate, and one past it is.
func TestIsBackstopCandidateRequiresPastGrace(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)
	now := time.Now().UTC()

	inside := now.Add(-2 * time.Minute)
	ok, err := isBackstopCandidate(context.Background(), reader, pool, testCluster, "acme/inside", inside, now, backstopTestGrace)
	if err != nil {
		t.Fatalf("isBackstopCandidate: %v", err)
	}
	if ok {
		t.Fatal("want no candidate for an occurrence inside the grace window")
	}

	past := now.Add(-10 * time.Minute)
	ok, err = isBackstopCandidate(context.Background(), reader, pool, testCluster, "acme/past", past, now, backstopTestGrace)
	if err != nil {
		t.Fatalf("isBackstopCandidate: %v", err)
	}
	if !ok {
		t.Fatal("want a candidate for an occurrence past the grace window")
	}
}

// TestIsBackstopCandidateRejectsOlderThan24Hours asserts a stale
// occurrence produces no candidate.
func TestIsBackstopCandidateRejectsOlderThan24Hours(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)
	now := time.Now().UTC()

	stale := now.Add(-30 * time.Hour)
	ok, err := isBackstopCandidate(context.Background(), reader, pool, testCluster, "acme/stale", stale, now, backstopTestGrace)
	if err != nil {
		t.Fatalf("isBackstopCandidate: %v", err)
	}
	if ok {
		t.Fatal("want no candidate for an occurrence older than 24 hours")
	}
}

// TestIsBackstopCandidateRejectsWhenIntentExists asserts a job path whose
// occurrence already holds a cron: intent is not a candidate: the cell
// fired on its own.
func TestIsBackstopCandidateRejectsWhenIntentExists(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)
	now := time.Now().UTC()
	occ := now.Add(-10 * time.Minute)

	ledger := NewLedger(pool, nil)
	key := cronIntentKey("acme/self-fired", occ)
	req := ClaimRequest{
		IntentKey: key, RunID: newTestUUIDv7String(), Cluster: testCluster, Namespace: testNamespace,
		Cell: "cell-a", CellInstance: "pod-a", Tenant: testTenant, JobPath: "acme/self-fired",
	}
	if resp := ledger.Claim(context.Background(), req); resp.Decision != DecisionGranted {
		t.Fatalf("seed intent: want granted, got %+v", resp)
	}

	ok, err := isBackstopCandidate(context.Background(), reader, pool, testCluster, "acme/self-fired", occ, now, backstopTestGrace)
	if err != nil {
		t.Fatalf("isBackstopCandidate: %v", err)
	}
	if ok {
		t.Fatal("want no candidate when the ledger already holds this occurrence's intent")
	}
}

// TestIsBackstopCandidateRejectsRecentActivityRow asserts an occurrence
// whose build.scheduled.backstop row is newer than the grace window is
// not a candidate: the round already fired for it recently.
func TestIsBackstopCandidateRejectsRecentActivityRow(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)
	now := time.Now().UTC()
	occ := now.Add(-10 * time.Minute)

	c := backstopFireCandidate{
		cluster: testCluster, tenant: testTenant, namespace: testNamespace, cell: "cell-a",
		jobPath: "acme/already-fired", occurrence: occ, grace: backstopTestGrace,
	}
	if err := writeBackstopActivity(context.Background(), pool, c, now.Add(-1*time.Minute)); err != nil {
		t.Fatalf("write backstop activity: %v", err)
	}

	ok, err := isBackstopCandidate(context.Background(), reader, pool, testCluster, "acme/already-fired", occ, now, backstopTestGrace)
	if err != nil {
		t.Fatalf("isBackstopCandidate: %v", err)
	}
	if ok {
		t.Fatal("want no candidate when a recent backstop activity row already covers this occurrence")
	}
}

// TestSelectCandidateOccurrencesEmptyListProducesNothing asserts an empty
// exported-occurrence list produces no candidate.
func TestSelectCandidateOccurrencesEmptyListProducesNothing(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)
	now := time.Now().UTC()

	got, err := selectCandidateOccurrences(context.Background(), reader, pool, testCluster, "acme/no-occurrences", nil, now, backstopTestGrace)
	if err != nil {
		t.Fatalf("selectCandidateOccurrences: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d candidates, want 0 for an empty occurrence list", len(got))
	}
}

// TestIsBackstopCandidateWeekdayJobNoCandidateOnSaturday asserts a
// weekday-only job's next exported occurrence (the coming Monday) is not
// a candidate on a Saturday: it is in the future, not past the grace
// window. Jenkins's own CronTab computes next_fire_times, so the round
// never sees a Saturday occurrence for such a job in the first place.
func TestIsBackstopCandidateWeekdayJobNoCandidateOnSaturday(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)

	// A Saturday, with the job's only exported occurrence the coming
	// Monday morning.
	saturday := time.Date(2026, time.September, 12, 10, 0, 0, 0, time.UTC)
	if saturday.Weekday() != time.Saturday {
		t.Fatalf("fixture date %s is not a Saturday", saturday)
	}
	monday := time.Date(2026, time.September, 14, 8, 0, 0, 0, time.UTC)

	ok, err := isBackstopCandidate(context.Background(), reader, pool, testCluster, "acme/weekday-only", monday, saturday, backstopTestGrace)
	if err != nil {
		t.Fatalf("isBackstopCandidate: %v", err)
	}
	if ok {
		t.Fatal("want no candidate on a Saturday for a weekday-only job's future Monday occurrence")
	}
}

// TestIsBackstopCandidateFiresAgainAfterSuppressionWindowPasses asserts
// the round admits the occurrence again once its previous
// build.scheduled.backstop row ages past the grace window: an action is
// at-most-once, so a permanent skip would strand a lost fire.
func TestIsBackstopCandidateFiresAgainAfterSuppressionWindowPasses(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)
	now := time.Now().UTC()
	occ := now.Add(-20 * time.Minute)

	c := backstopFireCandidate{
		cluster: testCluster, tenant: testTenant, namespace: testNamespace, cell: "cell-a",
		jobPath: "acme/resuppress", occurrence: occ, grace: backstopTestGrace,
	}
	// The prior fire's row is older than the grace window: the
	// suppression no longer applies.
	if err := writeBackstopActivity(context.Background(), pool, c, now.Add(-10*time.Minute)); err != nil {
		t.Fatalf("write backstop activity: %v", err)
	}

	ok, err := isBackstopCandidate(context.Background(), reader, pool, testCluster, "acme/resuppress", occ, now, backstopTestGrace)
	if err != nil {
		t.Fatalf("isBackstopCandidate: %v", err)
	}
	if !ok {
		t.Fatal("want the occurrence to become a candidate again once the suppression window passes")
	}
}

// TestIsBackstopCandidateIgnoresOtherClusterActivityRow asserts a
// build.scheduled.backstop row logged for the same job path and
// occurrence minute, but a different cluster, does not suppress the
// candidate: the suppression match scopes on cluster, not job path
// alone, because two clusters can carry the same job path.
func TestIsBackstopCandidateIgnoresOtherClusterActivityRow(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)
	now := time.Now().UTC()
	occ := now.Add(-10 * time.Minute)

	otherCluster := "hive-b"
	c := backstopFireCandidate{
		cluster: otherCluster, tenant: testTenant, namespace: testNamespace, cell: "cell-a",
		jobPath: "acme/cross-cluster", occurrence: occ, grace: backstopTestGrace,
	}
	if err := writeBackstopActivity(context.Background(), pool, c, now.Add(-1*time.Minute)); err != nil {
		t.Fatalf("write backstop activity: %v", err)
	}

	ok, err := isBackstopCandidate(context.Background(), reader, pool, testCluster, "acme/cross-cluster", occ, now, backstopTestGrace)
	if err != nil {
		t.Fatalf("isBackstopCandidate: %v", err)
	}
	if !ok {
		t.Fatal("want a candidate: the recent activity row belongs to a different cluster")
	}
}
