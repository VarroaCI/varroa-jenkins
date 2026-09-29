package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestDeleteControllerPodGraceSendsGraceAndUID(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "cell1-abcd1234-0", Namespace: "ns", UID: "pod-uid-1"}}
	cs := fake.NewSimpleClientset(pod)
	var got metav1.DeleteOptions
	cs.PrependReactor("delete", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		got = a.(clienttesting.DeleteActionImpl).DeleteOptions
		return false, nil, nil
	})
	c := &ClientsetClient{clientset: cs}

	if err := c.DeleteControllerPodGrace(context.Background(), "ns", "cell1-abcd1234-0", "pod-uid-1", 30); err != nil {
		t.Fatalf("DeleteControllerPodGrace: %v", err)
	}
	if got.GracePeriodSeconds == nil || *got.GracePeriodSeconds != 30 {
		t.Fatalf("GracePeriodSeconds = %v, want 30", got.GracePeriodSeconds)
	}
	if got.Preconditions == nil || got.Preconditions.UID == nil || *got.Preconditions.UID != "pod-uid-1" {
		t.Fatalf("Preconditions = %+v, want UID pod-uid-1", got.Preconditions)
	}
}

func TestDeleteControllerPodGraceMissingPodIsNil(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	if err := c.DeleteControllerPodGrace(context.Background(), "ns", "gone-0", "pod-uid-1", 30); err != nil {
		t.Fatalf("expected nil for a missing pod, got %v", err)
	}
}

func TestDeleteControllerPodGraceRejectsForceDelete(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	if err := c.DeleteControllerPodGrace(context.Background(), "ns", "cell1-0", "pod-uid-1", 0); err == nil {
		t.Fatal("expected an error for a grace of 0")
	}
}
