package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestCompatWarning_NamesUnknownMembers: the False branch names every unread
// closure member, so an operator sees why the closure is not complete.
func TestCompatWarning_NamesUnknownMembers(t *testing.T) {
	compat := []v1alpha1.CatalogItemCompat{{Profile: "p", Verdict: verdictCompatible}}
	cond := compatWarning(compat, []string{"aaa@1.0", "leaf@1.0"})
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("status = %q, want False", cond.Status)
	}
	for _, want := range []string{"aaa@1.0", "leaf@1.0"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message %q does not name %s", cond.Message, want)
		}
	}
}

// TestCompatWarning_NoProfilesNamesUnknownMembers: the NoProfiles branch also
// names every unread closure member.
func TestCompatWarning_NoProfilesNamesUnknownMembers(t *testing.T) {
	cond := compatWarning(nil, []string{"aaa@1.0", "leaf@1.0"})
	if cond.Reason != "NoProfiles" || cond.Status != metav1.ConditionUnknown {
		t.Fatalf("condition = %+v", cond)
	}
	for _, want := range []string{"aaa@1.0", "leaf@1.0"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message %q does not name %s", cond.Message, want)
		}
	}
}

// TestCompatWarning_OffenderBranchNamesUnknownMembers: a blocking verdict on
// one profile must not hide the unread members from the message.
func TestCompatWarning_OffenderBranchNamesUnknownMembers(t *testing.T) {
	compat := []v1alpha1.CatalogItemCompat{
		{Profile: "p1", Verdict: verdictCoreTooOld},
		{Profile: "p2", Verdict: verdictUnknown},
	}
	cond := compatWarning(compat, []string{"leaf@1.0"})
	if cond.Status != metav1.ConditionTrue {
		t.Fatalf("status = %q, want True", cond.Status)
	}
	if !strings.Contains(cond.Message, "leaf@1.0") {
		t.Errorf("message %q does not name leaf@1.0", cond.Message)
	}
}
