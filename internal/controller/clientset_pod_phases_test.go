package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

// TestListPodPhases asserts the read answers every pod of the namespace by
// UID, a pod with no label included, and no pod of another namespace.
func TestListPodPhases(t *testing.T) {
	pod := func(ns, name, uid string, managed bool, phase corev1.PodPhase) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)},
			Status:     corev1.PodStatus{Phase: phase},
		}
		if managed {
			p.Labels = map[string]string{"app.kubernetes.io/managed-by": "varroa-operator"}
		}
		return p
	}
	c := &ClientsetClient{clientset: fake.NewSimpleClientset(
		pod("cells-ns", "cell-a-0", "uid-a", true, corev1.PodRunning),
		pod("cells-ns", "cell-b-0", "uid-b", true, corev1.PodFailed),
		pod("cells-ns", "agent-0", "uid-agent", false, corev1.PodRunning),
		pod("other-ns", "cell-c-0", "uid-c", true, corev1.PodRunning),
	)}

	got, err := c.ListPodPhases(context.Background(), "cells-ns")
	if err != nil {
		t.Fatalf("ListPodPhases: %v", err)
	}
	want := map[types.UID]corev1.PodPhase{
		"uid-a": corev1.PodRunning, "uid-b": corev1.PodFailed, "uid-agent": corev1.PodRunning,
	}
	if len(got) != len(want) {
		t.Fatalf("phases = %v, want %v", got, want)
	}
	for uid, phase := range want {
		if got[uid] != phase {
			t.Fatalf("phase of %s = %q, want %q", uid, got[uid], phase)
		}
	}
}
