package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestPluginInventoryDeepCopyIsolatesSlices mirrors the pattern in
// versionprofile_deepcopy_test.go. The status struct must deep-copy its
// slice fields so a mutation to the copy does not reach the original.
func TestPluginInventoryDeepCopyIsolatesSlices(t *testing.T) {
	orig := &PluginInventoryStatus{
		Hash:     "v1:abcdef",
		Source:   "jenkins-api",
		Total:    5,
		Stale:    false,
		Degraded: false,
		Counts:   map[string]int{"declared": 3, "unmanaged": 2},
		Drift: []PluginInventoryDriftEntry{
			{Name: "rogue", Version: "1.0", Class: "unmanaged"},
		},
		VersionDrift: []PluginInventoryDriftEntry{
			{Name: "mailer", Version: "1.5", Class: "declared", Verdict: "ahead"},
		},
		PendingCollect: &PendingCollect{CommandID: "cmd-1"},
	}

	cp := orig.DeepCopy()
	cp.Counts["unmanaged"] = 999
	cp.Drift[0].Name = "mutated"
	cp.VersionDrift[0].Name = "mutated-version"
	cp.PendingCollect.CommandID = "mutated-cmd"

	if orig.Counts["unmanaged"] != 2 {
		t.Errorf("counts not isolated: original mutated to %d", orig.Counts["unmanaged"])
	}
	if orig.Drift[0].Name != "rogue" {
		t.Errorf("drift not isolated: original mutated to %q", orig.Drift[0].Name)
	}
	if orig.VersionDrift[0].Name != "mailer" {
		t.Errorf("versionDrift not isolated: original mutated to %q", orig.VersionDrift[0].Name)
	}
	if orig.PendingCollect.CommandID != "cmd-1" {
		t.Errorf("pendingCollect not isolated: original mutated to %q", orig.PendingCollect.CommandID)
	}
}

func TestPluginInventoryDriftEntry_DeepCopy(t *testing.T) {
	orig := PluginInventoryDriftEntry{Name: "test", Version: "1.0", Class: "unmanaged"}
	cp := orig.DeepCopy()
	cp.Name = "mutated"
	if orig.Name != "test" {
		t.Error("DeepCopy did not isolate")
	}
}

func TestPendingCollect_DeepCopy(t *testing.T) {
	orig := PendingCollect{CommandID: "cmd-1", IssuedAt: metav1.Now()}
	cp := orig.DeepCopy()
	cp.CommandID = "mutated"
	if orig.CommandID != "cmd-1" {
		t.Error("DeepCopy did not isolate")
	}
}
