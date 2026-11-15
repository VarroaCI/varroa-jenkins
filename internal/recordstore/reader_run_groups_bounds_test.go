package recordstore

import (
	"context"
	"testing"
	"time"
)

// TestRunGroupsRunAtNowIsNotHeld asserts a failure created exactly at the window
// end is not the held run. The earlier failure in the window is then the held
// run, because the held run is the newest counted run before the window end.
func TestRunGroupsRunAtNowIsNotHeld(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	failure := "FAILURE"
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-2*time.Hour), "COMPLETED", &failure)
	seedAcrossRun(t, db, "pay", "pay/build", now, "COMPLETED", &failure)

	page, err := r.RunGroups(context.Background(), RunGroupsQuery{
		Scopes: failingScope, Since: now.Add(-6 * time.Hour), Now: now,
	})
	if err != nil {
		t.Fatalf("RunGroups: %v", err)
	}
	if page.Held != 1 || len(page.Groups) != 0 {
		t.Fatalf("held = %d, groups = %+v; want the in-window failure held and no group", page.Held, page.Groups)
	}
}

// TestRunGroupsSinceIsInclusiveAndNowIsExclusive asserts a run created exactly at
// since is counted, and a run created exactly at now is not.
func TestRunGroupsSinceIsInclusiveAndNowIsExclusive(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	since := now.Add(-6 * time.Hour)
	success := "SUCCESS"
	seedAcrossRun(t, db, "pay", "pay/build", since, "COMPLETED", &success)
	seedAcrossRun(t, db, "pay", "pay/build", now, "COMPLETED", &success)

	page, err := r.RunGroups(context.Background(), RunGroupsQuery{Scopes: failingScope, Since: since, Now: now})
	if err != nil {
		t.Fatalf("RunGroups: %v", err)
	}
	if len(page.Groups) != 1 || page.Groups[0].Runs != 1 {
		t.Fatalf("groups = %+v, want one group of the run at since only", page.Groups)
	}
}
