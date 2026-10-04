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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:completed-foreign-lock")
	g := NewIngester(pool, nil, nil)

	now := time.Now()
	insertFleetLockRow(t, db, "alpha/deploy-prod", runID.String(), FleetLockStateHeld,
		now.Add(-time.Minute), now.Add(5*time.Minute))
	insertFleetLockRow(t, db, "alpha/deploy-gate", runID.String(), FleetLockStateWaiting,
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
	if err := db.QueryRow(`SELECT state FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2`,
		"alpha/deploy-gate", runID.String()).Scan(&state); err != nil {
		t.Fatalf("read waiting fleet lock row: %v", err)
	}
	if state != FleetLockStateWaiting {
		t.Fatalf("state = %q, want the row waiting: a foreign completion deletes no waiting row", state)
	}
}

// TestApplyRunCompletedDeletesWaitingLockRows asserts the owner's
// terminal run.completed deletes the run's waiting rows beside its held
// row, and reports one entry per deleted row.
func TestApplyRunCompletedDeletesWaitingLockRows(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:completed-waiting")
	g := NewIngester(pool, nil, nil)

	now := time.Now()
	insertFleetLockRow(t, db, "alpha/deploy-prod", runID.String(), FleetLockStateHeld,
		now.Add(-time.Minute), now.Add(5*time.Minute))
	insertFleetLockRow(t, db, "alpha/deploy-gate", runID.String(), FleetLockStateWaiting,
		now.Add(-time.Minute), now.Add(5*time.Minute))

	completed := newTestEnvelope(t, runID, "manual:completed-waiting", KindRunCompleted,
		RunCompletedPayload{Result: strPtr("FAILURE")})
	res, err := g.Apply(context.Background(), completed)
	if err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	if len(res.ReleasedFleetLocks) != 2 {
		t.Fatalf("released locks = %+v, want the held and the waiting row", res.ReleasedFleetLocks)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.fleet_locks WHERE run_id = $1`, runID.String()).Scan(&n); err != nil {
		t.Fatalf("count fleet lock rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d fleet lock rows left, want none for a completed run", n)
	}
}

// TestApplyRunCompletedKeepsOtherRunsWaitingRows asserts the delete
// matches the completed run alone: another run's waiting row on the same
// lock stays.
func TestApplyRunCompletedKeepsOtherRunsWaitingRows(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:completed-other-waiter")
	g := NewIngester(pool, nil, nil)

	now := time.Now()
	other := newTestUUIDv7String()
	insertFleetLockRow(t, db, "alpha/deploy-prod", runID.String(), FleetLockStateHeld,
		now.Add(-2*time.Minute), now.Add(5*time.Minute))
	insertFleetLockRow(t, db, "alpha/deploy-prod", other, FleetLockStateWaiting,
		now.Add(-time.Minute), now.Add(5*time.Minute))

	completed := newTestEnvelope(t, runID, "manual:completed-other-waiter", KindRunCompleted,
		RunCompletedPayload{Result: strPtr("SUCCESS")})
	res, err := g.Apply(context.Background(), completed)
	if err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	if len(res.ReleasedFleetLocks) != 1 {
		t.Fatalf("released locks = %+v, want the completed run's held row only", res.ReleasedFleetLocks)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2`,
		"alpha/deploy-prod", other).Scan(&state); err != nil {
		t.Fatalf("read the other run's row: %v", err)
	}
	if state != FleetLockStateWaiting {
		t.Fatalf("state = %q, want the other run's row still waiting", state)
	}
}

// --- summary: the run.completed merge ---

// TestApplyRunCompletedRaisesTheSummaryAndItsWatermark proves a
// completion whose payload carries a summary value writes both the
// summary column and the payload's summary_seq key, and that a seq above
// a prior heartbeat's watermark wins.
func TestApplyRunCompletedRaisesTheSummaryAndItsWatermark(t *testing.T) {
	t.Parallel()

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

// TestApplyRunCompletedClearsThePendingSet proves an owning completion
// clears the run's pending set and raises the inputs watermark, so a later
// run.input changes nothing.
func TestApplyRunCompletedClearsThePendingSet(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	const intentKey = "manual:completed-inputs-1"
	runID := seedGrantedIntent(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	startRunForSummary(t, pool, runID, intentKey)

	pending := newTestEnvelope(t, runID, intentKey, KindRunInput,
		RunInputPayload{Seq: inputSeqPtr(3), Inputs: runInputSet("input-1")})
	if _, err := g.Apply(context.Background(), pending); err != nil {
		t.Fatalf("apply run.input: %v", err)
	}

	completed := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{
		Result: strPtr("SUCCESS"),
		Seq:    5,
	})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	inputs, inputsSeq := readRunInputsRow(t, db, runID)
	if inputs != "" {
		t.Fatalf("stored inputs = %q, want NULL after the completion", inputs)
	}
	if inputsSeq != "5" {
		t.Fatalf("inputs_seq = %q, want 5", inputsSeq)
	}

	late := newTestEnvelope(t, runID, intentKey, KindRunInput,
		RunInputPayload{Seq: inputSeqPtr(9), Inputs: runInputSet("input-9")})
	res, err := g.Apply(context.Background(), late)
	if err != nil {
		t.Fatalf("apply late run.input: %v", err)
	}
	if !res.RefusedInput {
		t.Fatal("a completed run took a pending set, want refused")
	}
}

// TestHeartbeatOnACompletedRunRestoresNoPendingSet proves a heartbeat that
// races an owning completion cannot restore the set the completion
// cleared, whatever seq the heartbeat carries.
func TestHeartbeatOnACompletedRunRestoresNoPendingSet(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	const intentKey = "manual:completed-inputs-2"
	runID := seedGrantedIntent(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	startRunForSummary(t, pool, runID, intentKey)

	completed := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{
		Result: strPtr("SUCCESS"),
		Seq:    5,
	})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	applyHeartbeatWithInputs(t, pool, runID, 9, []StoredRunInput{{ID: "input-9"}})

	inputs, inputsSeq := readRunInputsRow(t, db, runID)
	if inputs != "" {
		t.Fatalf("stored inputs = %q, want NULL on a completed run", inputs)
	}
	if inputsSeq != "5" {
		t.Fatalf("inputs_seq = %q, want the completion's 5", inputsSeq)
	}
}

// TestApplyRunCompletedForeignInstanceKeepsThePendingSet proves a foreign
// completion clears nothing and raises nothing.
func TestApplyRunCompletedForeignInstanceKeepsThePendingSet(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	const intentKey = "manual:completed-inputs-foreign-1"
	runID := seedGrantedIntent(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	startRunForSummary(t, pool, runID, intentKey)

	pending := newTestEnvelope(t, runID, intentKey, KindRunInput,
		RunInputPayload{Seq: inputSeqPtr(3), Inputs: runInputSet("input-1")})
	if _, err := g.Apply(context.Background(), pending); err != nil {
		t.Fatalf("apply run.input: %v", err)
	}

	foreign := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{
		Result: strPtr("SUCCESS"),
		Seq:    5,
	})
	foreign.CellInstance = "pod-b" // differs from the intent's own instance
	if _, err := g.Apply(context.Background(), foreign); err != nil {
		t.Fatalf("apply the foreign run.completed: %v", err)
	}

	inputs, inputsSeq := readRunInputsRow(t, db, runID)
	if entries := storedRunInputs(t, inputs); len(entries) != 1 || entries[0].ID != "input-1" {
		t.Fatalf("stored inputs = %+v, want the stored set", entries)
	}
	if inputsSeq != "3" {
		t.Fatalf("inputs_seq = %q, want 3", inputsSeq)
	}
}

// TestApplyRunCompletedStaleSummaryCannotLowerTheWatermark proves a
// completion whose seq sits below the stored summary_seq, and one from a
// non-owning instance, leave both the summary column and the watermark at
// their stored values.
func TestApplyRunCompletedStaleSummaryCannotLowerTheWatermark(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
