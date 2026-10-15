package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestVarroaRoleDeepCopyIsolatesStatus mutates a deep copy of a role with
// federation entries and asserts the original stays unchanged. DeepCopy for
// the status types is hand-written.
func TestVarroaRoleDeepCopyIsolatesStatus(t *testing.T) {
	ts := metav1.NewTime(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))
	orig := &VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "r"},
		Spec:       VarroaRoleSpec{JenkinsRoleRef: "jr"},
		Status: VarroaRoleStatus{
			ObservedGeneration: 3,
			Federation: []BindingFederationStatus{
				{Cluster: "member-c", State: BindingFailed, Reason: "StaleJenkinsRoleDeleteFailed", Message: "denied", LastTransitionTime: ts},
			},
		},
	}
	cp := orig.DeepCopy()
	cp.Status.Federation[0].Message = "changed"
	cp.Status.Federation[0].LastTransitionTime = metav1.NewTime(ts.Add(time.Hour))
	cp.Status.Federation = append(cp.Status.Federation, BindingFederationStatus{Cluster: "edge"})

	if got := orig.Status.Federation[0].Message; got != "denied" {
		t.Fatalf("original message = %q, want denied", got)
	}
	if !orig.Status.Federation[0].LastTransitionTime.Equal(&ts) {
		t.Fatalf("original lastTransitionTime changed")
	}
	if len(orig.Status.Federation) != 1 {
		t.Fatalf("original federation length = %d, want 1", len(orig.Status.Federation))
	}
}
