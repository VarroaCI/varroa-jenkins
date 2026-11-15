package recordstore

import (
	"context"
	"testing"
	"time"
)

// groupsWindow returns the since and now bounds that the run-groups tests use.
func groupsWindow(now time.Time) (since, end time.Time) {
	return now.Add(-6 * time.Hour), now
}

// TestRunGroupsCountsPerJob asserts the passed and failed counts of a job's
// finished runs, with the held run left out.
func TestRunGroupsCountsPerJob(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	success, unstable := "SUCCESS", "UNSTABLE"
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-5*time.Hour), "COMPLETED", &success)
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-4*time.Hour), "COMPLETED", &unstable)
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-3*time.Hour), "COMPLETED", &success)

	since, end := groupsWindow(now)
	page, err := r.RunGroups(context.Background(), RunGroupsQuery{Scopes: failingScope, Since: since, Now: end})
	if err != nil {
		t.Fatalf("RunGroups: %v", err)
	}
	if len(page.Groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(page.Groups))
	}
	g := page.Groups[0]
	if g.Runs != 3 || g.Passed != 2 || g.Failed != 0 || g.JobPath != "pay/build" {
		t.Fatalf("group = %+v, want 3 runs, 2 passed, 0 failed", g)
	}
	if page.Held != 0 {
		t.Fatalf("held = %d, want 0", page.Held)
	}
}

// TestRunGroupsLeavesOutTheHeldFailure asserts the newest counted failure of a
// job is held: it is not in the group, and Held counts it.
func TestRunGroupsLeavesOutTheHeldFailure(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	success, failure := "SUCCESS", "FAILURE"
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-4*time.Hour), "COMPLETED", &success)
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-3*time.Hour), "COMPLETED", &success)
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-time.Hour), "COMPLETED", &failure)

	since, end := groupsWindow(now)
	page, err := r.RunGroups(context.Background(), RunGroupsQuery{Scopes: failingScope, Since: since, Now: end})
	if err != nil {
		t.Fatalf("RunGroups: %v", err)
	}
	if page.Held != 1 {
		t.Fatalf("held = %d, want 1", page.Held)
	}
	if len(page.Groups) != 1 || page.Groups[0].Runs != 2 || page.Groups[0].Failed != 0 {
		t.Fatalf("groups = %+v, want one group of two passes", page.Groups)
	}
	if page.Groups[0].Newest.Result == nil || *page.Groups[0].Newest.Result != "SUCCESS" {
		t.Fatalf("newest = %+v, want the newest pass", page.Groups[0].Newest)
	}
}

// TestRunGroupsDropsAGroupWithOnlyTheHeldRun asserts a job whose only finished
// run is held leaves no group behind.
func TestRunGroupsDropsAGroupWithOnlyTheHeldRun(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	failure := "FAILURE"
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-time.Hour), "COMPLETED", &failure)

	since, end := groupsWindow(now)
	page, err := r.RunGroups(context.Background(), RunGroupsQuery{Scopes: failingScope, Since: since, Now: end})
	if err != nil {
		t.Fatalf("RunGroups: %v", err)
	}
	if len(page.Groups) != 0 || page.Held != 1 {
		t.Fatalf("groups = %d, held = %d; want no group and one held run", len(page.Groups), page.Held)
	}
}

// TestRunGroupsKeepsAnOlderFailure asserts a failure that is not the newest
// counted run stays in its group.
func TestRunGroupsKeepsAnOlderFailure(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	success, failure := "SUCCESS", "FAILURE"
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-4*time.Hour), "COMPLETED", &failure)
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-3*time.Hour), "COMPLETED", &success)

	since, end := groupsWindow(now)
	page, err := r.RunGroups(context.Background(), RunGroupsQuery{Scopes: failingScope, Since: since, Now: end})
	if err != nil {
		t.Fatalf("RunGroups: %v", err)
	}
	if page.Held != 0 || len(page.Groups) != 1 || page.Groups[0].Failed != 1 || page.Groups[0].Runs != 2 {
		t.Fatalf("page = %+v, want the older failure kept", page)
	}
}

// TestRunGroupsSinceAndNowBound asserts the window is since <= created_at < now.
func TestRunGroupsSinceAndNowBound(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	success := "SUCCESS"
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-5*time.Hour), "COMPLETED", &success)
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-time.Hour), "COMPLETED", &success)

	page, err := r.RunGroups(context.Background(), RunGroupsQuery{
		Scopes: failingScope, Since: now.Add(-4 * time.Hour), Now: now.Add(-30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("RunGroups: %v", err)
	}
	// The run five hours back lies before since, so only the run one hour back counts.
	if len(page.Groups) != 1 || page.Groups[0].Runs != 1 {
		t.Fatalf("groups = %+v, want one group of one run inside the window", page.Groups)
	}
}

// TestRunGroupsJobPatternsApply asserts a scope's job patterns narrow the groups.
func TestRunGroupsJobPatternsApply(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	success := "SUCCESS"
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-2*time.Hour), "COMPLETED", &success)
	seedAcrossRun(t, db, "pay", "pay/deploy", now.Add(-2*time.Hour), "COMPLETED", &success)

	since, end := groupsWindow(now)
	scopes := []RunScope{{Cluster: testCluster, Tenant: "pay", JobPatterns: []string{`pay/deploy`}}}
	page, err := r.RunGroups(context.Background(), RunGroupsQuery{Scopes: scopes, Since: since, Now: end})
	if err != nil {
		t.Fatalf("RunGroups: %v", err)
	}
	if len(page.Groups) != 1 || page.Groups[0].JobPath != "pay/deploy" {
		t.Fatalf("groups = %+v, want only pay/deploy", page.Groups)
	}
}

// TestRunGroupsNewestRunAndOrder asserts groups come newest run first, and that
// each group's Newest is its newest run.
func TestRunGroupsNewestRunAndOrder(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	success := "SUCCESS"
	older := seedAcrossRun(t, db, "pay", "pay/a", now.Add(-4*time.Hour), "COMPLETED", &success)
	seedAcrossRun(t, db, "pay", "pay/b", now.Add(-3*time.Hour), "COMPLETED", &success)
	newest := seedAcrossRun(t, db, "pay", "pay/a", now.Add(-time.Hour), "COMPLETED", &success)

	since, end := groupsWindow(now)
	page, err := r.RunGroups(context.Background(), RunGroupsQuery{Scopes: failingScope, Since: since, Now: end})
	if err != nil {
		t.Fatalf("RunGroups: %v", err)
	}
	if len(page.Groups) != 2 || page.Groups[0].JobPath != "pay/a" || page.Groups[1].JobPath != "pay/b" {
		t.Fatalf("groups = %+v, want pay/a then pay/b", page.Groups)
	}
	if page.Groups[0].Newest.RunID != newest.String() {
		t.Fatalf("newest of pay/a = %s, want %s (not %s)", page.Groups[0].Newest.RunID, newest, older)
	}
}

// TestRunGroupsLimitSetsMore asserts a page cut at the limit sets More.
func TestRunGroupsLimitSetsMore(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	success := "SUCCESS"
	seedAcrossRun(t, db, "pay", "pay/a", now.Add(-3*time.Hour), "COMPLETED", &success)
	seedAcrossRun(t, db, "pay", "pay/b", now.Add(-2*time.Hour), "COMPLETED", &success)

	since, end := groupsWindow(now)
	page, err := r.RunGroups(context.Background(), RunGroupsQuery{Scopes: failingScope, Since: since, Now: end, Limit: 1})
	if err != nil {
		t.Fatalf("RunGroups: %v", err)
	}
	if len(page.Groups) != 1 || !page.More {
		t.Fatalf("page = %+v, want one group and More", page)
	}
}

// TestRunGroupsEmptyScopes asserts an empty scope list answers an empty page.
func TestRunGroupsEmptyScopes(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	r := NewReader(pool)
	page, err := r.RunGroups(context.Background(), RunGroupsQuery{})
	if err != nil || len(page.Groups) != 0 || page.Held != 0 || page.More {
		t.Fatalf("RunGroups(no scope) = %+v, %v; want an empty page", page, err)
	}
}

// TestRunGroupsSkipsLiveAndNoBuildRows asserts a run that is still live, or that
// records no build, is not counted. A job with only such runs has no group.
func TestRunGroupsSkipsLiveAndNoBuildRows(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	success, triggerFailed := "SUCCESS", "TRIGGER_FAILED"
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-3*time.Hour), "RUNNING", nil)
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-2*time.Hour), "COMPLETED", &triggerFailed)
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-time.Hour), "COMPLETED", &success)
	seedAcrossRun(t, db, "pay", "pay/idle", now.Add(-time.Hour), "COMPLETED", &triggerFailed)

	since, end := groupsWindow(now)
	page, err := r.RunGroups(context.Background(), RunGroupsQuery{Scopes: failingScope, Since: since, Now: end})
	if err != nil {
		t.Fatalf("RunGroups: %v", err)
	}
	if len(page.Groups) != 1 || page.Groups[0].JobPath != "pay/build" {
		t.Fatalf("groups = %+v, want only pay/build", page.Groups)
	}
	if g := page.Groups[0]; g.Runs != 1 || g.Passed != 1 {
		t.Fatalf("group = %+v, want one passed run", g)
	}
}
