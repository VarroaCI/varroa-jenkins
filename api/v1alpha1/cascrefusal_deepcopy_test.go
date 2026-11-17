package v1alpha1

import "testing"

// TestControllerStatusDeepCopyIsolatesCascRefusal proves that a copy of the
// status owns its own CascRefusal, so a change to the copy does not reach the
// original.
func TestControllerStatusDeepCopyIsolatesCascRefusal(t *testing.T) {
	orig := &ControllerStatus{CascRefusal: &CascRefusal{ConfigHash: "c1", RBACHash: "r1"}}
	var cp ControllerStatus
	orig.DeepCopyInto(&cp)
	cp.CascRefusal.ConfigHash = "mutated"
	if orig.CascRefusal.ConfigHash != "c1" {
		t.Fatalf("CascRefusal not isolated: original ConfigHash = %q", orig.CascRefusal.ConfigHash)
	}
	var empty ControllerStatus
	(&ControllerStatus{}).DeepCopyInto(&empty)
	if empty.CascRefusal != nil {
		t.Fatal("a nil CascRefusal must stay nil in the copy")
	}
}
