package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestResolveClosure_LockSourcedMemberIsNeverUnknown: a member the profile lock
// supplies has no store entry, so its dependency list is never read from the
// store and never counts as unknown.
func TestResolveClosure_LockSourcedMemberIsNeverUnknown(t *testing.T) {
	root := entry("root", "1.0", dep("locked", "1.0"))
	profiles := []ucProfile{{Name: "lts", Eligible: true, EffectiveCore: "2.555.3",
		Lock: map[string]string{"locked": "1.5"}}}
	cl := mustResolve(t, root, []inventoryEntry{root}, profiles)
	if len(cl.UnknownDeps) != 0 {
		t.Fatalf("UnknownDeps = %v, want none", cl.UnknownDeps)
	}
	if sel, ok := cl.Selected["locked"]; !ok || sel.Provenance != provenanceLock {
		t.Fatalf("locked selection = %+v, want provenance lock", cl.Selected["locked"])
	}
}

// TestClosureCompleteness_ConditionAndMessage covers both shapes of the
// ClosureComplete condition and the invalid message it returns.
func TestClosureCompleteness_ConditionAndMessage(t *testing.T) {
	cond, invalid := closureCompleteness(&ucClosure{})
	if cond.Type != closureCompleteCondition || cond.Status != metav1.ConditionTrue ||
		cond.Reason != reasonAllDependenciesKnown || invalid != "" {
		t.Fatalf("complete closure = %+v, invalid %q", cond, invalid)
	}

	cond, invalid = closureCompleteness(&ucClosure{UnknownDeps: []string{"aaa@1.0", "leaf@1.0"}})
	if cond.Status != metav1.ConditionFalse || cond.Reason != reasonUnknownDependencies {
		t.Fatalf("incomplete closure = %+v", cond)
	}
	for _, want := range []string{"aaa@1.0", "leaf@1.0"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("condition message %q does not name %s", cond.Message, want)
		}
	}
	if invalid != cond.Message {
		t.Errorf("invalid message %q must equal the condition message %q", invalid, cond.Message)
	}
}
