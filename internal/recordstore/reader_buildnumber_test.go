package recordstore

import (
	"context"
	"testing"
)

// TestListRunsFiltersBuildNumber asserts that RunQuery.BuildNumber keeps only
// the run with that number, and that a zero value applies no filter.
func TestListRunsFiltersBuildNumber(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	ingester := NewIngester(pool, nil, nil)
	reader := NewReader(pool)
	ctx := context.Background()

	var numbers []int64
	var ids []string
	for i := 0; i < 3; i++ {
		key := "manual:build-number-" + string(rune('a'+i))
		resp := mustGrant(t, ledger, newTestClaimRequest(key, 0))
		number := resp.BuildNumber
		env := newTestEnvelope(t, parseTestUUID(t, resp.RunID), key, KindRunStarted, RunStartedPayload{BuildNumber: &number})
		if _, err := ingester.Apply(ctx, env); err != nil {
			t.Fatalf("apply run.started for %s: %v", key, err)
		}
		numbers = append(numbers, resp.BuildNumber)
		ids = append(ids, resp.RunID)
	}

	t.Run("exact build number", func(t *testing.T) {
		page, err := reader.ListRuns(ctx, RunQuery{
			Cluster: testCluster, Tenant: testTenant, BuildNumber: numbers[1], Limit: 10,
		})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(page.Runs) != 1 {
			t.Fatalf("got %d runs, want 1", len(page.Runs))
		}
		if page.Runs[0].RunID != ids[1] {
			t.Errorf("run = %s, want %s", page.Runs[0].RunID, ids[1])
		}
	})

	t.Run("zero applies no filter", func(t *testing.T) {
		page, err := reader.ListRuns(ctx, RunQuery{
			Cluster: testCluster, Tenant: testTenant, Limit: 10,
		})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(page.Runs) < len(ids) {
			t.Errorf("got %d runs, want at least %d", len(page.Runs), len(ids))
		}
	})
}
