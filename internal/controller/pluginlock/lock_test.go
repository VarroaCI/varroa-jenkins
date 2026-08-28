package pluginlock

import (
	"testing"
)

func TestResolve_ExactMatch(t *testing.T) {
	set, matched := Resolve("2.570")
	if !matched {
		t.Error("expected matched=true for exact version key")
	}
	if len(set) == 0 {
		t.Fatal("expected non-empty set")
	}
	found := false
	for _, e := range set {
		if e.ArtifactID == "configuration-as-code" {
			found = true
			if e.Version == "" {
				t.Errorf("configuration-as-code version must not be empty, got %q", e.Version)
			}
		}
	}
	if !found {
		t.Error("configuration-as-code not found in resolved set")
	}
}

func TestResolve_LTS(t *testing.T) {
	for _, v := range []string{"lts", ""} {
		set, matched := Resolve(v)
		if !matched {
			t.Errorf("expected matched=true for %q", v)
		}
		if len(set) == 0 {
			t.Errorf("expected non-empty set for %q", v)
		}
	}
}

func TestResolve_UnknownVersion(t *testing.T) {
	set, matched := Resolve("9.9.9")
	if matched {
		t.Error("expected matched=false for unknown version")
	}
	if len(set) == 0 {
		t.Fatal("expected fallback set for unknown version")
	}
	found := false
	for _, e := range set {
		if e.ArtifactID == "configuration-as-code" {
			found = true
		}
	}
	if !found {
		t.Error("fallback set must include configuration-as-code")
	}
}

func TestResolve_NonFullKey(t *testing.T) {
	// A partial version like "2.479" should not match and fall back.
	set, matched := Resolve("2.479")
	if matched {
		t.Error("expected matched=false for non-full key")
	}
	if len(set) == 0 {
		t.Fatal("expected fallback set")
	}
}

func TestParsedLock_SeedMembership(t *testing.T) {
	if parsedLock.Baseline == "" {
		t.Fatal("baseline is empty")
	}
	if _, ok := parsedLock.Sets[parsedLock.Baseline]; !ok {
		t.Fatal("baseline key not in sets")
	}
	for version, s := range parsedLock.Sets {
		t.Run(version, func(t *testing.T) {
			seed := map[string]bool{
				"configuration-as-code":   false,
				"role-strategy":           false,
				"instance-identity":       false,
				"kubernetes":              false,
				"workflow-aggregator":     false,
				"workflow-cps-global-lib": false,
				"mcp-server":              false,
				"job-dsl":                 false,
				"docker-workflow":         false,
				"github-branch-source":    false,
				"git":                     false,
				"nodejs":                  false,
				"jdk-tool":                false,
				"maven-plugin":            false,
				"config-file-provider":    false,
			}
			for _, id := range s.Core {
				if _, exists := seed[id]; !exists {
					t.Errorf("unexpected core seed plugin: %q", id)
				}
				seed[id] = true
			}
			for id, found := range seed {
				if !found {
					t.Errorf("core seed plugin %q not in lock set", id)
				}
			}

			pluginIDs := make(map[string]bool)
			for _, e := range s.Plugins {
				pluginIDs[e.ArtifactID] = true
			}
			for id := range seed {
				if !pluginIDs[id] {
					t.Errorf("seed plugin %q not found in resolved plugin set", id)
				}
			}
		})
	}
}

func TestResolve_SortedOutput(t *testing.T) {
	set, _ := Resolve("2.570")
	for i := 1; i < len(set); i++ {
		if set[i-1].ArtifactID >= set[i].ArtifactID {
			t.Errorf("output not sorted: %q >= %q", set[i-1].ArtifactID, set[i].ArtifactID)
		}
	}
}

// --- Bootstrap ------------------------------------------------------------

// TestBootstrap_KeySelectionMatchesResolve is the load-bearing property: a
// caller that resolved a plugin set for a version must get the bootstrap
// closure from the SAME set, by construction. Bootstrap therefore reuses
// Resolve's key selection (exact → ""/"lts" → baseline) and deliberately not
// the LTS-line chain that lives in versionresolve.go.
func TestBootstrap_KeySelectionMatchesResolve(t *testing.T) {
	for _, v := range []string{"2.570", "2.555", "", "lts", "9.9.9-unknown", "2.5"} {
		_, resolveMatched := Resolve(v)
		entries, bootstrapMatched := Bootstrap(v)
		if resolveMatched != bootstrapMatched {
			t.Errorf("version %q: Resolve matched=%v but Bootstrap matched=%v", v, resolveMatched, bootstrapMatched)
		}
		if len(entries) == 0 {
			t.Errorf("version %q: expected a non-empty bootstrap closure", v)
		}
	}
}

func TestBootstrap_RootFirstWithoutMins(t *testing.T) {
	entries, matched := Bootstrap("2.555")
	if !matched {
		t.Fatal("expected matched=true for an exact key")
	}
	if entries[0].ArtifactID != "varroa-mite-auth" {
		t.Fatalf("first entry must be the closure root, got %q", entries[0].ArtifactID)
	}
	if entries[0].Version == "" {
		t.Error("the root must carry a version read from its own manifest")
	}
	if len(entries[0].Mins) != 0 {
		t.Errorf("nothing declares a dependency on the root, so it carries no mins: %v", entries[0].Mins)
	}
}

// TestBootstrap_MembersArePinnedInTheSameSet checks the invariant --check
// enforces offline: every member is present in the set's plugin list at exactly
// the recorded version.
func TestBootstrap_MembersArePinnedInTheSameSet(t *testing.T) {
	for _, v := range []string{"2.555", "2.570"} {
		set, _ := Resolve(v)
		pins := map[string]string{}
		for _, p := range set {
			pins[p.ArtifactID] = p.Version
		}
		entries, _ := Bootstrap(v)
		for _, m := range entries[1:] {
			pin, ok := pins[m.ArtifactID]
			if !ok {
				t.Errorf("version %q: bootstrap member %q is absent from the resolved set", v, m.ArtifactID)
				continue
			}
			if pin != m.Version {
				t.Errorf("version %q: member %q records %q but the set pins %q", v, m.ArtifactID, m.Version, pin)
			}
			if len(m.Mins) == 0 {
				t.Errorf("version %q: member %q records no declared minimum", v, m.ArtifactID)
			}
		}
	}
}

// TestBootstrap_MandatoryClosureIsComplete pins the closure D9 exists to
// protect: mailer and its transitive mandatory dependencies. The optional
// configuration-as-code dependency must NOT be traversed.
func TestBootstrap_MandatoryClosureIsComplete(t *testing.T) {
	entries, _ := Bootstrap("2.555")
	got := map[string]bool{}
	for _, e := range entries {
		got[e.ArtifactID] = true
	}
	for _, want := range []string{"varroa-mite-auth", "mailer", "jakarta-mail-api", "instance-identity", "display-url-api"} {
		if !got[want] {
			t.Errorf("bootstrap closure is missing %q", want)
		}
	}
	if got["configuration-as-code"] {
		t.Error("configuration-as-code is an OPTIONAL dependency and must not be in the closure")
	}
}

// TestBootstrap_ReturnsACopy guards the embedded lock against caller mutation.
func TestBootstrap_ReturnsACopy(t *testing.T) {
	a, _ := Bootstrap("2.555")
	a[1].ArtifactID = "mutated"
	if len(a[1].Mins) > 0 {
		a[1].Mins[0] = "mutated"
	}
	b, _ := Bootstrap("2.555")
	if b[1].ArtifactID == "mutated" || (len(b[1].Mins) > 0 && b[1].Mins[0] == "mutated") {
		t.Error("Bootstrap must return a copy of the embedded lock")
	}
}
