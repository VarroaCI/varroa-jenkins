// Package pluginlock provides the pinned core plugin lockfile for use by the
// operator. The lockfile maps supported Jenkins core versions to fully-resolved
// plugin sets and is embedded at build time. It is never updated at runtime.
package pluginlock

import (
	_ "embed"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// PluginEntry is a pinned plugin artifact.
type PluginEntry struct {
	ArtifactID string `yaml:"artifactId" json:"artifactId"`
	Version    string `yaml:"version" json:"version"`
}

// BootstrapEntry is one member of the varroa-mite-auth bootstrap closure
// recorded at lock-generation time. The first entry of a set's bootstrap list
// is the closure root (varroa-mite-auth itself), which is baked into the image
// rather than resolved into the lock and therefore carries no Mins.
//
// Mins holds every minimum declared upon this plugin by anything in the
// closure, verbatim and de-duplicated. Nothing in this package compares them:
// a minimum is a floor, and comparing a pin against one belongs to the shared
// plugin-version package.
//
// The struct tags are load-bearing, not decorative: yaml.v3 would otherwise map
// ArtifactID to "artifactid" and never match the "artifactId" key the generator
// writes.
type BootstrapEntry struct {
	ArtifactID string   `yaml:"artifactId" json:"artifactId"`
	Version    string   `yaml:"version" json:"version"`
	Mins       []string `yaml:"mins,omitempty" json:"mins,omitempty"`
}

//go:embed lock.yaml
var lockData []byte

type lockFile struct {
	Baseline string             `yaml:"baseline"`
	Sets     map[string]lockSet `yaml:"sets"`
}

type lockSet struct {
	Core      []string         `yaml:"core"`
	Plugins   []PluginEntry    `yaml:"plugins"`
	Bootstrap []BootstrapEntry `yaml:"bootstrap"`
}

var parsedLock lockFile

func init() {
	if err := yaml.Unmarshal(lockData, &parsedLock); err != nil {
		panic("pluginlock: failed to parse embedded lock.yaml: " + err.Error())
	}
	if parsedLock.Baseline == "" || len(parsedLock.Sets) == 0 {
		panic("pluginlock: lock.yaml is empty or missing baseline")
	}
	for k := range parsedLock.Sets {
		if len(parsedLock.Sets[k].Plugins) == 0 {
			panic("pluginlock: lock set " + k + " has no plugins")
		}
	}
}

// Baseline returns the baseline Jenkins core version key from the lockfile.
func Baseline() string {
	return parsedLock.Baseline
}

// Resolve selects the pinned plugin set for the given Jenkins core version.
// Exact full-version keys return that set with matched=true. "lts" and ""
// return the baseline with matched=true. Unknown or non-full keys return
// the baseline with matched=false.
func Resolve(version string) (set []PluginEntry, matched bool) {
	v := strings.TrimSpace(version)
	if v == "" || v == "lts" {
		return resolveSet(parsedLock.Baseline, true)
	}
	if _, ok := parsedLock.Sets[v]; ok {
		return resolveSet(v, true)
	}
	return resolveSet(parsedLock.Baseline, false)
}

// Bootstrap returns the recorded varroa-mite-auth bootstrap closure for the
// given Jenkins core version. Key selection is IDENTICAL to Resolve — exact
// full-version key, then ""/"lts" falling back to the baseline, then the
// baseline — so a caller that resolved a plugin set for a version gets the
// bootstrap closure from the same set by construction. It is NOT the
// exact → LTS-line → baseline chain that lives in versionresolve.go.
//
// The first entry is the closure root; the rest are its members in BFS order.
func Bootstrap(version string) (entries []BootstrapEntry, matched bool) {
	v := strings.TrimSpace(version)
	if v == "" || v == "lts" {
		return bootstrapSet(parsedLock.Baseline, true)
	}
	if _, ok := parsedLock.Sets[v]; ok {
		return bootstrapSet(v, true)
	}
	return bootstrapSet(parsedLock.Baseline, false)
}

func bootstrapSet(key string, matched bool) ([]BootstrapEntry, bool) {
	s := parsedLock.Sets[key]
	out := make([]BootstrapEntry, len(s.Bootstrap))
	copy(out, s.Bootstrap)
	// Deep-copy the Mins slices so a caller cannot mutate the embedded lock.
	for i := range out {
		if out[i].Mins != nil {
			mins := make([]string, len(out[i].Mins))
			copy(mins, out[i].Mins)
			out[i].Mins = mins
		}
	}
	return out, matched
}

func resolveSet(key string, matched bool) ([]PluginEntry, bool) {
	s := parsedLock.Sets[key]
	out := make([]PluginEntry, len(s.Plugins))
	copy(out, s.Plugins)
	sort.Slice(out, func(i, j int) bool {
		return out[i].ArtifactID < out[j].ArtifactID
	})
	return out, matched
}
