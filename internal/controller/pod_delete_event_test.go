package controller

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/overlay"
)

// startedCellPod answers a cell pod with uid whose Jenkins container runs.
// The roll rule reads it as a pod whose main containers started.
func startedCellPod(uid types.UID) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "cell1-0", Namespace: "ns", UID: uid},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  overlay.JenkinsContainerName,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
}

// bootFailedPod answers a crash-looping pod with uid that runs revision.
func bootFailedPod(uid types.UID, revision string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-0",
			UID:    uid,
			Labels: map[string]string{appsv1.StatefulSetRevisionLabel: revision},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:                 overlay.JenkinsContainerName,
			RestartCount:         3,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 5, Reason: "Error"}},
		}}},
	}
}

// captureActivity starts an embedded NATS server and gives rec a publisher
// on it. It answers a function that collects the activity events of
// namespace/name that arrive within wait. The test skips unless NATS_TEST or
// CI is set, the gate of the other embedded-NATS tests.
func captureActivity(t *testing.T, rec *Reconciler, namespace, name string) func(wait time.Duration) []activity.Event {
	t.Helper()
	if os.Getenv("NATS_TEST") == "" && os.Getenv("CI") == "" {
		t.Skip("skipping NATS integration test; set NATS_TEST=1 to run")
	}
	s, err := server.NewServer(&server.Options{Port: -1, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(s.Shutdown)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	received := make(chan activity.Event, 10)
	sub, err := conn.SubscribeData(bus.ActivitySubject(bus.DefaultCluster, namespace, name), func(data []byte) {
		var e activity.Event
		if jsonErr := json.Unmarshal(data, &e); jsonErr == nil {
			received <- e
		}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	rec.SetActivityPublisher(activity.NewPublisher(bus.DefaultCluster, conn))

	return func(wait time.Duration) []activity.Event {
		var got []activity.Event
		deadline := time.After(wait)
		for {
			select {
			case e := <-received:
				got = append(got, e)
			case <-deadline:
				return got
			}
		}
	}
}

// eventsOfType answers the events of type eventType in events.
func eventsOfType(events []activity.Event, eventType string) []activity.Event {
	var out []activity.Event
	for _, e := range events {
		if e.Type == eventType {
			out = append(out, e)
		}
	}
	return out
}

// TestPodDeletedEventNamesThePod asserts the shape of an operator pod delete
// event: the operator source, the controller, the reason word, the deleted
// pod's UID as instance, and a fixed info severity that Normalize keeps.
func TestPodDeletedEventNamesThePod(t *testing.T) {
	cr := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell1", Namespace: "ns"}}

	e := podDeletedEvent("pod.replaced", cr, "deleted the pod", v1alpha1.ReasonJenkinsBootFailed, "pod-uid-1")
	e.Normalize()

	if e.Type != "pod.replaced" || e.Source != "operator" || e.Controller != "cell1" || e.Namespace != "ns" {
		t.Fatalf("event = %+v, want an operator pod.replaced event of ns/cell1", e)
	}
	if e.Reason != v1alpha1.ReasonJenkinsBootFailed || e.Instance != "pod-uid-1" || e.Message != "deleted the pod" {
		t.Fatalf("event = %+v, want reason JenkinsBootFailed, instance pod-uid-1, and the message", e)
	}
	if e.Severity != "info" {
		t.Fatalf("severity = %q, want info: the reason word must not set the severity", e.Severity)
	}
}

// TestRolledPodInstance asserts which pod a cell roll names. The observed
// pod wins. A roll with no observed pod names status.cell.instance.
func TestRolledPodInstance(t *testing.T) {
	cases := []struct {
		name string
		obs  initObservation
		cell *v1alpha1.CellStatus
		want string
	}{
		{"the observed pod", initObservation{Present: true, UID: "pod-observed"}, &v1alpha1.CellStatus{Instance: "pod-status"}, "pod-observed"},
		{"no observed pod", initObservation{}, &v1alpha1.CellStatus{Instance: "pod-status"}, "pod-status"},
		{"a failed pod read", initObservation{ReadFailed: true}, &v1alpha1.CellStatus{Instance: "pod-status"}, "pod-status"},
		{"no cell status", initObservation{}, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rolledPodInstance(tc.obs, tc.cell); got != tc.want {
				t.Fatalf("rolledPodInstance = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCellRollSkipsAPodReplacedBeforeTheDelete asserts the precondition of
// a roll. The tick observes one pod, and a new pod takes the name before the
// delete. The delete for the observed UID fails its precondition, so the new
// pod survives, no cell.rolled event goes out, and the tick leaves rolledAt
// unset for the next tick to decide again.
func TestCellRollSkipsAPodReplacedBeforeTheDelete(t *testing.T) {
	grace := 10 * time.Minute
	rec, client, cr := readyGateCell(t, grace)
	collect := captureActivity(t, rec, "ns", "cell1")
	client.controllerPod = startedCellPod("pod-uid-old")
	replacement := startedCellPod("pod-uid-new")
	client.beforePodUIDDelete = func() { client.controllerPod = replacement }

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rec.evaluateCellReadiness(context.Background(), cr, t0)
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace+time.Minute))

	if client.podUIDDeleteRefusals != 1 || len(client.podUIDDeletes) != 0 || client.deleteControllerPodCalls != 0 {
		t.Fatalf("refusals = %d, UID deletes = %+v, deletes by name = %d, want one refused delete and nothing deleted",
			client.podUIDDeleteRefusals, client.podUIDDeletes, client.deleteControllerPodCalls)
	}
	if client.controllerPod.UID != "pod-uid-new" {
		t.Fatalf("pod UID = %q, want the replacement pod-uid-new to survive", client.controllerPod.UID)
	}
	if cr.Status.Cell.RolledAt != nil {
		t.Fatalf("rolledAt = %v, want unset after a refused delete", cr.Status.Cell.RolledAt)
	}
	if cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady); cond.Reason != v1alpha1.ReasonNotProvisioned {
		t.Fatalf("CellReady reason = %q, want NotProvisioned", cond.Reason)
	}
	if rolled := eventsOfType(collect(300*time.Millisecond), "cell.rolled"); len(rolled) != 0 {
		t.Fatalf("cell.rolled events = %+v, want none for a refused delete", rolled)
	}
}

// TestCellRollFallsBackToTheStatusInstance asserts the roll of a pod that the
// tick could not read. The delete carries status.cell.instance as its UID
// precondition. The roll goes when that UID still names the pod, and the
// event names it. A stale UID deletes nothing and writes no event.
func TestCellRollFallsBackToTheStatusInstance(t *testing.T) {
	cases := []struct {
		name        string
		statusUID   types.UID
		wantDeleted bool
	}{
		{"the status names the pod", "pod-uid-1", true},
		{"the status names a replaced pod", "pod-uid-old", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grace := 10 * time.Minute
			rec, client, cr := readyGateCell(t, grace)
			collect := captureActivity(t, rec, "ns", "cell1")
			client.controllerPod = startedCellPod("pod-uid-1")
			client.controllerPodErr = context.DeadlineExceeded
			cr.UID = testUID
			cr.Status.Cell = &v1alpha1.CellStatus{Instance: string(tc.statusUID)}

			t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			rec.evaluateCellReadiness(context.Background(), cr, t0)
			rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace+time.Minute))

			rolled := eventsOfType(collect(300*time.Millisecond), "cell.rolled")
			if !tc.wantDeleted {
				if len(client.podUIDDeletes) != 0 || client.podUIDDeleteRefusals != 1 || len(rolled) != 0 {
					t.Fatalf("UID deletes = %+v, refusals = %d, events = %+v, want one refusal and no event",
						client.podUIDDeletes, client.podUIDDeleteRefusals, rolled)
				}
				if cr.Status.Cell.RolledAt != nil {
					t.Fatalf("rolledAt = %v, want unset after a refused delete", cr.Status.Cell.RolledAt)
				}
				return
			}
			want := podUIDDelete{namespace: "ns", pod: PodName(cr, 0), uid: "pod-uid-1", graceSeconds: 0}
			if len(client.podUIDDeletes) != 1 || client.podUIDDeletes[0] != want {
				t.Fatalf("UID deletes = %+v, want exactly %+v", client.podUIDDeletes, want)
			}
			if len(rolled) != 1 || rolled[0].Instance != "pod-uid-1" {
				t.Fatalf("cell.rolled events = %+v, want one that names pod-uid-1", rolled)
			}
		})
	}
}

// TestCellRollWithNoKnownPodNamesNoPod asserts the roll when no read names a
// pod: no observed pod and no status.cell.instance. The delete goes by name,
// as before, and the cell.rolled event carries no instance, so the record
// store gives its reason to no run.
func TestCellRollWithNoKnownPodNamesNoPod(t *testing.T) {
	grace := 10 * time.Minute
	rec, client, cr := readyGateCell(t, grace)
	collect := captureActivity(t, rec, "ns", "cell1")

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rec.evaluateCellReadiness(context.Background(), cr, t0)
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace+time.Minute))

	if client.deleteControllerPodCalls != 1 || len(client.podUIDDeletes) != 0 {
		t.Fatalf("deletes by name = %d, UID deletes = %+v, want one delete by name",
			client.deleteControllerPodCalls, client.podUIDDeletes)
	}
	rolled := eventsOfType(collect(300*time.Millisecond), "cell.rolled")
	if len(rolled) != 1 || rolled[0].Instance != "" {
		t.Fatalf("cell.rolled events = %+v, want one with no instance", rolled)
	}
}

// TestReplaceStaleBootFailedPodPublishesTheCause asserts that the delete of
// a boot-failed pod that lags the StatefulSet revision carries the pod's UID
// as its precondition, and that the pod.replaced event names that UID and
// the reason JenkinsBootFailed.
func TestReplaceStaleBootFailedPodPublishesTheCause(t *testing.T) {
	client := newTestClientWithBundle()
	client.controllerPod = bootFailedPod("pod-boot-failed", "rev-1")
	client.statefulSetRollout = StatefulSetRollout{UpdateRevision: "rev-2", RollingUpdate: true}
	rec := newTestReconciler(client)
	collect := captureActivity(t, rec, "ns1", "test")
	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)

	rec.reconcileJenkinsBootFailure(context.Background(), cr)

	want := podUIDDelete{namespace: "ns1", pod: "test-0", uid: "pod-boot-failed", graceSeconds: 0}
	if client.deleteControllerPodCalls != 0 || len(client.podUIDDeletes) != 1 || client.podUIDDeletes[0] != want {
		t.Fatalf("deletes by name = %d, UID deletes = %+v, want exactly %+v",
			client.deleteControllerPodCalls, client.podUIDDeletes, want)
	}
	replaced := eventsOfType(collect(time.Second), "pod.replaced")
	if len(replaced) != 1 || replaced[0].Reason != v1alpha1.ReasonJenkinsBootFailed || replaced[0].Instance != "pod-boot-failed" {
		t.Fatalf("pod.replaced events = %+v, want one with JenkinsBootFailed and instance pod-boot-failed", replaced)
	}
}

// TestReplaceStaleBootFailedPodSkipsAReplacedPod asserts that a new pod that
// takes the name between the read and the delete survives, and that no
// pod.replaced event goes out.
func TestReplaceStaleBootFailedPodSkipsAReplacedPod(t *testing.T) {
	client := newTestClientWithBundle()
	client.controllerPod = bootFailedPod("pod-boot-failed", "rev-1")
	replacement := bootFailedPod("pod-uid-new", "rev-2")
	client.beforePodUIDDelete = func() { client.controllerPod = replacement }
	client.statefulSetRollout = StatefulSetRollout{UpdateRevision: "rev-2", RollingUpdate: true}
	rec := newTestReconciler(client)
	collect := captureActivity(t, rec, "ns1", "test")
	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)

	rec.reconcileJenkinsBootFailure(context.Background(), cr)

	if client.podUIDDeleteRefusals != 1 || len(client.podUIDDeletes) != 0 || client.deleteControllerPodCalls != 0 {
		t.Fatalf("refusals = %d, UID deletes = %+v, deletes by name = %d, want one refused delete and nothing deleted",
			client.podUIDDeleteRefusals, client.podUIDDeletes, client.deleteControllerPodCalls)
	}
	if client.controllerPod.UID != "pod-uid-new" {
		t.Fatalf("pod UID = %q, want the replacement pod-uid-new to survive", client.controllerPod.UID)
	}
	if replaced := eventsOfType(collect(300*time.Millisecond), "pod.replaced"); len(replaced) != 0 {
		t.Fatalf("pod.replaced events = %+v, want none for a refused delete", replaced)
	}
}
