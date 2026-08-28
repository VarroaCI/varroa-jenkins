package v1alpha1

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestPluginInventoryStatus_PendingCollectMergePatchTrap verifies task 7.6b:
// PendingCollect must marshal as explicit null when nil, not be omitted, so a
// JSON merge patch (RFC 7386) actually clears the key rather than leaving it
// unchanged. Also verifies that bool fields marshal false explicitly so they
// can clear a prior true under merge-patch.
func TestPluginInventoryStatus_PendingCollectMergePatchTrap(t *testing.T) {
	now := metav1.Now()

	// --- Set PendingCollect: marshal must contain it ---
	status := &PluginInventoryStatus{
		Hash:   "v1:abcdef",
		Source: "jenkins-api",
		Total:  5,
		PendingCollect: &PendingCollect{
			CommandID: "cmd-1",
			IssuedAt:  now,
		},
	}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal with PendingCollect: %v", err)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal to check keys: %v", err)
	}

	// pendingCollect must be present.
	if _, ok := m["pendingCollect"]; !ok {
		t.Error("pendingCollect key is absent when non-nil — would not be written in merge patch")
	}

	// --- Clear PendingCollect: marshal must produce explicit null ---
	status.PendingCollect = nil
	data, err = json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal with nil PendingCollect: %v", err)
	}

	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal to check keys after clear: %v", err)
	}

	// pendingCollect must still be present as an explicit key.
	raw, ok := m["pendingCollect"]
	if !ok {
		t.Fatal("pendingCollect key is absent when nil — merge patch would retain old value forever")
	}
	if string(raw) != "null" {
		t.Errorf("pendingCollect = %s, want null", string(raw))
	}

	// --- Verify bool fields marshal explicitly (not omitted) ---
	status.Stale = false
	status.Degraded = false
	status.BootstrapApproximate = false
	status.OptionalEdgesDropped = false
	status.Truncated = false
	status.DriftTruncated = false

	data, err = json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal with all-zero bools: %v", err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal to check bool keys: %v", err)
	}

	for _, key := range []string{"stale", "degraded", "bootstrapApproximate", "optionalEdgesDropped", "truncated", "driftTruncated"} {
		if _, ok := m[key]; !ok {
			t.Errorf("%q key is absent when false — merge patch would retain prior true", key)
		}
		if string(m[key]) != "false" {
			t.Errorf("%q = %s, want false", key, string(m[key]))
		}
	}
}

// TestPluginInventoryStatus_MergePatchClearsPendingCollectRoundTrip verifies
// that a cleared PendingCollect actually ends up absent after a simulated
// merge-patch round-trip — i.e. setting it, then clearing it, produces an
// object where PendingCollect is nil.
func TestPluginInventoryStatus_MergePatchClearsPendingCollectRoundTrip(t *testing.T) {
	now := metav1.Now()

	// Start with a status that has PendingCollect set.
	orig := &PluginInventoryStatus{
		Hash:           "v1:abcdef",
		Source:         "jenkins-api",
		Total:          5,
		PendingCollect: &PendingCollect{CommandID: "cmd-1", IssuedAt: now},
	}

	// Simulate the merge-patch cycle: marshal the status as the patch would.
	// First patch: set PendingCollect.
	patch1, _ := json.Marshal(orig)

	// Parse it back — this is what the API server stores.
	var stored map[string]interface{}
	if err := json.Unmarshal(patch1, &stored); err != nil {
		t.Fatalf("unmarshal patch1: %v", err)
	}

	// Require that pendingCollect exists in stored.
	if _, ok := stored["pendingCollect"]; !ok {
		t.Fatal("pendingCollect not found in stored state after first patch")
	}

	// Second tick: clear PendingCollect.
	orig.PendingCollect = nil
	patch2, _ := json.Marshal(orig)

	var patch2Map map[string]interface{}
	if err := json.Unmarshal(patch2, &patch2Map); err != nil {
		t.Fatalf("unmarshal patch2: %v", err)
	}

	// Assert pendingCollect is null in the patch.
	if pc, ok := patch2Map["pendingCollect"]; !ok || pc != nil {
		t.Fatalf("pendingCollect in patch2: %v (want explicit null)", pc)
	}

	// Simulate merge-patch: for each key in patch2, replace or null-remove in stored.
	for k, v := range patch2Map {
		if v == nil {
			delete(stored, k)
		} else {
			stored[k] = v
		}
	}

	// pendingCollect should now be absent from stored.
	if _, ok := stored["pendingCollect"]; ok {
		t.Error("pendingCollect still present in stored after merge-patch with null — should be deleted")
	}

	// Verify stale cleared properly too.
	if _, ok := stored["stale"]; ok {
		v := stored["stale"]
		if v != false {
			t.Errorf("stale = %v, want false", v)
		}
	}
}
