package recordstore

import (
	"context"
	"testing"
	"time"
)

// TestApplyRunCompletedSupersedesInterruption asserts a real run.completed
// from the owning cell instance replaces a synthetic INTERRUPTED result
// and stamps interrupted_superseded_at.
func TestApplyRunCompletedSupersedesInterruption(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:interrupt-1")
	g := NewIngester(pool, nil, nil)

	buildNumber := int64(1)
	started := newTestEnvelope(t, runID, "manual:interrupt-1", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	// started's CellInstance is "pod-a" (newTestEnvelope's fixed value).

	// Stand in for C6's sweep: mark the run synthetically interrupted.
	if _, err := db.Exec(`UPDATE varroa.runs SET status = 'COMPLETED', result = 'INTERRUPTED' WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("stand-in sweep: %v", err)
	}

	result := "SUCCESS"
	durationMs := int64(5000)
	completed := newTestEnvelope(t, runID, "manual:interrupt-1", KindRunCompleted, RunCompletedPayload{
		Result:     &result,
		DurationMs: &durationMs,
	})
	// CellInstance must equal the intent's owning cell instance ("pod-a").
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	var resultGot string
	var durationGot int64
	var superseded time.Time
	if err := db.QueryRow(`
		SELECT result, duration_ms, interrupted_superseded_at FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&resultGot, &durationGot, &superseded); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if resultGot != result {
		t.Fatalf("result = %q, want %q", resultGot, result)
	}
	if durationGot != durationMs {
		t.Fatalf("duration_ms = %d, want %d", durationGot, durationMs)
	}
	if superseded.IsZero() {
		t.Fatal("interrupted_superseded_at was not stamped")
	}
}

// TestApplyRunCompletedWhereRunningMatchesNoCompletedRow asserts that
// once a real result exists, an update naming WHERE status = 'RUNNING'
// (the shape C6's interruption sweep uses) matches no row.
func TestApplyRunCompletedWhereRunningMatchesNoCompletedRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:no-reinterrupt-1")
	g := NewIngester(pool, nil, nil)

	buildNumber := int64(1)
	started := newTestEnvelope(t, runID, "manual:no-reinterrupt-1", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	result := "SUCCESS"
	completed := newTestEnvelope(t, runID, "manual:no-reinterrupt-1", KindRunCompleted, RunCompletedPayload{Result: &result})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	tag, err := db.Exec(`UPDATE varroa.runs SET status = 'COMPLETED', result = 'INTERRUPTED' WHERE run_id = $1 AND status = 'RUNNING'`, runID)
	if err != nil {
		t.Fatalf("simulated interruption update: %v", err)
	}
	if n, _ := tag.RowsAffected(); n != 0 {
		t.Fatalf("interruption update matched %d rows, want 0", n)
	}

	var resultGot string
	var superseded *time.Time
	if err := db.QueryRow(`SELECT result, interrupted_superseded_at FROM varroa.runs WHERE run_id = $1`, runID).
		Scan(&resultGot, &superseded); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if resultGot != "SUCCESS" {
		t.Fatalf("result = %q, want SUCCESS", resultGot)
	}
	if superseded != nil {
		t.Fatalf("interrupted_superseded_at = %v, want nil", *superseded)
	}
}

// TestApplyRunCompletedForeignInstanceIsAuditOnly asserts a completion
// from a non-owning cell instance changes no terminal column and merges
// into payload.foreign_completion instead.
func TestApplyRunCompletedForeignInstanceIsAuditOnly(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:foreign-1")
	g := NewIngester(pool, nil, nil)

	buildNumber := int64(1)
	started := newTestEnvelope(t, runID, "manual:foreign-1", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	result := "FAILURE"
	foreign := newTestEnvelope(t, runID, "manual:foreign-1", KindRunCompleted, RunCompletedPayload{Result: &result})
	foreign.CellInstance = "pod-b" // differs from the intent's "pod-a"
	if _, err := g.Apply(context.Background(), foreign); err != nil {
		t.Fatalf("apply foreign run.completed: %v", err)
	}

	var status string
	var resultGot *string
	var foreignKey *string
	if err := db.QueryRow(`
		SELECT status, result, payload->'foreign_completion'->>'kind' FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &resultGot, &foreignKey); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != StatusRunning {
		t.Fatalf("status = %q, want RUNNING (unchanged)", status)
	}
	if resultGot != nil {
		t.Fatalf("result = %v, want nil (unchanged)", *resultGot)
	}
	if foreignKey == nil || *foreignKey != KindRunCompleted {
		t.Fatalf("foreign_completion.kind = %v, want %q", foreignKey, KindRunCompleted)
	}
}

// TestApplyRunCompletedFillsAbsentRevision asserts a completion writes
// the newest changelog entry's revision when the row holds none.
func TestApplyRunCompletedFillsAbsentRevision(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:completed-fill")
	g := NewIngester(pool, nil, nil)

	buildNumber := int64(1)
	started := newTestEnvelope(t, runID, "manual:completed-fill", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	result := "SUCCESS"
	revision := "changelog-rev"
	completed := newTestEnvelope(t, runID, "manual:completed-fill", KindRunCompleted, RunCompletedPayload{
		Result:      &result,
		SCMRevision: &revision,
	})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	var revisionGot *string
	if err := db.QueryRow(`SELECT scm_revision FROM varroa.runs WHERE run_id = $1`, runID).Scan(&revisionGot); err != nil {
		t.Fatalf("read scm_revision: %v", err)
	}
	if revisionGot == nil || *revisionGot != revision {
		t.Fatalf("scm_revision = %v, want %q", revisionGot, revision)
	}
}

// TestApplyRunCompletedKeepsSetRevision asserts a completion never
// replaces a revision the row already holds.
func TestApplyRunCompletedKeepsSetRevision(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:completed-keep")
	g := NewIngester(pool, nil, nil)

	buildNumber := int64(1)
	claimRevision := "claim-rev"
	started := newTestEnvelope(t, runID, "manual:completed-keep", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildNumber,
		SCMRevision: &claimRevision,
	})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	result := "SUCCESS"
	changelogRevision := "changelog-rev"
	completed := newTestEnvelope(t, runID, "manual:completed-keep", KindRunCompleted, RunCompletedPayload{
		Result:      &result,
		SCMRevision: &changelogRevision,
	})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	var revisionGot *string
	if err := db.QueryRow(`SELECT scm_revision FROM varroa.runs WHERE run_id = $1`, runID).Scan(&revisionGot); err != nil {
		t.Fatalf("read scm_revision: %v", err)
	}
	if revisionGot == nil || *revisionGot != claimRevision {
		t.Fatalf("scm_revision = %v, want %q", revisionGot, claimRevision)
	}
}

// TestApplyRunCompletedReportsReleasedLocks asserts the owner write
// reports one entry per fleet lock it deleted, each naming that row's
// own values, and that a run holding two nested locks reports both.
func TestApplyRunCompletedReportsReleasedLocks(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:completed-release")
	g := NewIngester(pool, nil, nil)

	now := time.Now()
	insertFleetLockRow(t, db, "alpha/deploy-prod", runID.String(), FleetLockStateHeld,
		now.Add(-time.Minute), now.Add(5*time.Minute))
	insertFleetLockRow(t, db, "alpha/deploy-gate", runID.String(), FleetLockStateHeld,
		now.Add(-time.Minute), now.Add(5*time.Minute))

	completed := newTestEnvelope(t, runID, "manual:completed-release", KindRunCompleted,
		RunCompletedPayload{Result: strPtr("SUCCESS")})
	res, err := g.Apply(context.Background(), completed)
	if err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	if len(res.ReleasedFleetLocks) != 2 {
		t.Fatalf("released locks = %+v, want one entry per deleted row", res.ReleasedFleetLocks)
	}
	names := map[string]string{}
	for _, lock := range res.ReleasedFleetLocks {
		names[lock.LockName] = lock.JobPath
		if lock.Cluster != testCluster || lock.Tenant != "alpha" {
			t.Errorf("released lock coordinates = %s/%s, want %s/alpha", lock.Cluster, lock.Tenant, testCluster)
		}
	}
	for _, want := range []string{"alpha/deploy-prod", "alpha/deploy-gate"} {
		if names[want] != "alpha/release" {
			t.Errorf("released lock %s = %q, want the deleted row's own job path", want, names[want])
		}
	}
}

// TestApplyRunCompletedForeignInstanceReportsNoReleasedLock asserts a
// foreign completion releases nothing and reports no entry.
func TestApplyRunCompletedForeignInstanceReportsNoReleasedLock(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:completed-foreign-lock")
	g := NewIngester(pool, nil, nil)

	now := time.Now()
	insertFleetLockRow(t, db, "alpha/deploy-prod", runID.String(), FleetLockStateHeld,
		now.Add(-time.Minute), now.Add(5*time.Minute))

	completed := newTestEnvelope(t, runID, "manual:completed-foreign-lock", KindRunCompleted,
		RunCompletedPayload{Result: strPtr("SUCCESS")})
	completed.CellInstance = "pod-other"
	res, err := g.Apply(context.Background(), completed)
	if err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	if len(res.ReleasedFleetLocks) != 0 {
		t.Fatalf("released locks = %+v, want none for a foreign completion", res.ReleasedFleetLocks)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2`,
		"alpha/deploy-prod", runID.String()).Scan(&state); err != nil {
		t.Fatalf("read fleet lock row: %v", err)
	}
	if state != FleetLockStateHeld {
		t.Fatalf("state = %q, want the row held: a foreign completion releases nothing", state)
	}
}

// --- summary: the run.completed merge ---

// TestApplyRunCompletedRaisesTheSummaryAndItsWatermark proves a
// completion whose payload carries a summary value writes both the
// summary column and the payload's summary_seq key, and that a seq above
// a prior heartbeat's watermark wins.
func TestApplyRunCompletedRaisesTheSummaryAndItsWatermark(t *testing.T) {
	pool, db := setupLedgerPool(t)
	const intentKey = "manual:completed-summary-1"
	runID := seedGrantedIntent(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	startRunForSummary(t, pool, runID, intentKey)

	applySummaryHeartbeat(t, pool, runID, intentKey, map[string]any{"summary": "from the heartbeat", "seq": int64(3)})

	completed := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{
		Result:  strPtr("FAILURE"),
		Summary: strPtr("from the completion"),
		Seq:     5,
	})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	text, seq := readRunSummary(t, db, runID)
	if !text.Valid || text.String != "from the completion" {
		t.Fatalf("summary = %v, want the completion's text", text)
	}
	if !seq.Valid || seq.String != "5" {
		t.Fatalf("summary_seq = %v, want 5", seq)
	}
}

// TestApplyRunCompletedStaleSummaryCannotLowerTheWatermark proves a
// completion whose seq sits below the stored summary_seq, and one from a
// non-owning instance, leave both the summary column and the watermark at
// their stored values.
func TestApplyRunCompletedStaleSummaryCannotLowerTheWatermark(t *testing.T) {
	pool, db := setupLedgerPool(t)
	const intentKey = "manual:completed-summary-stale-1"
	runID := seedGrantedIntent(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	startRunForSummary(t, pool, runID, intentKey)

	newer := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{
		Result:  strPtr("FAILURE"),
		Summary: strPtr("newer line"),
		Seq:     9,
	})
	if _, err := g.Apply(context.Background(), newer); err != nil {
		t.Fatalf("apply the newer completion: %v", err)
	}

	stale := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{
		Result:  strPtr("ABORTED"),
		Summary: strPtr("older line"),
		Seq:     2,
	})
	stale.CellInstance = "pod-b" // differs from the intent's "pod-a"
	if _, err := g.Apply(context.Background(), stale); err != nil {
		t.Fatalf("apply the stale completion: %v", err)
	}

	text, seq := readRunSummary(t, db, runID)
	if !text.Valid || text.String != "newer line" {
		t.Fatalf("summary = %v, want the newer line", text)
	}
	if !seq.Valid || seq.String != "9" {
		t.Fatalf("summary_seq = %v, want 9", seq)
	}
}

// TestApplyRunCompletedWithoutASummaryWritesNoWatermark proves a
// completion whose payload carries no summary value writes neither the
// summary column nor a summary_seq key.
func TestApplyRunCompletedWithoutASummaryWritesNoWatermark(t *testing.T) {
	pool, db := setupLedgerPool(t)
	const intentKey = "manual:completed-summary-absent-1"
	runID := seedGrantedIntent(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	startRunForSummary(t, pool, runID, intentKey)

	completed := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{
		Result: strPtr("FAILURE"),
		Seq:    4,
	})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	text, seq := readRunSummary(t, db, runID)
	if text.Valid {
		t.Fatalf("summary = %v, want null", text)
	}
	if seq.Valid {
		t.Fatalf("summary_seq = %v, want null", seq)
	}
	var hasKey bool
	if err := db.QueryRow(`SELECT payload ? 'summary_seq' FROM varroa.runs WHERE run_id = $1`, runID).Scan(&hasKey); err != nil {
		t.Fatalf("read the payload: %v", err)
	}
	if hasKey {
		t.Fatal("the payload carries a summary_seq key for a completion with no summary value")
	}
}

// TestApplyRunCompletedInsertsTheSummaryForANewRow proves a completion
// that inserts the row, because no earlier event wrote one, writes the
// summary text and the summary_seq key in that same insert.
func TestApplyRunCompletedInsertsTheSummaryForANewRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	const intentKey = "manual:completed-summary-insert-1"
	runID := seedGrantedIntent(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)

	// No run.started and no heartbeat: the completion itself inserts the
	// runs row.
	completed := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{
		Result:  strPtr("FAILURE"),
		Summary: strPtr("summary on the insert path"),
		Seq:     7,
	})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	text, seq := readRunSummary(t, db, runID)
	if !text.Valid || text.String != "summary on the insert path" {
		t.Fatalf("summary = %v, want the completion's text", text)
	}
	if !seq.Valid || seq.String != "7" {
		t.Fatalf("summary_seq = %v, want 7", seq)
	}
}
