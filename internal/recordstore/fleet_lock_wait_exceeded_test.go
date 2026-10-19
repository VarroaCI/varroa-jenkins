package recordstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestLockWaitExceeded_OneRowOnePublish checks that a wait of sixteen minutes
// writes one fleetlock.wait_exceeded row, and that a second round that sees
// the same wait writes no second row.
func TestLockWaitExceeded_OneRowOnePublish(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	name := "lock-wait-" + uuid.NewString()
	since := now.Add(-16 * time.Minute)
	insertLockRowForTenant(t, db, name, uuid.NewString(), "wait-t", "wait-t/job", "waiting", since, now.Add(time.Hour))

	count := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(`
			SELECT count(*) FROM varroa.activity
			WHERE type = 'fleetlock.wait_exceeded' AND reason = $1
		`, name).Scan(&n); err != nil {
			t.Fatalf("count wait rows: %v", err)
		}
		return n
	}

	written, err := publishLockWaitsExceeded(ctx, pool, nil, now, nil)
	if err != nil {
		t.Fatalf("first round: %v", err)
	}
	if written != 1 || count() != 1 {
		t.Fatalf("first round: written = %d, rows = %d, want 1 and 1", written, count())
	}

	if _, err := publishLockWaitsExceeded(ctx, pool, nil, now, nil); err != nil {
		t.Fatalf("second round: %v", err)
	}
	if n := count(); n != 1 {
		t.Fatalf("after a second round that sees the same wait: rows = %d, want 1", n)
	}
}
