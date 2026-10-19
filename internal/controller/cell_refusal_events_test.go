package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestPendingSignatures_PublishesNewPairOnce checks that one refusal of a
// tenant requester yields one event, that a repeat of the same status yields
// none, and that a requester without a tenant yields none.
func TestPendingSignatures_PublishesNewPairOnce(t *testing.T) {
	cr := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "ns-cells"}}
	now := []v1alpha1.CellPendingSignature{{
		Signature: "java.lang.Runtime.exec",
		Requesters: []v1alpha1.CellSignatureRequester{
			{Tenant: "team-a", JobPath: "team-a/build", BuildNumber: 7},
			{Tenant: "", JobPath: "orphan", BuildNumber: 8},
		},
	}}
	seen := refusalKeys(nil)

	first := refusalEvents(cr, now, seen)
	if len(first) != 1 {
		t.Fatalf("first status: events = %d, want 1", len(first))
	}
	e := first[0]
	if e.Type != "cell.signature_refused" || e.Source != "operator" || e.Severity != "warning" {
		t.Errorf("event = %+v", e)
	}
	if e.Namespace != "ns-cells" || e.Controller != "cell-a" || e.ItemPath != "team-a/build" || e.BuildNumber != 7 {
		t.Errorf("event routing = %+v", e)
	}

	if again := refusalEvents(cr, now, seen); len(again) != 0 {
		t.Fatalf("repeat status: events = %d, want 0", len(again))
	}
}

// TestRefusalReasonIsBounded cuts a long signature to the reason bound.
func TestRefusalReasonIsBounded(t *testing.T) {
	long := make([]byte, maxRefusalReasonLength+50)
	for i := range long {
		long[i] = 'a'
	}
	cr := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "ns"}}
	now := []v1alpha1.CellPendingSignature{{
		Signature:  string(long),
		Requesters: []v1alpha1.CellSignatureRequester{{Tenant: "t", JobPath: "t/j", BuildNumber: 1}},
	}}
	events := refusalEvents(cr, now, refusalKeys(nil))
	if len(events) != 1 || len(events[0].Reason) != maxRefusalReasonLength {
		t.Fatalf("reason length = %d, want %d", len(events[0].Reason), maxRefusalReasonLength)
	}
}
