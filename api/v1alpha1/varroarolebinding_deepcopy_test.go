package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestVarroaRoleBindingDeepCopyIsolatesStatus mutates a deep copy of a
// binding with federation entries and asserts the original stays unchanged.
// DeepCopy for the status types is hand-written.
func TestVarroaRoleBindingDeepCopyIsolatesStatus(t *testing.T) {
	ts := metav1.NewTime(time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC))
	orig := &VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec:       VarroaRoleBindingSpec{RoleRef: "r", Subjects: []SubjectRef{{Kind: "User", Name: "alice"}}},
		Status: VarroaRoleBindingStatus{
			ObservedGeneration: 2,
			Federation: []BindingFederationStatus{
				{Cluster: "member-c", State: BindingFailed, Reason: "HiveUnavailable", Message: "offline", LastTransitionTime: ts},
			},
		},
	}
	cp := orig.DeepCopy()
	cp.Status.Federation[0].State = BindingFederated
	cp.Status.Federation[0].LastTransitionTime = metav1.NewTime(ts.Add(time.Hour))
	cp.Status.Federation = append(cp.Status.Federation, BindingFederationStatus{Cluster: "edge"})

	if got := orig.Status.Federation[0].State; got != BindingFailed {
		t.Fatalf("original state = %q, want failed", got)
	}
	if !orig.Status.Federation[0].LastTransitionTime.Equal(&ts) {
		t.Fatalf("original lastTransitionTime changed")
	}
	if len(orig.Status.Federation) != 1 {
		t.Fatalf("original federation length = %d, want 1", len(orig.Status.Federation))
	}
}
