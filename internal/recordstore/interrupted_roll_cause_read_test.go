package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// rollCauseReadPassLimit is how long the timeout test waits for the mark to
// pass its roll cause read. It is far above cellRollCauseReadTimeout.
const rollCauseReadPassLimit = 20 * time.Second

// refuseActivityReads replaces varroa.activity with a view that refuses
// every read and still takes every insert. The mark then reaches its own
// build.interrupted insert, and only the roll cause read fails.
func refuseActivityReads(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`ALTER TABLE varroa.activity RENAME TO activity_base`,
		`CREATE FUNCTION varroa.refuse_activity_read() RETURNS boolean LANGUAGE plpgsql STABLE
		 AS $$ BEGIN RAISE EXCEPTION 'activity read refused'; END $$`,
		`CREATE VIEW varroa.activity AS SELECT * FROM varroa.activity_base WHERE varroa.refuse_activity_read()`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("refuse activity reads: %v", err)
		}
	}
}

// assertMarkedWithoutDetail fails the test unless the run reads COMPLETED,
// INTERRUPTED, and cell-rolled, with no interrupted_detail key.
func assertMarkedWithoutDetail(t *testing.T, db *sql.DB, runID uuid.UUID) {
	t.Helper()
	var status, result string
	var reason sql.NullString
	if err := db.QueryRow(`
		SELECT status, COALESCE(result, ''), payload->>'interrupted_reason' FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &result, &reason); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != StatusCompleted || result != "INTERRUPTED" || reason.String != ReasonCellRolled {
		t.Fatalf("status=%q result=%q reason=%v, want COMPLETED, INTERRUPTED, and cell-rolled", status, result, reason)
	}
	if detail, found := readInterruptedDetail(t, db, runID); found {
		t.Fatalf("interrupted_detail = %q, want no key", detail)
	}
}

// waitForActivityInsertLockWait waits until a statement that inserts into
// varroa.activity waits on a lock in the test database. The mark reaches
// its own activity insert only after its roll cause read ended.
func waitForActivityInsertLockWait(t *testing.T, db *sql.DB, limit time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		var waiting int
		if err := db.QueryRow(`
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			  AND query LIKE '%INSERT INTO varroa.activity%'
		`).Scan(&waiting); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestCellRolledMarkCommitsWhenTheActivityReadFails asserts that a failed
// roll cause read never stops the mark. The run reads INTERRUPTED with the
// reason cell-rolled and no detail, and the mark's own activity row exists.
func TestCellRolledMarkCommitsWhenTheActivityReadFails(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := startRunOnCell(t, pool, "manual:"+newTestUUIDv7String(), "refused-a")
	c := fetchRunningCandidate(t, pool, runID)
	mark := livenessBaseline(c).Add(10 * time.Minute).UTC().Truncate(time.Microsecond)
	ingestActivityEvent(t, pool, cellRolledRow("refused-a", mark.Add(-time.Second), "MiteUnreachable"))
	refuseActivityReads(t, db)

	changed, err := markInterrupted(context.Background(), pool, c, ReasonCellRolled, mark, "")
	if err != nil || !changed {
		t.Fatalf("mark cell-rolled: changed=%v err=%v, want a mark", changed, err)
	}
	assertMarkedWithoutDetail(t, db, runID)

	var rows int
	if err := db.QueryRow(`
		SELECT count(*) FROM varroa.activity_base WHERE type = 'build.interrupted' AND controller = 'refused-a'
	`).Scan(&rows); err != nil {
		t.Fatalf("count build.interrupted rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("build.interrupted rows = %d, want 1", rows)
	}
}

// TestCellRolledMarkCommitsWhenTheActivityReadTimesOut asserts that a roll
// cause read that passes cellRollCauseReadTimeout never stops the mark. A
// held table lock stops the read. The mark then waits on its own activity
// insert, which proves that the read gave up, and it commits when the lock
// goes.
func TestCellRolledMarkCommitsWhenTheActivityReadTimesOut(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := startRunOnCell(t, pool, "manual:"+newTestUUIDv7String(), "slow-a")
	c := fetchRunningCandidate(t, pool, runID)
	mark := livenessBaseline(c).Add(10 * time.Minute).UTC().Truncate(time.Microsecond)
	ingestActivityEvent(t, pool, cellRolledRow("slow-a", mark.Add(-time.Second), "MiteUnreachable"))

	locker, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin the locking transaction: %v", err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = locker.Rollback()
		}
	}
	t.Cleanup(release)
	if _, err := locker.Exec(`LOCK TABLE varroa.activity IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock varroa.activity: %v", err)
	}

	type markResult struct {
		changed bool
		err     error
	}
	done := make(chan markResult, 1)
	go func() {
		changed, err := markInterrupted(context.Background(), pool, c, ReasonCellRolled, mark, "")
		done <- markResult{changed, err}
	}()

	passedRead := waitForActivityInsertLockWait(t, db, rollCauseReadPassLimit)
	release()
	var r markResult
	select {
	case r = <-done:
	case <-time.After(time.Minute):
		t.Fatal("the mark did not end after the lock went")
	}
	if !passedRead {
		t.Fatalf("the mark did not pass the roll cause read within %s", rollCauseReadPassLimit)
	}
	if r.err != nil || !r.changed {
		t.Fatalf("mark cell-rolled: changed=%v err=%v, want a mark", r.changed, r.err)
	}
	assertMarkedWithoutDetail(t, db, runID)
}
