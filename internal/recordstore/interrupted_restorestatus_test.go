package recordstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// wantEnd fails the test unless the next ledger action is one
// end_commit_status for runID with its build number and reason.
func (r *backstopTestRig) wantEnd(t *testing.T, name string, runID uuid.UUID, reason string) {
	t.Helper()
	select {
	case a := <-r.actions:
		number := r.buildNumberOf(t, runID)
		if a.Kind != LedgerActionEndCommitStatus || a.RunID != runID.String() || a.BuildNumber == nil ||
			*a.BuildNumber != number || a.Reason != reason {
			t.Fatalf("%s: published %+v, want end_commit_status for run %s build %d reason %q", name, a, runID, number, reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no end_commit_status action was published", name)
	}
}

// markRun marks runID interrupted with reason through the marking write and
// returns the candidate that the write read.
func (r *backstopTestRig) markRun(t *testing.T, runID uuid.UUID, reason string) runInterruptCandidate {
	t.Helper()
	c := fetchRunningCandidate(t, r.pool, runID)
	changed, err := markInterrupted(context.Background(), r.pool, c, reason, time.Now(), "")
	if err != nil || !changed {
		t.Fatalf("mark %s %s: changed %v, err %v", runID, reason, changed, err)
	}
	return c
}

// TestSweepRestoresTheNewestStatusAfterAnOlderRunIsMarked asserts the order
// rule of a forge status context after a synthetic end. A newer run of the
// commit completes while an older run still builds. The older run can then
// write its pending status over the newer result. The sweep ends the older
// run, and the end sets that pending status to error. The sweep therefore
// publishes a restore_commit_status for the newer run after the end.
func TestSweepRestoresTheNewestStatusAfterAnOlderRunIsMarked(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)
	r := &backstopTestRig{pool: pool, g: NewIngester(pool, nil, nil), actions: subscribeClusterActions(t, conn)}

	const revision = "rev-mark-older"
	olderKey, newerKey := "manual:"+newTestUUIDv7String(), "manual:"+newTestUUIDv7String()
	older := seedGrantedIntent(t, pool, olderKey)
	newer := seedGrantedIntent(t, pool, newerKey)
	r.startAt(t, older, olderKey, revision, restoreClock)
	r.startAt(t, newer, newerKey, revision, restoreClock.Add(time.Minute))
	r.completeAt(t, newer, newerKey, "SUCCESS", restoreClock.Add(2*time.Minute))

	past := time.Now().Add(-17 * time.Minute)
	if _, err := db.Exec(`UPDATE varroa.runs SET heartbeat_received_at = $1 WHERE run_id = $2`, past, older); err != nil {
		t.Fatalf("backdate heartbeat_received_at: %v", err)
	}
	seedTenantPlacementLiveness(t, db, "pod-a", false, &past, &past, nil)

	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, nil, nil, nil, nil, nil,
		nil, DefaultSweepMaxPending, time.Now(), testGrace, "", nil)
	if err != nil || marked != 1 {
		t.Fatalf("SweepInterruptedRuns: marked %d, err %v; want 1, nil", marked, err)
	}
	r.wantEnd(t, "older run marked", older, ReasonNoHeartbeat)
	r.wantRestore(t, "older run marked", newer, revision, "SUCCESS", r.buildNumberOf(t, older))
	r.wantCancel(t, "older run marked", older)
	r.wantNoAction(t, "older run marked")
}

// TestAfterMarkKeepsTheEndOfTheNewestRun asserts that the mark of the newest
// run of a revision publishes no restore_commit_status. An older run of the
// revision completed, but the end of the newest run gives the outcome of the
// newest build, so an older result never replaces it through the mark.
func TestAfterMarkKeepsTheEndOfTheNewestRun(t *testing.T) {
	t.Parallel()

	r := newBackstopTestRig(t)
	const revision = "rev-mark-newest"
	firstKey, rebuildKey := "manual:"+newTestUUIDv7String(), "manual:"+newTestUUIDv7String()
	first := seedGrantedIntent(t, r.pool, firstKey)
	rebuild := seedGrantedIntent(t, r.pool, rebuildKey)
	r.startAt(t, first, firstKey, revision, restoreClock)
	r.completeAt(t, first, firstKey, "SUCCESS", restoreClock.Add(time.Minute))
	r.startAt(t, rebuild, rebuildKey, revision, restoreClock.Add(2*time.Minute))
	r.wantNoAction(t, "first run and rebuild")

	c := r.markRun(t, rebuild, ReasonCellLost)
	afterMark(context.Background(), r.g.conn, r.pool, JobHome{}, nil, nil, c, ReasonCellLost, "", nil)
	r.wantEnd(t, "newest run marked", rebuild, ReasonCellLost)
	r.wantNoAction(t, "newest run marked")
}

// TestAfterMarkProvisionalMarkReadsNoRestore asserts that a provisional mark
// of an older run publishes nothing. The owner can still complete the run,
// and its owner completion runs the restore check.
func TestAfterMarkProvisionalMarkReadsNoRestore(t *testing.T) {
	t.Parallel()

	r := newBackstopTestRig(t)
	const revision = "rev-mark-provisional"
	olderKey, newerKey := "manual:"+newTestUUIDv7String(), "manual:"+newTestUUIDv7String()
	older := seedGrantedIntent(t, r.pool, olderKey)
	newer := seedGrantedIntent(t, r.pool, newerKey)
	r.startAt(t, older, olderKey, revision, restoreClock)
	r.startAt(t, newer, newerKey, revision, restoreClock.Add(time.Minute))
	r.completeAt(t, newer, newerKey, "SUCCESS", restoreClock.Add(2*time.Minute))
	r.wantRestore(t, "newer run ends first", newer, revision, "SUCCESS", r.buildNumberOf(t, older))

	c := r.markRun(t, older, ReasonHeartbeatLost)
	afterMark(context.Background(), r.g.conn, r.pool, JobHome{}, nil, nil, c, ReasonHeartbeatLost, "", nil)
	r.wantNoAction(t, "older run marked heartbeat-lost")
}

// failingRestoreQuerier fails every read of the restore check.
type failingRestoreQuerier struct{}

// failingRestoreRow fails its scan.
type failingRestoreRow struct{}

var errRestoreReadFails = errors.New("restore read fails")

func (failingRestoreRow) Scan(...any) error { return errRestoreReadFails }

func (failingRestoreQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	return failingRestoreRow{}
}

func (failingRestoreQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errRestoreReadFails
}

// TestAfterMarkRestoreReadFailureKeepsTheOtherActions asserts that a failed
// restore read after a mark publishes no restore and stops no other action
// of the mark.
func TestAfterMarkRestoreReadFailureKeepsTheOtherActions(t *testing.T) {
	t.Parallel()

	conn := newTestActionConn(t)
	received := subscribeLedgerActionsForTest(t, conn)
	run := sourcedCandidate(uuid.Must(uuid.NewV7()))

	afterMark(context.Background(), conn, failingRestoreQuerier{}, JobHome{}, nil, nil, run, ReasonNoHeartbeat, "", nil)

	var kinds []string
	for len(kinds) < 2 {
		select {
		case a := <-received:
			kinds = append(kinds, a.Kind)
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out; kinds so far %v", kinds)
		}
	}
	if kinds[0] != LedgerActionEndCommitStatus || kinds[1] != LedgerActionCancelRun {
		t.Fatalf("kinds = %v, want [end_commit_status cancel_run]", kinds)
	}
	expectNoLedgerAction(t, received)
}
