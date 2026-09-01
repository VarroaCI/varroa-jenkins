package pluginlock

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSeeds_ParsesWithoutError verifies Seeds() returns exactly one entry per
// directory under seed/.
func TestSeeds_ParsesWithoutError(t *testing.T) {
	got := Seeds()
	if len(got) == 0 {
		t.Fatal("expected at least one seed profile")
	}

	entries, err := os.ReadDir("seed")
	if err != nil {
		t.Fatalf("failed to read seed directory: %v", err)
	}
	dirCount := 0
	for _, e := range entries {
		if e.IsDir() {
			dirCount++
		}
	}
	if len(got) != dirCount {
		t.Errorf("Seeds() returned %d entries, seed/ has %d directories", len(got), dirCount)
	}
}

// versionProfilesManifest mirrors the shape of hack/version-profiles.yaml —
// the reviewed source hack/gen-plugin-lock.sh generates seed/ from.
type versionProfilesManifest struct {
	Baseline string                         `yaml:"baseline"`
	Profiles []versionProfilesManifestEntry `yaml:"profiles"`
}

type versionProfilesManifestEntry struct {
	Version        string `yaml:"version"`
	Channel        string `yaml:"channel"`
	Recommended    bool   `yaml:"recommended"`
	EOL            string `yaml:"eol"`
	ResolveVersion string `yaml:"resolveVersion"`
}

// TestSeeds_RoundTripsAgainstVersionProfilesManifest compares each embedded
// seed entry field-by-field against the same version's entry in
// hack/version-profiles.yaml. This is the regression guard for
// yamlProfileSpec's load-bearing yaml tags: a missing or wrong tag would
// silently zero a field (e.g. resolveVersion) rather than fail to parse.
func TestSeeds_RoundTripsAgainstVersionProfilesManifest(t *testing.T) {
	data, err := os.ReadFile("../../../hack/version-profiles.yaml")
	if err != nil {
		t.Fatalf("failed to read hack/version-profiles.yaml: %v", err)
	}
	var manifest versionProfilesManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("failed to parse hack/version-profiles.yaml: %v", err)
	}
	want := make(map[string]versionProfilesManifestEntry, len(manifest.Profiles))
	for _, p := range manifest.Profiles {
		want[p.Version] = p
	}

	got := Seeds()
	if len(got) != len(want) {
		t.Fatalf("Seeds() returned %d entries, hack/version-profiles.yaml has %d", len(got), len(want))
	}

	for _, seed := range got {
		seed := seed
		t.Run(seed.Version, func(t *testing.T) {
			w, ok := want[seed.Version]
			if !ok {
				t.Fatalf("seed directory %q has no matching entry in hack/version-profiles.yaml", seed.Version)
			}
			if seed.Spec.Version != w.Version {
				t.Errorf("version: got %q, want %q", seed.Spec.Version, w.Version)
			}
			if seed.Spec.Channel != w.Channel {
				t.Errorf("channel: got %q, want %q", seed.Spec.Channel, w.Channel)
			}
			if seed.Spec.Recommended != w.Recommended {
				t.Errorf("recommended: got %v, want %v", seed.Spec.Recommended, w.Recommended)
			}
			if seed.Spec.EOL != w.EOL {
				t.Errorf("eol: got %q, want %q", seed.Spec.EOL, w.EOL)
			}

			// gen-plugin-lock.sh only writes resolveVersion into profile.yaml
			// when it's set AND differs from version; an exact pin with no
			// explicit resolveVersion in the manifest resolves against its own
			// tag, so the emitted field (and this expectation) is empty too.
			wantResolveVersion := w.ResolveVersion
			if wantResolveVersion == w.Version {
				wantResolveVersion = ""
			}
			if seed.Spec.ResolveVersion != wantResolveVersion {
				t.Errorf("resolveVersion: got %q, want %q", seed.Spec.ResolveVersion, wantResolveVersion)
			}

			wantProfileName := ProfileName(seed.Version)
			wantPluginSetName := wantProfileName + "-pluginset"
			if seed.Spec.PluginSetRef == nil {
				t.Fatal("PluginSetRef is nil")
			}
			if seed.Spec.PluginSetRef.Name != wantPluginSetName {
				t.Errorf("PluginSetRef.Name: got %q, want %q", seed.Spec.PluginSetRef.Name, wantPluginSetName)
			}
		})
	}
}
