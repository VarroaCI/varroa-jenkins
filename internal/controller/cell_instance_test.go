package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestUpdateCellInstance_RunningPod covers task 6.0: a running pod's UID
// is written to status.cell.instance.
func TestUpdateCellInstance_RunningPod(t *testing.T) {
	client := newTestClient()
	client.controllerPod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("pod-uid-1")},
	}
	rec := newTestReconciler(client)
	cr := &v1alpha1.Controller{}
	cr.Name = "test"
	cr.Namespace = "default"

	rec.updateCellInstance(context.Background(), cr)

	if cr.Status.Cell == nil || cr.Status.Cell.Instance != "pod-uid-1" {
		t.Fatalf("expected cell.instance to be pod-uid-1, got %+v", cr.Status.Cell)
	}
}

// TestUpdateCellInstance_NoPod covers task 6.0: an absent pod clears the
// field.
func TestUpdateCellInstance_NoPod(t *testing.T) {
	client := newTestClient()
	client.controllerPod = nil
	rec := newTestReconciler(client)
	cr := &v1alpha1.Controller{}
	cr.Name = "test"
	cr.Namespace = "default"
	cr.Status.Cell = &v1alpha1.CellStatus{Instance: "stale-uid"}

	rec.updateCellInstance(context.Background(), cr)

	if cr.Status.Cell.Instance != "" {
		t.Fatalf("expected cell.instance to be cleared, got %q", cr.Status.Cell.Instance)
	}
}

// TestUpdateCellInstance_ReplacedPodRecordsPrevious asserts a changed pod
// uid moves the old value to previousInstance with the mite's last
// heartbeat, and that a later empty reading keeps that record.
func TestUpdateCellInstance_ReplacedPodRecordsPrevious(t *testing.T) {
	client := newTestClient()
	client.controllerPod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("pod-uid-2")},
	}
	rec := newTestReconciler(client)
	lastSeen := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cr := &v1alpha1.Controller{}
	cr.Name = "test"
	cr.Namespace = "default"
	cr.Status.Cell = &v1alpha1.CellStatus{Instance: "pod-uid-1"}
	cr.Status.MiteStatus = &v1alpha1.MiteStatus{LastSeen: &lastSeen}
	rec.updateCellInstance(context.Background(), cr)
	if cr.Status.Cell.Instance != "pod-uid-2" {
		t.Fatalf("instance = %q, want pod-uid-2", cr.Status.Cell.Instance)
	}
	if cr.Status.Cell.PreviousInstance != "pod-uid-1" {
		t.Fatalf("previousInstance = %q, want pod-uid-1", cr.Status.Cell.PreviousInstance)
	}
	if cr.Status.Cell.PreviousInstanceLastSeen == nil || !cr.Status.Cell.PreviousInstanceLastSeen.Time.Equal(lastSeen.Time) {
		t.Fatalf("previousInstanceLastSeen = %v, want %v", cr.Status.Cell.PreviousInstanceLastSeen, lastSeen)
	}

	client.controllerPod = nil
	rec.updateCellInstance(context.Background(), cr)
	if cr.Status.Cell.Instance != "" || cr.Status.Cell.PreviousInstance != "pod-uid-2" {
		t.Fatalf("after pod loss: instance = %q previous = %q, want \"\" and pod-uid-2", cr.Status.Cell.Instance, cr.Status.Cell.PreviousInstance)
	}
	client.controllerPod = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID("pod-uid-3")}}
	rec.updateCellInstance(context.Background(), cr)
	if cr.Status.Cell.Instance != "pod-uid-3" || cr.Status.Cell.PreviousInstance != "pod-uid-2" {
		t.Fatalf("after new pod: instance = %q previous = %q, want pod-uid-3 and pod-uid-2", cr.Status.Cell.Instance, cr.Status.Cell.PreviousInstance)
	}
}

// TestUpdateCellInstance_PodPhase asserts the pod-phase rule. A pod in
// phase Failed or Succeeded counts as no pod, so the reconciler clears
// status.cell.instance and records the replaced uid. A pod in phase
// Running or Pending keeps the uid.
func TestUpdateCellInstance_PodPhase(t *testing.T) {
	cases := []struct {
		name         string
		phase        corev1.PodPhase
		wantInstance string
		wantPrevious string
	}{
		{name: "failed", phase: corev1.PodFailed, wantInstance: "", wantPrevious: "old-uid"},
		{name: "succeeded", phase: corev1.PodSucceeded, wantInstance: "", wantPrevious: "old-uid"},
		{name: "running", phase: corev1.PodRunning, wantInstance: "pod-uid-9", wantPrevious: "old-uid"},
		{name: "pending", phase: corev1.PodPending, wantInstance: "pod-uid-9", wantPrevious: "old-uid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient()
			client.controllerPod = &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{UID: types.UID("pod-uid-9")},
				Status:     corev1.PodStatus{Phase: tc.phase},
			}
			rec := newTestReconciler(client)
			cr := &v1alpha1.Controller{}
			cr.Name = "test"
			cr.Namespace = "default"
			cr.Status.Cell = &v1alpha1.CellStatus{Instance: "old-uid"}

			rec.updateCellInstance(context.Background(), cr)

			if cr.Status.Cell.Instance != tc.wantInstance {
				t.Fatalf("instance = %q, want %q", cr.Status.Cell.Instance, tc.wantInstance)
			}
			if cr.Status.Cell.PreviousInstance != tc.wantPrevious {
				t.Fatalf("previousInstance = %q, want %q", cr.Status.Cell.PreviousInstance, tc.wantPrevious)
			}
		})
	}
}
