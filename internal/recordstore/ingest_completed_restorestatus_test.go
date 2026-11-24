package recordstore

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

// restoreRepo is the repository URL that the restore tests store on each run.
const restoreRepo = "https://git.example.org/acme/app"

// restoreClock is a fixed start for the run times of the restore tests.
var restoreClock = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

// buildNumberOf reads the build number that the ledger granted runID.
func (r *backstopTestRig) buildNumberOf(t *testing.T, runID uuid.UUID) int64 {
	t.Helper()
	var number int64
	if err := r.pool.QueryRow(context.Background(),
		`SELECT build_number FROM varroa.run_intents WHERE run_id = $1`, runID).Scan(&number); err != nil {
		t.Fatalf("read granted number of %s: %v", runID, err)
	}
	return number
}

// startAt applies run.started for runID with its granted build number, the
// revision, the repository, and the start time.
func (r *backstopTestRig) startAt(t *testing.T, runID uuid.UUID, key, revision string, startedAt time.Time) {
	t.Helper()
	payload := map[string]any{
		"build_number": r.buildNumberOf(t, runID),
		"scm_revision": revision,
		"scm_url":      restoreRepo,
		"started_at":   startedAt,
	}
	if _, err := r.g.Apply(context.Background(), newTestEnvelope(t, runID, key, KindRunStarted, payload)); err != nil {
		t.Fatalf("apply run.started %s: %v", key, err)
	}
}

// completeAt applies the owner run.completed of runID with the result and
// the end time.
func (r *backstopTestRig) completeAt(t *testing.T, runID uuid.UUID, key, result string, endedAt time.Time) {
	t.Helper()
	payload := map[string]any{"result": result, "ended_at": endedAt, "seq": 1000}
	if _, err := r.g.Apply(context.Background(), newTestEnvelope(t, runID, key, KindRunCompleted, payload)); err != nil {
		t.Fatalf("apply run.completed %s: %v", key, err)
	}
}

// wantRestore fails the test unless the next ledger action is one
// restore_commit_status for the newest run, with its result and the
// superseded build numbers.
func (r *backstopTestRig) wantRestore(t *testing.T, name string, newest uuid.UUID, revision, result string, superseded ...int64) {
	t.Helper()
	select {
	case a := <-r.actions:
		if a.Kind != LedgerActionRestoreCommitStatus {
			t.Fatalf("%s: published %s for run %s, want restore_commit_status", name, a.Kind, a.RunID)
		}
		number := r.buildNumberOf(t, newest)
		if a.RunID != newest.String() || a.BuildNumber == nil || *a.BuildNumber != number || a.Result != result {
			t.Fatalf("%s: restore names run %s build %v result %q, want run %s build %d result %q",
				name, a.RunID, a.BuildNumber, a.Result, newest, number, result)
		}
		if !slices.Equal(a.SupersededBuilds, superseded) {
			t.Fatalf("%s: superseded builds = %v, want %v", name, a.SupersededBuilds, superseded)
		}
		if a.Cluster != testCluster || a.Tenant != testTenant || a.JobPath != testJobPath ||
			a.Namespace != testNamespace || a.Controller != "cell-a" || a.Actor != LedgerActionRecorder ||
			a.Repo != restoreRepo || a.Revision != revision || a.Reason != "" {
			t.Fatalf("%s: restore fields = %+v", name, a)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no restore_commit_status action was published", name)
	}
}

// TestRestoreStatusWhenAnOlderRunEndsLast asserts the order rule of a forge
// status context. A re-run builds beside an older live run of the same
// commit and ends first. Each completion then publishes a restore action
// for the newer run. The action after the older run's completion is the
// one that puts the newer result back.
func TestRestoreStatusWhenAnOlderRunEndsLast(t *testing.T) {
	t.Parallel()

	r := newBackstopTestRig(t)
	const revision = "rev-restore-older-last"
	olderKey, newerKey := "scm:acme/app:"+revision, "manual:"+newTestUUIDv7String()
	older := seedGrantedIntent(t, r.pool, olderKey)
	newer := seedGrantedIntent(t, r.pool, newerKey)

	r.startAt(t, older, olderKey, revision, restoreClock)
	r.startAt(t, newer, newerKey, revision, restoreClock.Add(time.Minute))
	r.wantNoAction(t, "two live runs")

	r.completeAt(t, newer, newerKey, "SUCCESS", restoreClock.Add(2*time.Minute))
	r.wantRestore(t, "newer run ends first", newer, revision, "SUCCESS", r.buildNumberOf(t, older))

	r.completeAt(t, older, olderKey, "FAILURE", restoreClock.Add(3*time.Minute))
	r.wantRestore(t, "older run ends last", newer, revision, "SUCCESS", r.buildNumberOf(t, older))
}

// TestRestoreStatusWaitsForALiveNewerRun asserts that the completion of an
// older run publishes nothing while a newer run of the revision is live.
// The newer run writes its own status last.
func TestRestoreStatusWaitsForALiveNewerRun(t *testing.T) {
	t.Parallel()

	r := newBackstopTestRig(t)
	const revision = "rev-restore-newer-live"
	olderKey, newerKey := "manual:"+newTestUUIDv7String(), "manual:"+newTestUUIDv7String()
	older := seedGrantedIntent(t, r.pool, olderKey)
	newer := seedGrantedIntent(t, r.pool, newerKey)

	r.startAt(t, older, olderKey, revision, restoreClock)
	r.startAt(t, newer, newerKey, revision, restoreClock.Add(time.Minute))
	r.completeAt(t, older, olderKey, "FAILURE", restoreClock.Add(2*time.Minute))
	r.wantNoAction(t, "older run ends while the newer run builds")
}

// TestRestoreStatusSkipsARevisionWithOneRun asserts that a revision with one
// run publishes nothing.
func TestRestoreStatusSkipsARevisionWithOneRun(t *testing.T) {
	t.Parallel()

	r := newBackstopTestRig(t)
	const revision = "rev-restore-one-run"
	key := "scm:acme/app:" + revision
	run := seedGrantedIntent(t, r.pool, key)

	r.startAt(t, run, key, revision, restoreClock)
	r.completeAt(t, run, key, "SUCCESS", restoreClock.Add(time.Minute))
	r.wantNoAction(t, "one run")
}

// TestRestoreStatusSkipsAnEarlierRebuild asserts that a run that ended
// before the newest run started is not superseded. A later rebuild of an
// old revision therefore costs no forge read.
func TestRestoreStatusSkipsAnEarlierRebuild(t *testing.T) {
	t.Parallel()

	r := newBackstopTestRig(t)
	const revision = "rev-restore-rebuild"
	firstKey, rebuildKey := "manual:"+newTestUUIDv7String(), "manual:"+newTestUUIDv7String()
	first := seedGrantedIntent(t, r.pool, firstKey)
	rebuild := seedGrantedIntent(t, r.pool, rebuildKey)

	r.startAt(t, first, firstKey, revision, restoreClock)
	r.completeAt(t, first, firstKey, "FAILURE", restoreClock.Add(time.Minute))
	r.wantNoAction(t, "first run")
	r.startAt(t, rebuild, rebuildKey, revision, restoreClock.Add(2*time.Minute))
	r.completeAt(t, rebuild, rebuildKey, "SUCCESS", restoreClock.Add(3*time.Minute))
	r.wantNoAction(t, "rebuild after the first run ended")
}

// TestRestoreStatusKeepsTheOlderRunOverADuplicate asserts that a DUPLICATE
// run never owns the status context. The older run gives the outcome of the
// revision, so the aborted status of the DUPLICATE run is superseded.
func TestRestoreStatusKeepsTheOlderRunOverADuplicate(t *testing.T) {
	t.Parallel()

	r := newBackstopTestRig(t)
	const revision = "rev-restore-duplicate"
	olderKey, duplicateKey := "api:"+newTestUUIDv7String(), "api:"+newTestUUIDv7String()
	older := seedGrantedIntent(t, r.pool, olderKey)
	duplicate := seedGrantedIntent(t, r.pool, duplicateKey)

	r.startAt(t, older, olderKey, revision, restoreClock)
	r.startAt(t, duplicate, duplicateKey, revision, restoreClock.Add(time.Minute))
	r.wantCancel(t, "backstop", duplicate)
	r.wantStatus(t, "backstop", duplicate, StatusDuplicate)

	r.completeAt(t, older, olderKey, "UNSTABLE", restoreClock.Add(2*time.Minute))
	r.wantRestore(t, "older run ends", older, revision, "UNSTABLE", r.buildNumberOf(t, duplicate))

	r.completeAt(t, duplicate, duplicateKey, "ABORTED", restoreClock.Add(3*time.Minute))
	r.wantRestore(t, "duplicate run ends last", older, revision, "UNSTABLE", r.buildNumberOf(t, duplicate))
}

// TestRestoreStatusReadFailureKeepsTheCompletion asserts that a failed
// restore read never fails, rolls back, or delays the completion. The read
// runs on the pool after the commit, so a SQL error there cannot abort the
// completion's own transaction.
func TestRestoreStatusReadFailureKeepsTheCompletion(t *testing.T) {
	t.Parallel()

	r := newBackstopTestRig(t)
	reads := 0
	r.g.restoreRead = func(ctx context.Context, q restoreQuerier, _ *intentRow) (*LedgerAction, error) {
		reads++
		var n int
		return nil, q.QueryRow(ctx, `SELECT no_such_column FROM varroa.runs LIMIT 1`).Scan(&n)
	}
	const revision = "rev-restore-read-fails"
	olderKey, newerKey := "manual:"+newTestUUIDv7String(), "manual:"+newTestUUIDv7String()
	older := seedGrantedIntent(t, r.pool, olderKey)
	newer := seedGrantedIntent(t, r.pool, newerKey)

	r.startAt(t, older, olderKey, revision, restoreClock)
	r.startAt(t, newer, newerKey, revision, restoreClock.Add(time.Minute))
	r.completeAt(t, newer, newerKey, "SUCCESS", restoreClock.Add(2*time.Minute))
	r.completeAt(t, older, olderKey, "FAILURE", restoreClock.Add(3*time.Minute))

	if reads != 2 {
		t.Fatalf("restore reads = %d, want one for each completion", reads)
	}
	for _, run := range []struct {
		id     uuid.UUID
		result string
	}{{newer, "SUCCESS"}, {older, "FAILURE"}} {
		var status, result string
		if err := r.pool.QueryRow(context.Background(),
			`SELECT status, COALESCE(result, '') FROM varroa.runs WHERE run_id = $1`, run.id).Scan(&status, &result); err != nil {
			t.Fatalf("read run %s: %v", run.id, err)
		}
		if status != StatusCompleted || result != run.result {
			t.Fatalf("run %s = %s %s, want %s %s", run.id, status, result, StatusCompleted, run.result)
		}
	}
	r.wantNoAction(t, "failed restore read")
}
