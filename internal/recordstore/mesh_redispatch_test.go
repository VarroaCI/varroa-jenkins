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

// TestTakeOverLostRunsRedispatchesAfterFailure is this change's proof.
// A resume the first target could not apply must not cost the run its
// remaining arm window: the next round dispatches to the replacement
// instance the tenant now reports.
func TestTakeOverLostRunsRedispatchesAfterFailure(t *testing.T) {
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
	_, armExpires, _ := readMeshResume(t, db, runID)

	// The target fails the command. The gateway records that failure.
	failResumeOn(t, g, runID, resumeFailedTarget)

	// The failed instance is excluded, so a round before the pod rolls
	// dispatches nothing at all.
	if got := runTakeover(t, pool, conn, metrics, actionMetrics); got != 0 {
		t.Fatalf("round before the roll published = %d, want 0", got)
	}

	// The pod rolls, and the tenant reports a new seeded instance.
	movePlacementInstance(t, db, meshRolledInstance)

	if got := runTakeover(t, pool, conn, metrics, actionMetrics); got != 1 {
		t.Fatalf("round after the roll published = %d, want 1", got)
	}
	second := waitForAction(t, received)
	if second.Kind != LedgerActionResumeRun {
		t.Fatalf("second action kind = %q, want %s", second.Kind, LedgerActionResumeRun)
	}
	if second.Instance != meshRolledInstance {
		t.Fatalf("second action names %q, want %s", second.Instance, meshRolledInstance)
	}
	if second.RunID != runID.String() {
		t.Fatalf("second action run = %q, want %s", second.RunID, runID)
	}

	// The arm window is never extended. The run holds the window it had
	// before its first target failed.
	_, gotExpires, gotInstance := readMeshResume(t, db, runID)
	if gotExpires != armExpires {
		t.Fatalf("arm_expires moved from %q to %q, want it unchanged", armExpires, gotExpires)
	}
	if gotInstance != meshRolledInstance {
		t.Fatalf("instance = %q, want %s", gotInstance, meshRolledInstance)
	}

	// The failed instance stays on the list for the rest of the window.
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
