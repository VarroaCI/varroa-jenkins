package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// captureDeleteOptions makes cs record the options of each pod delete in
// got, and lets the fake clientset run the delete.
func captureDeleteOptions(cs *fake.Clientset, got *metav1.DeleteOptions) {
	cs.PrependReactor("delete", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		*got = a.(clienttesting.DeleteActionImpl).DeleteOptions
		return false, nil, nil
	})
}

func TestDeleteControllerPodUIDSendsGraceAndUID(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "cell1-abcd1234-0", Namespace: "ns", UID: "pod-uid-1"}}
	cs := fake.NewSimpleClientset(pod)
	var got metav1.DeleteOptions
	captureDeleteOptions(cs, &got)
	c := &ClientsetClient{clientset: cs}

	deleted, err := c.DeleteControllerPodUID(context.Background(), "ns", "cell1-abcd1234-0", "pod-uid-1", 30)
	if err != nil || !deleted {
		t.Fatalf("DeleteControllerPodUID = %v, %v, want an accepted delete", deleted, err)
	}
	if got.GracePeriodSeconds == nil || *got.GracePeriodSeconds != 30 {
		t.Fatalf("GracePeriodSeconds = %v, want 30", got.GracePeriodSeconds)
	}
	if got.Preconditions == nil || got.Preconditions.UID == nil || *got.Preconditions.UID != "pod-uid-1" {
		t.Fatalf("Preconditions = %+v, want UID pod-uid-1", got.Preconditions)
	}
}

// TestDeleteControllerPodUIDKeepsThePodGrace asserts that a grace of 0 sends
// no GracePeriodSeconds, so the delete is never a force delete.
func TestDeleteControllerPodUIDKeepsThePodGrace(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "cell1-0", Namespace: "ns", UID: "pod-uid-1"}}
	cs := fake.NewSimpleClientset(pod)
	var got metav1.DeleteOptions
	captureDeleteOptions(cs, &got)
	c := &ClientsetClient{clientset: cs}

	if deleted, err := c.DeleteControllerPodUID(context.Background(), "ns", "cell1-0", "pod-uid-1", 0); err != nil || !deleted {
		t.Fatalf("DeleteControllerPodUID = %v, %v, want an accepted delete", deleted, err)
	}
	if got.GracePeriodSeconds != nil {
		t.Fatalf("GracePeriodSeconds = %d, want none", *got.GracePeriodSeconds)
	}
	if got.Preconditions == nil || got.Preconditions.UID == nil || *got.Preconditions.UID != "pod-uid-1" {
		t.Fatalf("Preconditions = %+v, want UID pod-uid-1", got.Preconditions)
	}
}

func TestDeleteControllerPodUIDMissingPodIsNotDeleted(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	deleted, err := c.DeleteControllerPodUID(context.Background(), "ns", "gone-0", "pod-uid-1", 30)
	if err != nil || deleted {
		t.Fatalf("DeleteControllerPodUID = %v, %v, want false and no error for a missing pod", deleted, err)
	}
}

// honourUIDPrecondition makes cs answer Conflict to a pod delete whose UID
// precondition names another pod, as the API server does. The fake
// clientset alone ignores preconditions.
func honourUIDPrecondition(cs *fake.Clientset) {
	cs.PrependReactor("delete", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		del := a.(clienttesting.DeleteActionImpl)
		pre := del.DeleteOptions.Preconditions
		if pre == nil || pre.UID == nil {
			return false, nil, nil
		}
		obj, err := cs.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), del.Namespace, del.Name)
		if err == nil && obj.(*corev1.Pod).UID != *pre.UID {
			return true, nil, apierrors.NewConflict(corev1.Resource("pods"), del.Name,
				errors.New("the UID in the precondition does not match the UID in the record"))
		}
		return false, nil, nil
	})
}

// TestDeleteControllerPodUIDKeepsAReplacedPod asserts that a pod created
// again under the same name survives a delete for the old UID.
func TestDeleteControllerPodUIDKeepsAReplacedPod(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "cell1-0", Namespace: "ns", UID: "pod-uid-new"}}
	cs := fake.NewSimpleClientset(pod)
	honourUIDPrecondition(cs)
	c := &ClientsetClient{clientset: cs}

	deleted, err := c.DeleteControllerPodUID(context.Background(), "ns", "cell1-0", "pod-uid-old", 0)
	if err != nil || deleted {
		t.Fatalf("DeleteControllerPodUID = %v, %v, want false and no error for another UID", deleted, err)
	}
	if _, err := cs.CoreV1().Pods("ns").Get(context.Background(), "cell1-0", metav1.GetOptions{}); err != nil {
		t.Fatalf("the replacement pod is gone: %v", err)
	}
}

func TestDeleteControllerPodUIDRejectsANegativeGrace(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	if _, err := c.DeleteControllerPodUID(context.Background(), "ns", "cell1-0", "pod-uid-1", -1); err == nil {
		t.Fatal("expected an error for a negative grace")
	}
}
