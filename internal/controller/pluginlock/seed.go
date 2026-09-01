package pluginlock

import (
	"embed"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// yamlProfileSpec mirrors v1alpha1.JenkinsVersionProfileSpec's seedable fields.
// The tags are load-bearing, not decorative: JenkinsVersionProfileSpec carries
// only json tags, and yaml.v3 without an explicit yaml tag falls back to a
// lowercased field name (e.g. "resolveversion"), which would silently fail to
// match this package's own generated profile.yaml files.
type yamlProfileSpec struct {
	Version        string `yaml:"version"`
	Channel        string `yaml:"channel"`
	Recommended    bool   `yaml:"recommended"`
	EOL            string `yaml:"eol"`
	ResolveVersion string `yaml:"resolveVersion"`
}

// SeedProfile is one ship-time default JenkinsVersionProfile embedded in the
// operator binary, parsed from seed/<version>/{profile.yaml,plugins.yaml}.
type SeedProfile struct {
	// Version is the seed directory name (e.g. "2.555"), the source for the
	// JenkinsVersionProfile CR name and its pluginset ConfigMap name.
	Version string
	// Spec is copied from the parsed profile.yaml. PluginSetRef is left zero
	// here; the caller fills it in once the pluginset ConfigMap name is known.
	Spec v1alpha1.JenkinsVersionProfileSpec
	// Plugins is the raw plugins.yaml content, embedded verbatim into the
	// pluginset ConfigMap's data["plugins.yaml"] key.
	Plugins string
}

//go:embed seed
var seedFS embed.FS

var seeds []SeedProfile

func init() {
	entries, err := seedFS.ReadDir("seed")
	if err != nil {
		panic("pluginlock: failed to read embedded seed directory: " + err.Error())
	}
	if len(entries) == 0 {
		panic("pluginlock: embedded seed directory is empty")
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		version := entry.Name()

		profileBytes, err := seedFS.ReadFile("seed/" + version + "/profile.yaml")
		if err != nil {
			panic(fmt.Sprintf("pluginlock: failed to read seed/%s/profile.yaml: %s", version, err))
		}
		pluginsBytes, err := seedFS.ReadFile("seed/" + version + "/plugins.yaml")
		if err != nil {
			panic(fmt.Sprintf("pluginlock: failed to read seed/%s/plugins.yaml: %s", version, err))
		}
		if len(pluginsBytes) == 0 {
			panic("pluginlock: seed/" + version + "/plugins.yaml is empty")
		}

		var parsed yamlProfileSpec
		if err := yaml.Unmarshal(profileBytes, &parsed); err != nil {
			panic(fmt.Sprintf("pluginlock: failed to parse seed/%s/profile.yaml: %s", version, err))
		}
		if parsed.Version == "" {
			panic("pluginlock: seed/" + version + "/profile.yaml has no version")
		}

		seeds = append(seeds, SeedProfile{
			Version: version,
			Spec: v1alpha1.JenkinsVersionProfileSpec{
				Version:        parsed.Version,
				Channel:        parsed.Channel,
				Recommended:    parsed.Recommended,
				EOL:            parsed.EOL,
				ResolveVersion: parsed.ResolveVersion,
				PluginSetRef:   &v1alpha1.ConfigMapRef{Name: pluginSetName(version)},
			},
			Plugins: string(pluginsBytes),
		})
	}
	sort.Slice(seeds, func(i, j int) bool { return seeds[i].Version < seeds[j].Version })
}

// Seeds returns every ship-time default JenkinsVersionProfile embedded in the
// operator binary. The returned slice is a copy; order is stable (sorted by
// seed directory name) but not otherwise meaningful.
func Seeds() []SeedProfile {
	out := make([]SeedProfile, len(seeds))
	copy(out, seeds)
	return out
}

// ProfileName derives a seeded JenkinsVersionProfile's resource name from its
// version (e.g. "2.555" -> "jenkins-version-2-555") — the existing
// dots-to-dashes naming scheme, unchanged from the chart templates this
// package replaces. Exported so VersionProfileSeedReconciler names the
// profile CR the same way Seeds() names its embedded PluginSetRef, rather
// than reimplementing the substitution and risking drift.
func ProfileName(version string) string {
	return "jenkins-version-" + strings.ReplaceAll(version, ".", "-")
}

func pluginSetName(version string) string {
	return ProfileName(version) + "-pluginset"
}
