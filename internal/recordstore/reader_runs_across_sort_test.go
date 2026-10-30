package recordstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

func int64Ptr(n int64) *int64 { return &n }

func TestListRunsAcrossSortDurationPagesWithoutRepeat(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	base := time.Now().UTC().Add(-time.Hour)
	success := "SUCCESS"
	durations := []*int64{int64Ptr(5), int64Ptr(1), nil, int64Ptr(3), int64Ptr(3), int64Ptr(9)}
	for i, d := range durations {
		seedAcrossRun(t, db, "pay", "pay/d"+string(rune('a'+i)), base.Add(time.Duration(i)*time.Minute),
			"COMPLETED", &success, func(s *acrossSeed) { s.durationMs = d })
	}

	scope := []RunScope{{Cluster: "core", Tenant: "pay"}}
	var got []string
	var durationsSeen []*int64
	cursor := ""
	seen := map[string]bool{}
	for pages := 0; pages < 10; pages++ {
		page, err := r.ListRunsAcross(context.Background(), RunsAcrossQuery{
			Scopes: scope, Sort: RunSortDuration, Limit: 2, Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		for _, run := range page.Runs {
			if seen[run.RunID] {
				t.Fatalf("run %s repeated", run.RunID)
			}
			seen[run.RunID] = true
			got = append(got, run.JobPath)
			durationsSeen = append(durationsSeen, run.DurationMs)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(got) != 6 {
		t.Fatalf("got %d runs; want 6", len(got))
	}
	// Durations descending: 9, 5, 3, 3, 1, nil. The two runs of duration 3
	// come in created_at descending order, so pd (index 4) precedes pc (index 3).
	want := []string{"pay/df", "pay/da", "pay/de", "pay/dd", "pay/db", "pay/dc"}
	if !sameStrings(got, want) {
		t.Fatalf("job paths = %v; want %v", got, want)
	}
	if durationsSeen[5] != nil {
		t.Fatalf("last duration = %v; want nil", *durationsSeen[5])
	}
}

func TestListRunsAcrossSortStatusRank(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	base := time.Now().UTC().Add(-time.Hour)
	result := func(s string) *string { return &s }
	type seedRow struct {
		job, status string
		result      *string
	}
	rows := []seedRow{
		{"pay/success", "COMPLETED", result("SUCCESS")},
		{"pay/unstable", "COMPLETED", result("UNSTABLE")},
		{"pay/failure", "COMPLETED", result("FAILURE")},
		{"pay/interrupted", "COMPLETED", result("INTERRUPTED")},
		{"pay/trigger", "COMPLETED", result("TRIGGER_FAILED")},
		{"pay/aborted", "COMPLETED", result("ABORTED")},
		{"pay/running", "RUNNING", nil},
		{"pay/queued", "QUEUED", nil},
	}
	for i, row := range rows {
		seedAcrossRun(t, db, "pay", row.job, base.Add(time.Duration(i)*time.Second), row.status, row.result)
	}

	page, err := r.ListRunsAcross(context.Background(), RunsAcrossQuery{
		Scopes: []RunScope{{Cluster: "core", Tenant: "pay"}}, Sort: RunSortStatus, Ascending: true,
	})
	if err != nil {
		t.Fatalf("ListRunsAcross: %v", err)
	}
	want := []string{"pay/failure", "pay/trigger", "pay/interrupted", "pay/unstable",
		"pay/running", "pay/queued", "pay/aborted", "pay/success"}
	if got := acrossJobPaths(page); !sameStrings(got, want) {
		t.Fatalf("job paths = %v; want %v", got, want)
	}
}

func TestListRunsAcrossSortActorAndTenantAscending(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	base := time.Now().UTC().Add(-time.Hour)
	success := "SUCCESS"
	seedAcrossRun(t, db, "pay", "pay/carol", base, "COMPLETED", &success,
		func(s *acrossSeed) { s.actor = strPtr("carol") })
	seedAcrossRun(t, db, "pay", "pay/nobody", base.Add(time.Minute), "COMPLETED", &success)
	seedAcrossRun(t, db, "pay", "pay/alice", base.Add(2*time.Minute), "COMPLETED", &success,
		func(s *acrossSeed) { s.actor = strPtr("alice") })
	scope := []RunScope{{Cluster: "core", Tenant: "pay"}}

	page, err := r.ListRunsAcross(context.Background(), RunsAcrossQuery{Scopes: scope, Sort: RunSortActor, Ascending: true})
	if err != nil {
		t.Fatalf("actor sort: %v", err)
	}
	if got, want := acrossJobPaths(page), []string{"pay/nobody", "pay/alice", "pay/carol"}; !sameStrings(got, want) {
		t.Fatalf("actor sort job paths = %v; want %v", got, want)
	}

	tenants := []RunScope{{Cluster: "core", Tenant: "pay"}, {Cluster: "core", Tenant: "ops"}}
	seedAcrossRun(t, db, "ops", "ops/deploy", base, "COMPLETED", &success)
	page, err = r.ListRunsAcross(context.Background(), RunsAcrossQuery{Scopes: tenants, Sort: RunSortTenant, Ascending: true})
	if err != nil {
		t.Fatalf("tenant sort: %v", err)
	}
	if page.Runs[0].Tenant != "ops" {
		t.Fatalf("first tenant = %q; want ops", page.Runs[0].Tenant)
	}
}

func TestListRunsAcrossCursorOfAnotherSortRefused(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	base := time.Now().UTC().Add(-time.Hour)
	success := "SUCCESS"
	for i := 0; i < 3; i++ {
		seedAcrossRun(t, db, "pay", "pay/job", base.Add(time.Duration(i)*time.Minute), "COMPLETED", &success,
			func(s *acrossSeed) { s.durationMs = int64Ptr(int64(i)) })
	}
	scope := []RunScope{{Cluster: "core", Tenant: "pay"}}
	page, err := r.ListRunsAcross(context.Background(), RunsAcrossQuery{Scopes: scope, Sort: RunSortDuration, Limit: 1})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if page.NextCursor == "" {
		t.Fatal("first page has no cursor")
	}

	cases := []struct {
		name  string
		query RunsAcrossQuery
	}{
		{"another sort", RunsAcrossQuery{Scopes: scope, Sort: RunSortStarted, Limit: 1, Cursor: page.NextCursor}},
		{"another order", RunsAcrossQuery{Scopes: scope, Sort: RunSortDuration, Ascending: true, Limit: 1, Cursor: page.NextCursor}},
		{"malformed", RunsAcrossQuery{Scopes: scope, Sort: RunSortDuration, Limit: 1, Cursor: "%%%"}},
	}
	for _, tc := range cases {
		_, err := r.ListRunsAcross(context.Background(), tc.query)
		if !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("%s: err = %v; want ErrInvalidCursor", tc.name, err)
		}
	}
}

func TestRunStatusRankMatchesSQL(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	base := time.Now().UTC().Add(-time.Hour)
	cases := []struct {
		job, status string
		result      *string
	}{
		{"pay/failure", "COMPLETED", strPtr("FAILURE")},
		{"pay/trigger", "COMPLETED", strPtr("TRIGGER_FAILED")},
		{"pay/interrupted", "COMPLETED", strPtr("INTERRUPTED")},
		{"pay/unstable", "COMPLETED", strPtr("UNSTABLE")},
		{"pay/running", "RUNNING", nil},
		{"pay/queued", "QUEUED", nil},
		{"pay/aborted", "COMPLETED", strPtr("ABORTED")},
		{"pay/success", "COMPLETED", strPtr("SUCCESS")},
		{"pay/other", "COMPLETED", nil},
	}
	for i, c := range cases {
		id := seedAcrossRun(t, db, "pay", c.job, base.Add(time.Duration(i)*time.Second), c.status, c.result)
		var sqlRank int
		if err := db.QueryRow("SELECT "+runStatusRankSQL+" FROM varroa.runs WHERE run_id = $1", id).Scan(&sqlRank); err != nil {
			t.Fatalf("%s: rank query: %v", c.job, err)
		}
		runs, err := r.ListRunsAcross(context.Background(), RunsAcrossQuery{
			Scopes: []RunScope{{Cluster: "core", Tenant: "pay"}}, JobPath: c.job,
		})
		if err != nil || len(runs.Runs) != 1 {
			t.Fatalf("%s: read back: %v, %d runs", c.job, err, len(runs.Runs))
		}
		if got := runStatusRank(runs.Runs[0]); got != sqlRank {
			t.Errorf("%s: runStatusRank = %d; SQL rank = %d", c.job, got, sqlRank)
		}
	}
}
