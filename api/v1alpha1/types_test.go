package v1alpha1

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestMiteSpecDeepCopyIsolatesScalars ensures that a MiteSpec with the
// Image/ImagePullPolicy scalar fields produces an independent copy with
// equal values via DeepCopy.
func TestMiteSpecDeepCopyIsolatesScalars(t *testing.T) {
	orig := &MiteSpec{
		Image:           "ghcr.io/varroaci/varroa-jenkins:v2",
		ImagePullPolicy: corev1.PullAlways,
	}

	cp := new(MiteSpec)
	orig.DeepCopyInto(cp)

	// Values must be equal.
	if cp.Image != orig.Image {
		t.Errorf("Image = %q, want %q", cp.Image, orig.Image)
	}
	if cp.ImagePullPolicy != orig.ImagePullPolicy {
		t.Errorf("ImagePullPolicy = %q, want %q", cp.ImagePullPolicy, orig.ImagePullPolicy)
	}

	// Independence: mutate copy, original must be unchanged.
	cp.Image = "mutated:tag"
	cp.ImagePullPolicy = corev1.PullNever

	if orig.Image != "ghcr.io/varroaci/varroa-jenkins:v2" {
		t.Errorf("original Image was mutated to %q", orig.Image)
	}
	if orig.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("original ImagePullPolicy was mutated to %q", orig.ImagePullPolicy)
	}
}

// TestControllerStatusDeepCopy_LastReconcileErrorAt ensures the
// LastReconcileErrorAt pointer field survives DeepCopy as an equal-but-
// distinct pointer, and nil survives as nil.
func TestControllerStatusDeepCopy_LastReconcileErrorAt(t *testing.T) {
	// Non-nil value → equal but distinct pointer after DeepCopy.
	now := metav1.NewTime(time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC))
	orig := &ControllerStatus{
		LastReconcileError:   "some error",
		LastReconcileErrorAt: &now,
	}

	cp := new(ControllerStatus)
	orig.DeepCopyInto(cp)

	// Values must be equal.
	if cp.LastReconcileError != orig.LastReconcileError {
		t.Errorf("LastReconcileError = %q, want %q", cp.LastReconcileError, orig.LastReconcileError)
	}
	if cp.LastReconcileErrorAt == nil {
		t.Fatal("LastReconcileErrorAt became nil")
	}
	if !cp.LastReconcileErrorAt.Time.Equal(orig.LastReconcileErrorAt.Time) {
		t.Errorf("LastReconcileErrorAt = %v, want %v", cp.LastReconcileErrorAt.Time, orig.LastReconcileErrorAt.Time)
	}

	// Pointers must be distinct (not a shallow copy).
	if cp.LastReconcileErrorAt == orig.LastReconcileErrorAt {
		t.Error("LastReconcileErrorAt pointers are identical — deep copy did not allocate")
	}

	// Mutate copy; original must be unchanged.
	cp.LastReconcileErrorAt.Time = orig.LastReconcileErrorAt.Add(time.Hour)
	if orig.LastReconcileErrorAt.Time.Equal(cp.LastReconcileErrorAt.Time) {
		t.Error("original LastReconcileErrorAt was mutated through copy")
	}

	// Nil value → nil after DeepCopy.
	nilOrig := &ControllerStatus{
		LastReconcileError:   "",
		LastReconcileErrorAt: nil,
	}
	nilCp := new(ControllerStatus)
	nilOrig.DeepCopyInto(nilCp)

	if nilCp.LastReconcileErrorAt != nil {
		t.Errorf("LastReconcileErrorAt should be nil after deep copy, got %v", nilCp.LastReconcileErrorAt)
	}
}
