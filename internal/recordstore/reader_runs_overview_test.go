package recordstore

import (
	"context"
	"testing"
	"time"
)

func TestRunsOverviewCountsWindowAndPrior(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	success, failure, interrupted, triggerFailed := "SUCCESS", "FAILURE", "INTERRUPTED", "TRIGGER_FAILED"
	type window struct {
		result   string
		created  time.Time
		duration int64
		waitMs   int64
	}
	inWindow := []window{
		{success, now.Add(-1 * time.Hour), 1000, 100},
		{failure, now.Add(-2 * time.Hour), 2000, 300},
		{interrupted, now.Add(-3 * time.Hour), 4000, 500},
	}
	for _, w := range inWindow {
		result := w.result
		queued := w.created
		started := w.created.Add(time.Duration(w.waitMs) * time.Millisecond)
		dur := w.duration
		seedAcrossRun(t, db, "pay", "pay/build", w.created, "COMPLETED", &result,
			func(s *acrossSeed) {
				s.durationMs = &dur
				s.queuedAt = &queued
				s.startedAt = &started
			})
	}
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-4*time.Hour), "COMPLETED", &triggerFailed)
	prior := []time.Time{now.Add(-30 * time.Hour), now.Add(-31 * time.Hour)}
	for _, created := range prior {
		seedAcrossRun(t, db, "pay", "pay/build", created, "COMPLETED", &success)
	}
	seedAcrossRun(t, db, "pay", "pay/build", now.Add(-72*time.Hour), "COMPLETED", &success)

	out, err := r.RunsOverview(context.Background(), RunsOverviewQuery{
		Scopes: []RunScope{{Cluster: "core", Tenant: "pay"}},
		Now:    now,
	})
	if err != nil {
		t.Fatalf("RunsOverview: %v", err)
	}
	tot := out.Totals
	if tot.Runs24h != 3 || tot.RunsPrior24h != 2 {
		t.Errorf("runs 24h = %d, prior = %d; want 3 and 2", tot.Runs24h, tot.RunsPrior24h)
	}
	if tot.Finished != 3 || tot.Passed != 1 || tot.Failed != 1 || tot.Interrupted != 1 {
		t.Errorf("finished/passed/failed/interrupted = %d/%d/%d/%d; want 3/1/1/1",
			tot.Finished, tot.Passed, tot.Failed, tot.Interrupted)
	}
	if tot.MedianDurationMs == nil || *tot.MedianDurationMs != 2000 {
		t.Errorf("median duration = %v; want 2000", tot.MedianDurationMs)
	}
	if tot.P90DurationMs == nil || *tot.P90DurationMs != 3600 {
		t.Errorf("p90 duration = %v; want 3600", tot.P90DurationMs)
	}
	if tot.MedianQueueWaitMs == nil || *tot.MedianQueueWaitMs != 300 {
		t.Errorf("median queue wait = %v; want 300", tot.MedianQueueWaitMs)
	}
}

func TestRunsOverviewLiveCountsHaveNoTimeBound(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	old := now.Add(-72 * time.Hour)
	seedAcrossRun(t, db, "pay", "pay/old-running", old, "RUNNING", nil)
	seedAcrossRun(t, db, "pay", "pay/old-waiting", old, "RUNNING", nil,
		func(s *acrossSeed) { s.inputs = `[{"id":"a"}]` })
	seedAcrossRun(t, db, "pay", "pay/queued", now.Add(-time.Hour), "QUEUED", nil)

	out, err := r.RunsOverview(context.Background(), RunsOverviewQuery{
		Scopes: []RunScope{{Cluster: "core", Tenant: "pay"}},
		Now:    now,
	})
	if err != nil {
		t.Fatalf("RunsOverview: %v", err)
	}
	if out.Totals.Running != 2 || out.Totals.Queued != 1 {
		t.Errorf("running/queued = %d/%d; want 2/1", out.Totals.Running, out.Totals.Queued)
	}
	scope := out.Scopes[0]
	if scope.Running != 2 || scope.WaitingInput != 1 {
		t.Errorf("scope running/waiting = %d/%d; want 2/1", scope.Running, scope.WaitingInput)
	}
}

func TestRunsOverviewJobPatternsApply(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	success := "SUCCESS"
	seedAcrossRun(t, db, "pay", "pay/release/a", now.Add(-time.Hour), "COMPLETED", &success)
	seedAcrossRun(t, db, "pay", "pay/other", now.Add(-2*time.Hour), "COMPLETED", &success)

	out, err := r.RunsOverview(context.Background(), RunsOverviewQuery{
		Scopes: []RunScope{{Cluster: "core", Tenant: "pay", JobPatterns: []string{`^pay/(?:release/[^/]*)$`}}},
		Now:    now,
	})
	if err != nil {
		t.Fatalf("RunsOverview: %v", err)
	}
	if out.Totals.Runs24h != 1 || out.Scopes[0].Runs24h != 1 {
		t.Errorf("runs = %d/%d; want 1/1", out.Totals.Runs24h, out.Scopes[0].Runs24h)
	}
	if len(out.Scopes[0].Recent) != 1 || out.Scopes[0].Recent[0].RunID == "" {
		t.Fatalf("recent = %+v; want the one matching run", out.Scopes[0].Recent)
	}
}

func TestRunsOverviewRecentNewestFirstCapped(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC()
	success := "SUCCESS"
	for i := 0; i < 12; i++ {
		seedAcrossRun(t, db, "pay", "pay/job", now.Add(-time.Duration(i+1)*time.Minute), "COMPLETED", &success)
	}

	out, err := r.RunsOverview(context.Background(), RunsOverviewQuery{
		Scopes: []RunScope{{Cluster: "core", Tenant: "pay"}},
		Now:    now,
	})
	if err != nil {
		t.Fatalf("RunsOverview: %v", err)
	}
	recent := out.Scopes[0].Recent
	if len(recent) != RecentResultsSize {
		t.Fatalf("recent = %d entries; want %d", len(recent), RecentResultsSize)
	}
	for i := 1; i < len(recent); i++ {
		if !recent[i-1].CreatedAt.After(recent[i].CreatedAt) {
			t.Fatalf("recent[%d] is not newer than recent[%d]", i-1, i)
		}
	}
}

func TestRunsOverviewEmptyScopes(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	now := time.Now().UTC()
	out, err := NewReader(pool).RunsOverview(context.Background(), RunsOverviewQuery{Now: now})
	if err != nil {
		t.Fatalf("RunsOverview: %v", err)
	}
	if out.Totals.Runs24h != 0 || len(out.Scopes) != 0 {
		t.Fatalf("overview = %+v; want zero totals and no scopes", out)
	}
	if !out.WindowStart.Equal(now.Add(-24 * time.Hour)) {
		t.Fatalf("window start = %v; want now minus 24h", out.WindowStart)
	}
}
