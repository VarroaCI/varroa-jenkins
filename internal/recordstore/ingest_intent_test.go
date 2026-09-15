package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// waitForRowLockWaiter polls pg_stat_activity until one backend waits on
// a row lock inside a statement that contains fragment, which proves the
// other transaction's lock is what holds it. It fails the test when no
// waiter appears within five seconds, so a removed lock cannot pass the
// test through a vacuous timeout.
func waitForRowLockWaiter(t *testing.T, db *sql.DB, fragment string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := db.QueryRow(`
			SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%' || $1 || '%'
		`, fragment).Scan(&n); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no backend waited on the intent row lock within 5s")
}

// TestLoadIntentBlocksConcurrentClaim asserts an ingest transaction that
// loaded the intent row holds a lock a concurrent claim on the same run
// waits for, and that the claim proceeds once the transaction commits.
func TestLoadIntentBlocksConcurrentClaim(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:lock-ingest-first", 1)
	req.JenkinsNextBuildNumber = 1
	first := mustGrant(t, ledger, req)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := loadIntent(ctx, tx, uuid.MustParse(first.RunID)); err != nil {
		t.Fatalf("loadIntent: %v", err)
	}

	repeat := req
	repeat.CellInstance = "pod-b"
	repeat.JenkinsNextBuildNumber = first.BuildNumber + 1
	answers := make(chan ClaimResponse, 1)
	go func() { answers <- ledger.Claim(ctx, repeat) }()

	// existingKeyDecision's SELECT ... FOR UPDATE on run_intents is the
	// statement that waits.
	waitForRowLockWaiter(t, db, "FROM varroa.run_intents")
	select {
	case resp := <-answers:
		t.Fatalf("claim answered while the ingest transaction held the intent row: %+v", resp)
	default:
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	select {
	case resp := <-answers:
		if resp.Decision != DecisionGranted || resp.BuildNumber != first.BuildNumber+1 {
			t.Fatalf("want granted with build_number %d after the commit, got %+v", first.BuildNumber+1, resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("claim did not answer within 5s of the commit")
	}
}

// TestClaimRefreshBlocksConcurrentLoadIntent asserts an ingest write for
// a run waits for a claim transaction that moves and refreshes the same
// intent, and then compares against the refreshed number: the old
// instance's start for the old number reads as superseded.
func TestClaimRefreshBlocksConcurrentLoadIntent(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:lock-claim-first", 1)
	req.JenkinsNextBuildNumber = 1
	first := mustGrant(t, ledger, req)
	runID := uuid.MustParse(first.RunID)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var locked string
	if err := tx.QueryRow(ctx, `SELECT state FROM varroa.run_intents WHERE intent_key = $1 FOR UPDATE`, req.IntentKey).Scan(&locked); err != nil {
		t.Fatalf("lock intent: %v", err)
	}
	fresh, err := reserveBuildNumber(ctx, tx, req.Cluster, req.Tenant, req.JobPath, first.BuildNumber+1)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE varroa.run_intents
		SET cell = 'cell-b', cell_instance = 'pod-b', placement_generation = 2, build_number = $1
		WHERE run_id = $2
	`, fresh, runID); err != nil {
		t.Fatalf("move and refresh: %v", err)
	}

	g := NewIngester(pool, nil, nil)
	old := first.BuildNumber
	env := newTestEnvelope(t, runID, req.IntentKey, KindRunStarted, RunStartedPayload{BuildNumber: &old})
	results := make(chan ApplyResult, 1)
	errs := make(chan error, 1)
	go func() {
		res, applyErr := g.Apply(ctx, env)
		if applyErr != nil {
			errs <- applyErr
			return
		}
		results <- res
	}()

	// loadIntent's SELECT ... FOR UPDATE on run_intents is the statement
	// that waits.
	waitForRowLockWaiter(t, db, "FROM varroa.run_intents")
	select {
	case res := <-results:
		t.Fatalf("apply finished while the claim transaction held the intent row: %+v", res)
	case applyErr := <-errs:
		t.Fatalf("apply failed while the claim transaction held the intent row: %v", applyErr)
	default:
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	select {
	case res := <-results:
		if !res.SupersededStart || res.SupersededReported != old || res.SupersededStored != fresh || res.SupersededOwner != "pod-b" {
			t.Fatalf("want a superseded start against the refreshed row, got %+v", res)
		}
	case applyErr := <-errs:
		t.Fatalf("apply failed after the commit: %v", applyErr)
	case <-time.After(5 * time.Second):
		t.Fatal("apply did not finish within 5s of the commit")
	}

	var stored int64
	if err := db.QueryRow(`SELECT build_number FROM varroa.run_intents WHERE run_id = $1`, runID).Scan(&stored); err != nil {
		t.Fatalf("read intent: %v", err)
	}
	if stored != fresh {
		t.Fatalf("intent build_number = %d, want the fresh %d", stored, fresh)
	}
}
