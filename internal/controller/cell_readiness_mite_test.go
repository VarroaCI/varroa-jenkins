package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// streamLostCell returns a cell that passed every gate and lost its mite
// stream: phase Running and MiteConnected=False/StreamLost, so its
// CellReady reason is MiteUnreachable.
func streamLostCell(t *testing.T, grace time.Duration) (*Reconciler, *testClient, *v1alpha1.Controller) {
	t.Helper()
	rec, client, cr := readyGateCell(t, grace)
	cr.Status.Phase = v1alpha1.ControllerPhaseRunning
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionMiteConnected,
		Status:  metav1.ConditionFalse,
		Reason:  v1alpha1.ReasonStreamLost,
		Message: "mite ns/cell1 command stream lost: no presence record; last heartbeat 2026-01-01T00:00:00Z",
	})
	return rec, client, cr
}

// runningCellPod returns a cell pod whose init containers finished long
// before start and whose main containers run. ready sets its Ready
// condition.
func runningCellPod(start time.Time, ready bool) *corev1.Pod {
	readyStatus := corev1.ConditionFalse
	if ready {
		readyStatus = corev1.ConditionTrue
	}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Time{Time: start}}}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "cell1-abcd1234-0", Namespace: "ns", UID: types.UID("pod-uid-1")},
		Status: corev1.PodStatus{
			StartTime:  &metav1.Time{Time: start},
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: readyStatus}},
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "plugins-init",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 0, FinishedAt: metav1.Time{Time: start},
				}},
			}},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "jenkins", State: running, Ready: ready},
				{Name: "mite", State: running, Ready: true},
			},
		},
	}
}

func TestObserveInitReadsPodReady(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if obs := observeInit(runningCellPod(now.Add(-time.Hour), true), now, nil); !obs.Ready {
		t.Error("expected Ready for a pod whose Ready condition is True")
	}
	if obs := observeInit(runningCellPod(now.Add(-time.Hour), false), now, nil); obs.Ready {
		t.Error("expected not Ready for a pod whose Ready condition is False")
	}
	terminating := runningCellPod(now.Add(-time.Hour), true)
	terminating.DeletionTimestamp = &metav1.Time{Time: now}
	if obs := observeInit(terminating, now, nil); obs.Ready {
		t.Error("expected not Ready for a terminating pod")
	}
}

// TestCellRollHoldsForReadyPod covers the second fact: a lost presence
// record alone does not delete a Ready pod. The hold also keeps evacuate
// unset. Past cellMiteRollHoldCap the roll goes, and evacuate waits one
// more grace after the roll.
func TestCellRollHoldsForReadyPod(t *testing.T) {
	grace := 5 * time.Minute
	rec, client, cr := streamLostCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client.controllerPod = runningCellPod(t0.Add(-time.Hour), true)

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cond.Reason != v1alpha1.ReasonMiteUnreachable {
		t.Fatalf("tick1: Reason = %q, want MiteUnreachable", cond.Reason)
	}

	for _, at := range []time.Duration{grace + time.Minute, 2*grace + time.Minute, cellMiteRollHoldCap - time.Minute} {
		rec.evaluateCellReadiness(context.Background(), cr, t0.Add(at))
		if client.deleteControllerPodCalls != 0 {
			t.Fatalf("at %v: expected no delete for a Ready pod, got %d", at, client.deleteControllerPodCalls)
		}
		cond = findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
		if cond.Reason != v1alpha1.ReasonMiteUnreachable || !strings.HasPrefix(cond.Message, cellRollHeldPrefix) {
			t.Fatalf("at %v: condition = %s/%q, want MiteUnreachable with a held message", at, cond.Reason, cond.Message)
		}
		if !strings.Contains(cond.Message, "cell1-abcd1234-0 is Ready") {
			t.Fatalf("at %v: message %q does not name the Ready pod", at, cond.Message)
		}
		if cr.Status.Cell.RolledAt != nil {
			t.Fatalf("at %v: expected no rolledAt while held", at)
		}
		if cr.Status.Cell.Evacuate {
			t.Fatalf("at %v: expected no evacuate while the roll is held", at)
		}
	}

	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(cellMiteRollHoldCap))
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("past the hold cap: expected one delete, got %d", client.deleteControllerPodCalls)
	}
	cond = findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cond.Reason != v1alpha1.ReasonCellRolled || cond.Message != v1alpha1.ReasonMiteUnreachable {
		t.Fatalf("past the hold cap: condition = %s/%q, want CellRolled/MiteUnreachable", cond.Reason, cond.Message)
	}
	if cr.Status.Cell.Evacuate {
		t.Fatal("past the hold cap: expected no evacuate in the tick of the roll")
	}

	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(cellMiteRollHoldCap+grace-time.Second))
	if cr.Status.Cell.Evacuate {
		t.Fatal("expected no evacuate less than one grace after the roll")
	}
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(cellMiteRollHoldCap+grace))
	if !cr.Status.Cell.Evacuate {
		t.Fatal("expected evacuate one grace after the roll")
	}
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("expected still one delete, got %d", client.deleteControllerPodCalls)
	}
}

// TestCellEvacuateOneGraceAfterTimelyRoll pins the timeline of a pod that
// is not Ready: the roll at the grace, evacuate at twice the grace.
func TestCellEvacuateOneGraceAfterTimelyRoll(t *testing.T) {
	grace := 5 * time.Minute
	rec, client, cr := streamLostCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client.controllerPod = runningCellPod(t0.Add(-time.Hour), false)

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace))
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("expected the roll at the grace, got %d deletes", client.deleteControllerPodCalls)
	}
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(2*grace-time.Second))
	if cr.Status.Cell.Evacuate {
		t.Fatal("expected no evacuate before twice the grace")
	}
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(2*grace))
	if !cr.Status.Cell.Evacuate {
		t.Fatal("expected evacuate at twice the grace")
	}
}

// TestCellRollGraceLongerThanHoldCap asserts that the deadline is the grace
// when a pool sets a grace longer than cellMiteRollHoldCap. The hold then
// never applies, and the roll goes at the grace.
func TestCellRollGraceLongerThanHoldCap(t *testing.T) {
	grace := cellMiteRollHoldCap + 15*time.Minute
	rec, client, cr := streamLostCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client.controllerPod = runningCellPod(t0.Add(-time.Hour), true)

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace-time.Minute))
	if client.deleteControllerPodCalls != 0 {
		t.Fatalf("expected no delete before the grace, got %d", client.deleteControllerPodCalls)
	}
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace))
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("expected one delete at the grace, got %d", client.deleteControllerPodCalls)
	}
}

func TestCellRollGoesForNotReadyPod(t *testing.T) {
	grace := 5 * time.Minute
	rec, client, cr := streamLostCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client.controllerPod = runningCellPod(t0.Add(-time.Hour), false)

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace+time.Minute))
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("expected one delete for a pod that is not Ready, got %d", client.deleteControllerPodCalls)
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cond.Reason != v1alpha1.ReasonCellRolled {
		t.Fatalf("Reason = %q, want CellRolled", cond.Reason)
	}
}

func TestCellRollHoldsOnPodReadFailure(t *testing.T) {
	grace := 5 * time.Minute
	rec, client, cr := streamLostCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client.controllerPodErr = errors.New("apiserver unavailable")

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace+time.Minute))
	if client.deleteControllerPodCalls != 0 {
		t.Fatalf("expected no delete after a failed pod read, got %d", client.deleteControllerPodCalls)
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if !strings.Contains(cond.Message, "pod read failed") {
		t.Fatalf("message %q does not name the failed pod read", cond.Message)
	}

	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(cellMiteRollHoldCap))
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("past the hold cap: expected one delete, got %d", client.deleteControllerPodCalls)
	}
}

// TestCellRollNotProvisionedIgnoresPodReady keeps the roll for a cell that
// never connected on the unready clock alone: the hold applies to
// MiteUnreachable only.
func TestCellRollNotProvisionedIgnoresPodReady(t *testing.T) {
	grace := 5 * time.Minute
	rec, client, cr := readyGateCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client.controllerPod = runningCellPod(t0.Add(-time.Hour), true)

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace+time.Minute))
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("expected one delete for NotProvisioned, got %d", client.deleteControllerPodCalls)
	}
}

// TestCellRollClockWaitsAfterPresenceFault covers the doubtful read: a
// failed presence read of the cell restarts its MiteUnreachable roll clock.
func TestCellRollClockWaitsAfterPresenceFault(t *testing.T) {
	grace := 10 * time.Minute
	rec, client, cr := streamLostCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client.controllerPod = runningCellPod(t0.Add(-time.Hour), false)

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	fault := t0.Add(8 * time.Minute)
	rec.notePresenceFault("ns/cell1", fault)

	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace+time.Minute))
	if client.deleteControllerPodCalls != 0 {
		t.Fatalf("expected no delete inside the grace after a presence fault, got %d", client.deleteControllerPodCalls)
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if !strings.Contains(cond.Message, "presence read failed") {
		t.Fatalf("message %q does not name the presence fault", cond.Message)
	}

	rec.evaluateCellReadiness(context.Background(), cr, fault.Add(grace))
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("expected one delete one grace after the presence fault, got %d", client.deleteControllerPodCalls)
	}
}

// TestCellRollPresenceFaultOfOtherCellKeepsClock asserts that a failed
// presence read of one controller does not move the roll clock of another.
func TestCellRollPresenceFaultOfOtherCellKeepsClock(t *testing.T) {
	grace := 10 * time.Minute
	rec, client, cr := streamLostCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client.controllerPod = runningCellPod(t0.Add(-time.Hour), false)

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	rec.notePresenceFault("ns/other-cell", t0.Add(8*time.Minute))
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace))
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("expected the roll at the grace, got %d deletes", client.deleteControllerPodCalls)
	}
}

// TestCellRollPresenceFaultTrickleRollsAtDeadline is the steady-fault case:
// a pod that is not Ready and one failed presence read each minute for four
// hours. The faults delay the roll to the deadline and no further, and
// evacuate follows one grace after the roll.
func TestCellRollPresenceFaultTrickleRollsAtDeadline(t *testing.T) {
	grace := 5 * time.Minute
	rec, client, cr := streamLostCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client.controllerPod = runningCellPod(t0.Add(-time.Hour), false)

	var rolledAt, evacuatedAt time.Duration = -1, -1
	for m := 0; m <= 240; m++ {
		at := time.Duration(m) * time.Minute
		now := t0.Add(at)
		rec.notePresenceFault("ns/cell1", now)
		rec.evaluateCellReadiness(context.Background(), cr, now)
		if rolledAt < 0 && client.deleteControllerPodCalls > 0 {
			rolledAt = at
		}
		if evacuatedAt < 0 && cr.Status.Cell.Evacuate {
			evacuatedAt = at
		}
	}
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("deletes = %d, want exactly one", client.deleteControllerPodCalls)
	}
	if rolledAt != cellMiteRollHoldCap {
		t.Fatalf("rolled at %v, want the deadline %v", rolledAt, cellMiteRollHoldCap)
	}
	if evacuatedAt != cellMiteRollHoldCap+grace {
		t.Fatalf("evacuated at %v, want %v", evacuatedAt, cellMiteRollHoldCap+grace)
	}
}

// TestCellRollReadyPodAndFaultTrickleRollsAtDeadline combines the two
// delays: a Ready pod and a steady presence fault together still roll at
// the deadline.
func TestCellRollReadyPodAndFaultTrickleRollsAtDeadline(t *testing.T) {
	grace := 5 * time.Minute
	rec, client, cr := streamLostCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client.controllerPod = runningCellPod(t0.Add(-time.Hour), true)

	for m := 0; m < int(cellMiteRollHoldCap/time.Minute); m++ {
		now := t0.Add(time.Duration(m) * time.Minute)
		rec.notePresenceFault("ns/cell1", now)
		rec.evaluateCellReadiness(context.Background(), cr, now)
	}
	if client.deleteControllerPodCalls != 0 {
		t.Fatalf("expected no delete before the deadline, got %d", client.deleteControllerPodCalls)
	}
	deadline := t0.Add(cellMiteRollHoldCap)
	rec.notePresenceFault("ns/cell1", deadline)
	rec.evaluateCellReadiness(context.Background(), cr, deadline)
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("expected one delete at the deadline, got %d", client.deleteControllerPodCalls)
	}
}

// TestCellRollDeadlineKeepsNewPodGrace asserts that the deadline does not
// cut the grace of a new pod: a pod that finished init after the deadline
// still gets a full grace from its init finish.
func TestCellRollDeadlineKeepsNewPodGrace(t *testing.T) {
	grace := 5 * time.Minute
	rec, client, cr := streamLostCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	initDone := t0.Add(cellMiteRollHoldCap + 10*time.Minute)
	client.controllerPod = runningCellPod(initDone, false)

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	rec.evaluateCellReadiness(context.Background(), cr, initDone.Add(time.Minute))
	if client.deleteControllerPodCalls != 0 {
		t.Fatalf("expected no delete one minute after the init finish, got %d", client.deleteControllerPodCalls)
	}
	rec.evaluateCellReadiness(context.Background(), cr, initDone.Add(grace))
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("expected one delete one grace after the init finish, got %d", client.deleteControllerPodCalls)
	}
}

func TestNotePresenceFaultKeepsNewestPerKey(t *testing.T) {
	rec := &Reconciler{}
	if !rec.lastPresenceFault("ns/a").IsZero() {
		t.Fatal("expected no presence fault on a new reconciler")
	}
	newer := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	rec.notePresenceFault("ns/a", newer)
	rec.notePresenceFault("ns/a", newer.Add(-time.Minute))
	if got := rec.lastPresenceFault("ns/a"); !got.Equal(newer) {
		t.Fatalf("lastPresenceFault(ns/a) = %v, want %v", got, newer)
	}
	if !rec.lastPresenceFault("ns/b").IsZero() {
		t.Fatal("expected no presence fault for another key")
	}
	rec.notePresenceFault("ns/b", newer.Add(presenceFaultKeep+time.Second))
	if !rec.lastPresenceFault("ns/a").IsZero() {
		t.Fatal("expected a fault older than presenceFaultKeep to be dropped")
	}
}

func TestCellRollCauseNamesRuleAndHeartbeatAge(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC)
	_, _, cr := streamLostCell(t, 5*time.Minute)
	cr.Status.MiteStatus = &v1alpha1.MiteStatus{LastSeen: &metav1.Time{Time: now.Add(-11 * time.Minute)}}

	got := cellRollCause(cr, v1alpha1.ReasonMiteUnreachable, 5*time.Minute, now)
	for _, want := range []string{"MiteUnreachable for 5m0s", "no presence record", "last mite heartbeat seen 11m0s ago"} {
		if !strings.Contains(got, want) {
			t.Errorf("cellRollCause = %q, want it to contain %q", got, want)
		}
	}
}

// TestReconcileCellPresenceFaultFeedsRollClock runs whole reconcile passes.
// The failed read in handleConnected marks the stream lost, and the same
// pass sets MiteUnreachable. A later failed read moves the roll clock of the
// cell. Once the fault is older than unreadySince, the roll goes.
func TestReconcileCellPresenceFaultFeedsRollClock(t *testing.T) {
	ctx := context.Background()
	grace := 5 * time.Minute
	client := newTestClientWithBundle()
	client.existingSecrets = map[string]map[string][]byte{
		"abase-test": {"access-key-id": []byte("a"), "secret-access-key": []byte("b")},
	}
	rec := newTestReconciler(client)
	rec.SetClaimGate("1")
	rec.SetArtifactSecretBase("abase")
	rec.SetBlobstoreURL("s3://my-bucket?region=us-east-1")
	rec.cellsConfig = CellsConfig{UnreadyGracePeriod: grace}
	base := rec.miteTransport
	rec.miteTransport = &presenceReadErrTransport{Transport: base}

	cr := testController("test", "ns", v1alpha1.ControllerPhaseConnected)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	pinnedProfile(client, cr)
	seedClientCRDs(client)
	seen := metav1.NewTime(time.Now().Add(-staleMiteThreshold - time.Minute))
	cr.Status.MiteStatus = &v1alpha1.MiteStatus{Connected: true, LastSeen: &seen}
	// A miss pass returns before the bundle resolve, so the cell keeps the
	// ArtifactStorage condition of its earlier passes.
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type: ConditionArtifactStorage, Status: metav1.ConditionTrue, Reason: "External",
	})
	rec.disconnectedTicks["ns/test"] = streamLossMisses - 1

	if err := rec.reconcileController(ctx, cr); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if cr.Status.Phase != v1alpha1.ControllerPhaseRunning || !isStreamLost(cr) {
		t.Fatalf("pass 1: phase = %s, want Running with StreamLost", cr.Status.Phase)
	}
	if rec.lastPresenceFault("ns/test").IsZero() {
		t.Fatal("pass 1: expected handleConnected to record the presence fault")
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonMiteUnreachable || cr.Status.Cell.UnreadySince == nil {
		t.Fatalf("pass 1: CellReady = %+v, want MiteUnreachable with unreadySince set", cond)
	}

	// The cell has been unready for longer than the grace, and its pod is
	// not Ready. Only the failed read of pass 2 holds the roll.
	unready := time.Now().Add(-grace - time.Minute)
	cr.Status.Cell.UnreadySince = &metav1.Time{Time: unready}
	client.controllerPod = runningCellPod(unready.Add(-time.Hour), false)
	client.stsContainerEnv = map[string]map[string]string{
		"mite": {"VARROA_CA_PEM": rec.caPEM, "VARROA_ENDPOINT": rec.varroaEndpoint},
	}
	if err := rec.reconcileController(ctx, cr); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if client.deleteControllerPodCalls != 0 {
		t.Fatalf("pass 2: expected no delete right after a presence fault, got %d", client.deleteControllerPodCalls)
	}
	cond = findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if !strings.Contains(cond.Message, "presence read failed") {
		t.Fatalf("pass 2: CellReady message %q does not name the presence fault", cond.Message)
	}

	// The reads work again, and the fault is older than unreadySince.
	rec.miteTransport = base
	rec.presenceFaults["ns/test"] = unready.Add(-time.Second)
	if err := rec.reconcileController(ctx, cr); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("pass 3: expected the roll once the fault is old, got %d deletes", client.deleteControllerPodCalls)
	}
	cond = findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cond.Reason != v1alpha1.ReasonCellRolled {
		t.Fatalf("pass 3: CellReady reason = %q, want CellRolled", cond.Reason)
	}
}

// TestCellRollHoldsForSilentOpenStream covers the open stream fact: the
// gateway holds the mite stream open, and no heartbeat reaches it. The roll
// of a pod that is not Ready waits until cellMiteRollHoldCap, and the
// message names the open stream.
func TestCellRollHoldsForSilentOpenStream(t *testing.T) {
	grace := 5 * time.Minute
	rec, client, cr := streamLostCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client.controllerPod = runningCellPod(t0.Add(-time.Hour), false)
	rec.miteTransport.(*memTransport).silent("cell1", "ns", t0.Add(-5*time.Minute))

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	for _, at := range []time.Duration{grace + time.Minute, cellMiteRollHoldCap - time.Minute} {
		rec.evaluateCellReadiness(context.Background(), cr, t0.Add(at))
		if client.deleteControllerPodCalls != 0 {
			t.Fatalf("at %v: expected no delete while the stream is open, got %d", at, client.deleteControllerPodCalls)
		}
		cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
		if !strings.HasPrefix(cond.Message, cellRollHeldPrefix) ||
			!strings.Contains(cond.Message, "the gateway holds the mite stream open with no heartbeat") {
			t.Fatalf("at %v: message %q does not name the open stream", at, cond.Message)
		}
		if cr.Status.Cell.Evacuate {
			t.Fatalf("at %v: expected no evacuate while the roll is held", at)
		}
	}

	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(cellMiteRollHoldCap))
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("past the hold cap: expected one delete, got %d", client.deleteControllerPodCalls)
	}
}

// TestCellRollHoldNamesEachFact covers a Ready pod with a silent open
// stream: the held message names both facts.
func TestCellRollHoldNamesEachFact(t *testing.T) {
	grace := 5 * time.Minute
	rec, client, cr := streamLostCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client.controllerPod = runningCellPod(t0.Add(-time.Hour), true)
	rec.miteTransport.(*memTransport).silent("cell1", "ns", t0.Add(-5*time.Minute))

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace+time.Minute))
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	for _, want := range []string{"the gateway holds the mite stream open with no heartbeat", "pod cell1-abcd1234-0 is Ready"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message %q lacks %q", cond.Message, want)
		}
	}
}

// TestCellRollGoesAfterTheGatewayStops covers a gateway replica that stops
// while it holds a silent stream. Its timer stops, and the bucket TTL
// removes the record. The open stream fact goes with the record, so the
// roll of a pod that is not Ready goes on the next tick.
func TestCellRollGoesAfterTheGatewayStops(t *testing.T) {
	grace := 5 * time.Minute
	rec, client, cr := streamLostCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client.controllerPod = runningCellPod(t0.Add(-time.Hour), false)
	mites := rec.miteTransport.(*memTransport)
	mites.silent("cell1", "ns", t0.Add(-5*time.Minute))

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace+time.Minute))
	if client.deleteControllerPodCalls != 0 {
		t.Fatalf("expected no delete while the stream is open, got %d", client.deleteControllerPodCalls)
	}

	mites.drop("cell1", "ns")
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace+2*time.Minute))
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("expected one delete after the record aged out, got %d", client.deleteControllerPodCalls)
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cond.Reason != v1alpha1.ReasonCellRolled {
		t.Fatalf("Reason = %q, want CellRolled", cond.Reason)
	}
}
