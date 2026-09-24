package recordstore

import (
	"context"
	"fmt"
	"testing"
)

// TestListTestCasesOrdersFailedFirst asserts that every row that is not
// SKIPPED precedes every SKIPPED row, and that each group orders by suite,
// class name, and test name.
func TestListTestCasesOrdersFailedFirst(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:cases-order-1")
	g := NewIngester(pool, nil, nil)
	reader := NewReader(pool)
	ctx := context.Background()

	env := newTestEnvelope(t, runID, "manual:cases-order-1", KindRunTests, RunTestsPayload{
		Suite: "_total", Total: 5, Failed: 3, Skipped: 2,
		Cases: []RunTestCasePayload{
			{Suite: "b", ClassName: "C", Name: "skip1", Status: "SKIPPED"},
			{Suite: "b", ClassName: "C", Name: "fail2", Status: "FAILED"},
			{Suite: "a", ClassName: "C", Name: "skip0", Status: "SKIPPED"},
			{Suite: "a", ClassName: "D", Name: "fail1", Status: "REGRESSION"},
			{Suite: "a", ClassName: "C", Name: "fail0", Status: "FAILED"},
		},
	})
	if _, err := g.Apply(ctx, env); err != nil {
		t.Fatalf("apply: %v", err)
	}

	cases, err := reader.ListTestCases(ctx, runID.String())
	if err != nil {
		t.Fatalf("ListTestCases: %v", err)
	}
	names := make([]string, 0, len(cases))
	for _, c := range cases {
		names = append(names, c.Name)
	}
	if got, want := fmt.Sprint(names), "[fail0 fail1 fail2 skip0 skip1]"; got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}

	empty, err := reader.ListTestCases(ctx, "00000000-0000-7000-8000-000000000000")
	if err != nil {
		t.Fatalf("ListTestCases for an unknown run: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("unknown run answered %d cases, want none", len(empty))
	}
}

// TestListTestCaseHistoryNewestFirst asserts that the history read answers
// one row per run for one class and name, newest first, and honors the
// limit.
func TestListTestCaseHistoryNewestFirst(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	reader := NewReader(pool)
	ctx := context.Background()

	older := seedGrantedIntent(t, pool, "manual:history-1")
	newer := seedGrantedIntent(t, pool, "manual:history-2")
	for i, runID := range []struct {
		key    string
		status string
	}{{"manual:history-1", "FAILED"}, {"manual:history-2", "REGRESSION"}} {
		id := older
		if i == 1 {
			id = newer
		}
		env := newTestEnvelope(t, id, runID.key, KindRunTests, RunTestsPayload{
			Suite: "_total", Total: 2, Failed: 1,
			Cases: []RunTestCasePayload{
				{Suite: "unit", ClassName: "com.example.FooTest", Name: "bar", Status: runID.status, ErrorDetails: "boom"},
				{Suite: "unit", ClassName: "com.example.FooTest", Name: "other", Status: "FAILED"},
			},
		})
		if _, err := g.Apply(ctx, env); err != nil {
			t.Fatalf("apply %s: %v", runID.key, err)
		}
	}

	q := TestCaseHistoryQuery{
		Cluster: testCluster, Tenant: testTenant, JobPath: testJobPath,
		ClassName: "com.example.FooTest", Name: "bar", Limit: 50,
	}
	outcomes, err := reader.ListTestCaseHistory(ctx, q)
	if err != nil {
		t.Fatalf("ListTestCaseHistory: %v", err)
	}
	if len(outcomes) != 2 {
		t.Fatalf("outcomes = %d, want 2", len(outcomes))
	}
	if outcomes[0].RunID != newer.String() || outcomes[1].RunID != older.String() {
		t.Fatalf("order = [%s %s], want newest first [%s %s]", outcomes[0].RunID, outcomes[1].RunID, newer, older)
	}
	if outcomes[0].Status != "REGRESSION" || outcomes[0].Suite != "unit" || outcomes[0].ErrorDetails != "boom" {
		t.Fatalf("newest outcome = %+v", outcomes[0])
	}
	if outcomes[0].CreatedAt.Before(outcomes[1].CreatedAt) {
		t.Fatalf("created_at order = %v then %v, want descending", outcomes[0].CreatedAt, outcomes[1].CreatedAt)
	}

	q.Limit = 1
	limited, err := reader.ListTestCaseHistory(ctx, q)
	if err != nil {
		t.Fatalf("ListTestCaseHistory limited: %v", err)
	}
	if len(limited) != 1 || limited[0].RunID != newer.String() {
		t.Fatalf("limited = %+v, want the newest row alone", limited)
	}

	q.Limit = 50
	q.Name = "missing"
	none, err := reader.ListTestCaseHistory(ctx, q)
	if err != nil {
		t.Fatalf("ListTestCaseHistory for a missing case: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("missing case answered %d rows, want none", len(none))
	}
}
