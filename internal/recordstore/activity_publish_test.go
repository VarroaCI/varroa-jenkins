package recordstore

import (
	"context"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// TestPublishAfterCommit_ReingestWritesNoSecondRow writes one event through
// the outbox helper, then re-ingests the same event as a consumer would. The
// second write must find the row and write nothing.
func TestPublishAfterCommit_ReingestWritesNoSecondRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	e := activity.Event{
		Cluster: "core", Timestamp: time.Now().UTC(), Source: "recorder",
		Type: "tenant.placement_moved", Namespace: "jenkins", Controller: "cell-x",
		Message: "publish-after-commit re-ingest",
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var out activityOutbox
	if err := insertActivityForPublish(ctx, tx, e, &out); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(out.events) != 1 {
		t.Fatalf("outbox holds %d events, want 1", len(out.events))
	}
	published := out.events[0]

	inserted, err := NewActivityIngester(pool).Insert(ctx, published, activity.EventKey(published))
	if err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	if inserted {
		t.Fatal("the re-ingested copy reported a new row")
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.activity WHERE message = $1`, e.Message).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("activity rows = %d, want 1", n)
	}
}

// TestPublishAfterCommit_RollbackPublishesNothing rolls the transaction back
// and checks that no row lands and the outbox stays empty of nothing written.
func TestPublishAfterCommit_RollbackPublishesNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	e := activity.Event{
		Cluster: "core", Timestamp: time.Now().UTC(), Source: "recorder",
		Type: "tenant.placement_moved", Namespace: "jenkins", Controller: "cell-y",
		Message: "rolled back publish",
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var out activityOutbox
	if err := insertActivityForPublish(ctx, tx, e, &out); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	publishActivityAfterCommit(nil, &out)
	if len(out.events) != 0 {
		t.Fatalf("outbox holds %d events after the publish step, want 0", len(out.events))
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.activity WHERE message = $1`, e.Message).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("activity rows after rollback = %d, want 0", n)
	}
}
