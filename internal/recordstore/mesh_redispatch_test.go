package recordstore

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/metric/noop"
)

// meshRolledInstance is the instance a replacement pod reports after the
// first target failed the resume. A re-dispatch needs an instance that
// is new, so a pod must roll or a job home must move.
const meshRolledInstance = "pod-live-2"

// movePlacementInstance rewrites the tenant's live instance and its
// rehydration seed, as a rolled cell pod reports them.
func movePlacementInstance(t *testing.T, db *sql.DB, instance string) {
	t.Helper()
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET cell_instance = $1, rehydrated_instance = $1
		 WHERE cluster = $2 AND tenant = $3
	`, instance, testCluster, testTenant); err != nil {
		t.Fatalf("move placement instance: %v", err)
	}
}

// failResumeOn applies one run.resume_failed envelope naming instance,
// and asserts the recorder returned the run to the armed state.
func failResumeOn(t *testing.T, g *Ingester, runID uuid.UUID, instance string) {
	t.Helper()
	res := applyResumeFailedEnvelope(t, g,
		newResumeFailedEnvelope(t, runID.String(), instance, "RESUME_FAILED"))
	if !res.ResumeFailedCleared {
		t.Fatalf("the failure record for %s did not re-arm the run", instance)
	}
}

// TestTakeOverLostRunsEndsArmBeforeAReplacementRolls asserts a resume the
// only home target could not apply ends the run's arm at once. The next
// round names the failed target, so it ends the arm and dispatches
// nothing. A replacement that rolls later finds the arm already ended,
// so the sweep marks the run resume-failed instead of waiting for it.
func TestTakeOverLostRunsEndsArmBeforeAReplacementRolls(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestActionConn(t)
	received := collectActions(t, conn)
	g := NewIngester(pool, nil, nil)

	runID := seedMeshRunningRun(t, pool, db)
	seedMeshFleetLock(t, db, runID)
	seedMeshPlacement(t, db, resumeFailedTarget, resumeFailedTarget)

	metrics, err := NewInterruptedMetrics(noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	actionMetrics, err := NewActionMetrics(noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("new action metrics: %v", err)
	}

	// Round one arms the run and dispatches the resume to the first
	// target the tenant reports.
	if got := runTakeover(t, pool, conn, metrics, actionMetrics); got != 1 {
		t.Fatalf("first round published = %d, want 1", got)
	}
	first := waitForAction(t, received)
	if first.Instance != resumeFailedTarget {
		t.Fatalf("first action names %q, want %s", first.Instance, resumeFailedTarget)
	}

	// The target fails the command. The gateway records that failure.
	failResumeOn(t, g, runID, resumeFailedTarget)

	// The failed instance is the only home candidate, so this round
	// dispatches nothing and ends the arm.
	if got := runTakeover(t, pool, conn, metrics, actionMetrics); got != 0 {
		t.Fatalf("round after the failure published = %d, want 0", got)
	}
	_, endedExpires, _ := readMeshResume(t, db, runID)
	armed, err := time.Parse(time.RFC3339Nano, endedExpires)
	if err != nil {
		t.Fatalf("parse arm_expires %q: %v", endedExpires, err)
	}
	if armed.After(time.Now()) {
		t.Fatalf("arm_expires = %v, want the arm ended at the refusal", armed)
	}

	// The pod rolls, and the tenant reports a new seeded instance. The
	// ended arm dispatches nothing to it.
	movePlacementInstance(t, db, meshRolledInstance)
	if got := runTakeover(t, pool, conn, metrics, actionMetrics); got != 0 {
		t.Fatalf("round after the roll published = %d, want 0: the arm has ended", got)
	}
	_, _, gotInstance := readMeshResume(t, db, runID)
	if gotInstance != "" {
		t.Fatalf("instance = %q, want no dispatch", gotInstance)
	}

	// The failed instance stays on the list for the rest of the record.
	if got := readResumeFailedList(t, db, runID); len(got) != 1 || got[0] != resumeFailedTarget {
		t.Fatalf("failed list = %v, want [%s]", got, resumeFailedTarget)
	}
}

// TestTakeOverLostRunsSkipsExcludedInstance asserts a home slot that
// still names the failed instance is never picked again, and that the
// refusal is counted apart from a tenant with no replacement at all.
func TestTakeOverLostRunsSkipsExcludedInstance(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestActionConn(t)
	mp, reader := newTestMeterProvider(t)
	g := NewIngester(pool, nil, nil)

	runID := seedMeshRunningRun(t, pool, db)
	seedMeshPlacement(t, db, resumeFailedTarget, resumeFailedTarget)

	metrics, err := NewInterruptedMetrics(mp)
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	if got := runTakeover(t, pool, conn, metrics, nil); got != 1 {
		t.Fatalf("first round published = %d, want 1", got)
	}
	failResumeOn(t, g, runID, resumeFailedTarget)

	if got := runTakeover(t, pool, conn, metrics, nil); got != 0 {
		t.Fatalf("published = %d, want 0: the only candidate already failed", got)
	}
	if got := sumCounterByAttr(t, reader, "recorder_run_resume_total",
		"reason", resumeReasonTargetFailed); got != 1 {
		t.Fatalf("recorder_run_resume_total{reason=target-failed} = %d, want 1", got)
	}
}

// waitForAction reads one published ledger action, or fails the test.
func waitForAction(t *testing.T, received chan LedgerAction) LedgerAction {
	t.Helper()
	select {
	case a := <-received:
		return a
	case <-time.After(5 * time.Second):
		t.Fatal("no ledger action published")
		return LedgerAction{}
	}
}
