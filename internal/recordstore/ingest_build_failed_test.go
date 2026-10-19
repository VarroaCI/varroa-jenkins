package recordstore

import (
	"context"
	"testing"
)

// TestRunCompletedPublishesBuildFailed checks one build.failed row per failed
// owner terminal write, none for a success or an interruption, and no second
// row when the same completion is delivered again.
func TestRunCompletedPublishesBuildFailed(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	count := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM varroa.activity WHERE type = 'build.failed'`).Scan(&n); err != nil {
			t.Fatalf("count build.failed rows: %v", err)
		}
		return n
	}
	cases := []struct {
		result string
		want   int
	}{
		{"FAILURE", 1},
		{"UNSTABLE", 1},
		{"ABORTED", 1},
		{"SUCCESS", 0},
		{"INTERRUPTED", 0},
	}
	for _, c := range cases {
		t.Run(c.result, func(t *testing.T) {
			key := "manual:build-failed-" + c.result
			runID := seedGrantedIntent(t, pool, key)
			env := newTestEnvelope(t, runID, key, KindRunCompleted, RunCompletedPayload{Result: strPtr(c.result)})
			before := count()
			if _, err := NewIngester(pool, nil, nil).Apply(context.Background(), env); err != nil {
				t.Fatalf("apply run.completed: %v", err)
			}
			if got := count() - before; got != c.want {
				t.Fatalf("build.failed rows = %d, want %d", got, c.want)
			}
			if _, err := NewIngester(pool, nil, nil).Apply(context.Background(), env); err != nil {
				t.Fatalf("redeliver run.completed: %v", err)
			}
			if got := count() - before; got != c.want {
				t.Fatalf("after redelivery build.failed rows = %d, want %d", got, c.want)
			}
		})
	}
}
