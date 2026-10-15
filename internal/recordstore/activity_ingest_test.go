package recordstore

import (
	"context"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// TestActivityIngesterInsertConflictIgnoring asserts a redelivered
// message writes exactly one row: the second Insert of the same key
// reports it was not newly inserted, and the table still holds one row.
func TestActivityIngesterInsertConflictIgnoring(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ing := NewActivityIngester(pool)
	ctx := context.Background()

	e := activity.Event{
		Cluster: "core", Timestamp: time.Now().UTC(), Source: "operator",
		Type: "controller.created", Namespace: "jenkins", Controller: "foo",
		Message: "created",
	}
	e.Normalize()
	key := EventKey(e)

	inserted, err := ing.Insert(ctx, e, key)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if !inserted {
		t.Fatal("want the first insert to report newly inserted")
	}

	inserted, err = ing.Insert(ctx, e, key)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if inserted {
		t.Fatal("want the redelivered insert to report a conflict, not a new row")
	}

	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM varroa.activity WHERE event_key = $1", key).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("want exactly one row for a redelivered event, got %d", count)
	}
}

// TestActivityIngesterInsertStoresDerivedSeverity asserts a stored row
// carries the severity Event.Normalize derives, proving the recorder
// calls Normalize before the write.
func TestActivityIngesterInsertStoresDerivedSeverity(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ing := NewActivityIngester(pool)
	ctx := context.Background()

	e := activity.Event{
		Cluster: "core", Timestamp: time.Now().UTC(), Source: "operator",
		Type: "controller.failed", Reason: "provisioning failed", Message: "boom",
	}
	// No explicit severity: Normalize must derive one from type/phase/reason.
	e.Normalize()
	if e.Severity != "error" {
		t.Fatalf("test setup: want Normalize to derive severity=error, got %q", e.Severity)
	}
	key := EventKey(e)

	if _, err := ing.Insert(ctx, e, key); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var severity string
	if err := db.QueryRowContext(ctx, "SELECT severity FROM varroa.activity WHERE event_key = $1", key).Scan(&severity); err != nil {
		t.Fatalf("read severity: %v", err)
	}
	if severity != "error" {
		t.Fatalf("stored severity = %q, want %q", severity, "error")
	}
}

// TestActivityIngesterInsertNullsZeroBuildNumber asserts a zero build
// number stores SQL NULL, matching D1's nullable build_number column.
func TestActivityIngesterInsertNullsZeroBuildNumber(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ing := NewActivityIngester(pool)
	ctx := context.Background()

	e := activity.Event{Cluster: "core", Timestamp: time.Now().UTC(), Source: "mite", Type: "heartbeat"}
	e.Normalize()
	key := EventKey(e)

	if _, err := ing.Insert(ctx, e, key); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var buildNumber *int64
	if err := db.QueryRowContext(ctx, "SELECT build_number FROM varroa.activity WHERE event_key = $1", key).Scan(&buildNumber); err != nil {
		t.Fatalf("read build_number: %v", err)
	}
	if buildNumber != nil {
		t.Fatalf("build_number = %v, want NULL for a zero build number", *buildNumber)
	}
}
