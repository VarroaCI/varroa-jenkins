package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// initStagePod returns a cell pod that started at start and whose
// plugins-init container is in state. The main containers have not started.
func initStagePod(start time.Time, state corev1.ContainerState, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "cell1-abcd1234-0", Namespace: "ns", UID: types.UID("pod-uid-1")},
		Status: corev1.PodStatus{
			StartTime: &metav1.Time{Time: start},
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "plugins-init", State: state, RestartCount: restarts},
				{Name: "init-groovy", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "jenkins", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
			},
		},
	}
}

func runningInit(since time.Time) corev1.ContainerState {
	return corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Time{Time: since}}}
}

func TestCellRollHoldsWhileInitProgressing(t *testing.T) {
	grace := 10 * time.Minute
	rec, client, cr := readyGateCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client.controllerPod = initStagePod(t0, runningInit(t0), 0)

	for _, at := range []time.Duration{0, 11 * time.Minute, 21 * time.Minute, 25 * time.Minute} {
		rec.evaluateCellReadiness(context.Background(), cr, t0.Add(at))
	}
	if client.deleteControllerPodCalls != 0 || len(client.podGraceDeletes) != 0 {
		t.Fatalf("expected no delete while plugins-init progresses, got %d plain and %d grace deletes",
			client.deleteControllerPodCalls, len(client.podGraceDeletes))
	}
	if cr.Status.Cell.Evacuate {
		t.Fatal("expected no evacuate while plugins-init progresses, past twice the grace period")
	}
	if cr.Status.Cell.RolledAt != nil {
		t.Fatalf("expected rolledAt unset, got %v", cr.Status.Cell.RolledAt)
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cond.Reason != v1alpha1.ReasonNotProvisioned {
		t.Fatalf("Reason = %q, want NotProvisioned", cond.Reason)
	}
	if !strings.Contains(cond.Message, "plugins-init running for 25m") {
		t.Fatalf("Message = %q, want it to name plugins-init running for 25m", cond.Message)
	}
}

// TestCellRollHoldsBetweenInitContainers asserts the hand-off window: the
// first init container exited 0 and the next one waits in PodInitializing.
// No init container runs, but the pod still makes progress.
func TestCellRollHoldsBetweenInitContainers(t *testing.T) {
	grace := 10 * time.Minute
	rec, client, cr := readyGateCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client.controllerPod = initStagePod(t0, corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, FinishedAt: metav1.Time{Time: t0.Add(24 * time.Minute)}},
	}, 0)

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(24*time.Minute))
	if client.deleteControllerPodCalls != 0 || len(client.podGraceDeletes) != 0 {
		t.Fatalf("expected no delete between init containers, got %d plain and %d grace deletes",
			client.deleteControllerPodCalls, len(client.podGraceDeletes))
	}
	if cr.Status.Cell.Evacuate {
		t.Fatal("expected no evacuate between init containers")
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if !strings.Contains(cond.Message, "init-groovy running for 24m") {
		t.Fatalf("Message = %q, want it to name init-groovy", cond.Message)
	}
}

func TestCellRollFiresPastInitCap(t *testing.T) {
	grace := 10 * time.Minute
	rec, client, cr := readyGateCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client.controllerPod = initStagePod(t0, runningInit(t0), 0)

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(cellInitRollCap+time.Minute))
	if len(client.podGraceDeletes) != 1 {
		t.Fatalf("expected one roll past the init cap, got %d grace deletes", len(client.podGraceDeletes))
	}
	if !cr.Status.Cell.Evacuate {
		t.Fatal("expected evacuate past the init cap and twice the grace period")
	}
}

func TestCellRollFiresOnInitCrashLoop(t *testing.T) {
	grace := 10 * time.Minute
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := map[string]*corev1.Pod{
		"restarted and running": initStagePod(t0, runningInit(t0.Add(5*time.Minute)), 1),
		"crash loop back-off": initStagePod(t0, corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
		}, 2),
		"image pull back-off": initStagePod(t0, corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"},
		}, 0),
	}
	for name, pod := range cases {
		t.Run(name, func(t *testing.T) {
			rec, client, cr := readyGateCell(t, grace)
			client.controllerPod = pod
			rec.evaluateCellReadiness(context.Background(), cr, t0)
			rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace+time.Minute))
			if len(client.podGraceDeletes) != 1 {
				t.Fatalf("expected one roll, got %d grace deletes", len(client.podGraceDeletes))
			}
		})
	}
}

func TestCellRollClockStartsAtInitFinish(t *testing.T) {
	grace := 10 * time.Minute
	rec, client, cr := readyGateCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	finished := t0.Add(16 * time.Minute)
	pod := initStagePod(t0, corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, FinishedAt: metav1.Time{Time: finished.Add(-time.Minute)}},
	}, 0)
	pod.Status.InitContainerStatuses[1].State = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, FinishedAt: metav1.Time{Time: finished}},
	}
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Time{Time: finished}}}
	client.controllerPod = pod

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(20*time.Minute))
	if client.deleteControllerPodCalls != 0 || len(client.podGraceDeletes) != 0 {
		t.Fatalf("expected no roll four minutes after init finish, got %d plain and %d grace deletes",
			client.deleteControllerPodCalls, len(client.podGraceDeletes))
	}
	if cr.Status.Cell.Evacuate {
		t.Fatal("expected no evacuate four minutes after init finish")
	}
	rec.evaluateCellReadiness(context.Background(), cr, finished.Add(grace+time.Second))
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("expected one plain delete of a started pod once the init-finish clock passes the grace, got %d", client.deleteControllerPodCalls)
	}
	if len(client.podGraceDeletes) != 0 {
		t.Fatalf("expected no short-grace delete for a pod whose main containers run, got %d", len(client.podGraceDeletes))
	}
}

func TestCellEvacuateStickyWithinUnreadyPeriod(t *testing.T) {
	grace := 10 * time.Minute
	rec, client, cr := readyGateCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rolled := t0.Add(grace)
	cr.Status.Cell = &v1alpha1.CellStatus{
		UnreadySince: &metav1.Time{Time: t0},
		RolledAt:     &metav1.Time{Time: rolled},
		Evacuate:     true,
	}
	client.controllerPod = initStagePod(rolled, runningInit(rolled), 0)

	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(25*time.Minute))
	if !cr.Status.Cell.Evacuate {
		t.Fatal("expected evacuate to stay true within the same unready period")
	}
	if client.deleteControllerPodCalls != 0 || len(client.podGraceDeletes) != 0 {
		t.Fatal("expected no second roll within the same unready period")
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cond.Reason != v1alpha1.ReasonCellRolled {
		t.Fatalf("Reason = %q, want CellRolled", cond.Reason)
	}
}

// TestCellEvacuateHeldAfterRoll asserts that a rolled cell whose new pod
// runs plugins-init does not evacuate at twice the grace period.
func TestCellEvacuateHeldAfterRoll(t *testing.T) {
	grace := 10 * time.Minute
	rec, client, cr := readyGateCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rolled := t0.Add(grace)
	cr.Status.Cell = &v1alpha1.CellStatus{
		UnreadySince: &metav1.Time{Time: t0},
		RolledAt:     &metav1.Time{Time: rolled},
	}
	client.controllerPod = initStagePod(rolled, runningInit(rolled), 0)

	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(25*time.Minute))
	if cr.Status.Cell.Evacuate {
		t.Fatal("expected no evacuate while the new pod runs plugins-init")
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cond.Reason != v1alpha1.ReasonCellRolled || !strings.HasPrefix(cond.Message, v1alpha1.ReasonNotProvisioned) ||
		!strings.Contains(cond.Message, "plugins-init running for 15m") {
		t.Fatalf("condition = %+v, want CellRolled with the fault reason and the init run time", cond)
	}
}

func TestCellRollSkipsTerminatingPod(t *testing.T) {
	grace := 10 * time.Minute
	rec, client, cr := readyGateCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pod := initStagePod(t0, runningInit(t0), 0)
	pod.DeletionTimestamp = &metav1.Time{Time: t0.Add(5 * time.Minute)}
	client.controllerPod = pod

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace+time.Minute))
	if client.deleteControllerPodCalls != 0 || len(client.podGraceDeletes) != 0 {
		t.Fatalf("expected no delete of a terminating pod, got %d plain and %d grace deletes",
			client.deleteControllerPodCalls, len(client.podGraceDeletes))
	}
	if cr.Status.Cell.RolledAt != nil {
		t.Fatalf("expected rolledAt unset, got %v", cr.Status.Cell.RolledAt)
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cond.Reason != v1alpha1.ReasonNotProvisioned {
		t.Fatalf("Reason = %q, want NotProvisioned", cond.Reason)
	}
}

func TestCellRollInitPodUsesShortGraceAndUID(t *testing.T) {
	grace := 10 * time.Minute
	rec, client, cr := readyGateCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client.controllerPod = initStagePod(t0, runningInit(t0), 1)

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace+time.Minute))
	if client.deleteControllerPodCalls != 0 {
		t.Fatalf("expected no plain delete for an init-stage pod, got %d", client.deleteControllerPodCalls)
	}
	want := podGraceDelete{namespace: "ns", pod: "cell1-abcd1234-0", uid: "pod-uid-1", graceSeconds: 30}
	if len(client.podGraceDeletes) != 1 || client.podGraceDeletes[0] != want {
		t.Fatalf("grace deletes = %+v, want exactly %+v", client.podGraceDeletes, want)
	}
	if cr.Status.Cell.RolledAt == nil {
		t.Fatal("expected rolledAt set after the short-grace delete")
	}
}

func TestCellRollNoPodKeepsLegacyRule(t *testing.T) {
	grace := 10 * time.Minute
	rec, client, cr := readyGateCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	if client.getControllerPodCalls != 0 {
		t.Fatalf("expected no pod read inside the grace period, got %d", client.getControllerPodCalls)
	}
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace+time.Minute))
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("expected the legacy delete with no pod, got %d", client.deleteControllerPodCalls)
	}
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(2*grace+time.Minute))
	if !cr.Status.Cell.Evacuate {
		t.Fatal("expected evacuate after twice the grace period with no pod")
	}
}
