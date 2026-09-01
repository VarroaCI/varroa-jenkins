package v1alpha1

import (
	"encoding/json"
	"testing"
)

// TestBroodUpgradeActionDeepCopyIsolatesTargetVersion mirrors the other
// DeepCopy isolation tests: a populated BroodUpgradeAction is deep-copied,
// the COPY's pointer field is mutated, and the ORIGINAL must be unchanged.
func TestBroodUpgradeActionDeepCopyIsolatesTargetVersion(t *testing.T) {
	v := "2.570.2"
	orig := BroodUpgradeAction{TargetVersion: &v}

	cp := orig.DeepCopy()
	*cp.TargetVersion = "9.9.9"

	if *orig.TargetVersion != "2.570.2" {
		t.Fatalf("TargetVersion shallow-copied: original = %q, want 2.570.2", *orig.TargetVersion)
	}
}

// TestBroodUpgradeActionDeepCopyNilTargetVersion covers granularity B: a nil
// TargetVersion must round-trip through DeepCopy as nil, not a pointer to "".
func TestBroodUpgradeActionDeepCopyNilTargetVersion(t *testing.T) {
	orig := BroodUpgradeAction{}
	cp := orig.DeepCopy()
	if cp.TargetVersion != nil {
		t.Fatalf("TargetVersion = %v, want nil", cp.TargetVersion)
	}
}

// TestBroodUpgradePolicyDeepCopyIsolatesSlices mirrors ExecuteGroovyPolicy's
// own DeepCopy contract: AllowedNamespaces must be an independent backing
// array in the copy.
func TestBroodUpgradePolicyDeepCopyIsolatesSlices(t *testing.T) {
	enabled := true
	orig := BroodUpgradePolicy{
		Enabled:           &enabled,
		AllowedNamespaces: []string{"team-a"},
	}

	cp := orig.DeepCopy()
	cp.AllowedNamespaces[0] = "team-b"
	*cp.Enabled = false

	if orig.AllowedNamespaces[0] != "team-a" {
		t.Fatalf("AllowedNamespaces shallow-copied: original[0] = %q, want team-a", orig.AllowedNamespaces[0])
	}
	if !*orig.Enabled {
		t.Fatal("Enabled shallow-copied: original mutated by copy")
	}
}

// TestBroodActionUpgradeFieldShape guards the schema-level contract the CEL
// rule `self.verb == 'upgrade' ? has(self.upgrade) : !has(self.upgrade)`
// polices: granularity A and B both admit at the Go/JSON level, and a
// non-upgrade verb never carries an upgrade key for the CEL rule to trip on.
func TestBroodActionUpgradeFieldShape(t *testing.T) {
	t.Run("granularity A: targetVersion present", func(t *testing.T) {
		v := "2.570.2"
		action := BroodAction{Verb: BroodVerbUpgrade, Upgrade: &BroodUpgradeAction{TargetVersion: &v}}
		m := marshalToMap(t, action)
		upgrade, ok := m["upgrade"].(map[string]any)
		if !ok {
			t.Fatalf("expected an upgrade object, got %#v", m["upgrade"])
		}
		if upgrade["targetVersion"] != "2.570.2" {
			t.Errorf("targetVersion = %v, want 2.570.2", upgrade["targetVersion"])
		}
	})

	t.Run("granularity B: targetVersion absent", func(t *testing.T) {
		action := BroodAction{Verb: BroodVerbUpgrade, Upgrade: &BroodUpgradeAction{}}
		m := marshalToMap(t, action)
		upgrade, ok := m["upgrade"].(map[string]any)
		if !ok {
			t.Fatalf("expected an upgrade object, got %#v", m["upgrade"])
		}
		if _, ok := upgrade["targetVersion"]; ok {
			t.Error("targetVersion should be omitted when absent, not serialized as null/empty")
		}
	})

	t.Run("non-upgrade verb: no upgrade key", func(t *testing.T) {
		action := BroodAction{Verb: BroodVerbRestart}
		m := marshalToMap(t, action)
		if _, ok := m["upgrade"]; ok {
			t.Error("a restart action must not serialize an upgrade key")
		}
	})
}

func marshalToMap(t *testing.T, v any) map[string]any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return m
}
