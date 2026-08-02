package main

import (
	"fmt"
	"io"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

const defaultLockPath = "internal/controller/pluginlock/lock.yaml"

// lockFile mirrors the shape internal/controller/pluginlock parses. It is
// re-declared here so --check reads the file on disk rather than the copy
// embedded into the operator binary at build time.
type lockFile struct {
	Baseline string             `yaml:"baseline"`
	Sets     map[string]lockSet `yaml:"sets"`
}

type lockSet struct {
	Plugins []struct {
		ArtifactID string `yaml:"artifactId"`
		Version    string `yaml:"version"`
	} `yaml:"plugins"`
	Bootstrap []struct {
		ArtifactID string   `yaml:"artifactId"`
		Version    string   `yaml:"version"`
		Mins       []string `yaml:"mins"`
	} `yaml:"bootstrap"`
}

// runCheck re-verifies every committed bootstrap block against its own set's
// committed plugin pins. It is offline and decidable from the file alone, which
// is what makes the assertion enforceable on a pull request where there is no
// network and no built HPI.
//
// It checks members only — every bootstrap entry except the first. The first is
// the closure root: baked into the image, never a lock member, no mins, so no
// pin-based check can apply to it. The root is verified only in --resolve,
// where its manifest is actually read.
//
// Two checks, and deliberately no third comparing a pin against the recorded
// minimums: that needs a version comparator this tool does not own. The
// minimums are recorded anyway so the check can be added later without a
// regeneration pass.
func runCheck(path string, stdout io.Writer) error {
	data, err := os.ReadFile(path) // #nosec G304 -- maintenance tool, path is a flag
	if err != nil {
		return fmt.Errorf("read lock: %w", err)
	}
	var lock lockFile
	if err := yaml.Unmarshal(data, &lock); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if len(lock.Sets) == 0 {
		return fmt.Errorf("%s declares no sets", path)
	}

	keys := make([]string, 0, len(lock.Sets))
	for k := range lock.Sets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var problems []string
	checked := 0
	for _, key := range keys {
		set := lock.Sets[key]
		if len(set.Bootstrap) == 0 {
			problems = append(problems, fmt.Sprintf("set %s: no bootstrap block — re-run hack/gen-plugin-lock.sh", key))
			continue
		}
		pins := make(map[string]string, len(set.Plugins))
		for _, p := range set.Plugins {
			pins[p.ArtifactID] = p.Version
		}
		for _, member := range set.Bootstrap[1:] {
			checked++
			pin, ok := pins[member.ArtifactID]
			if !ok {
				problems = append(problems, fmt.Sprintf(
					"set %s: bootstrap member %q is absent from the set's plugins — varroa-mite-auth will not load and Jenkins will not boot",
					key, member.ArtifactID))
				continue
			}
			if pin != member.Version {
				problems = append(problems, fmt.Sprintf(
					"set %s: bootstrap member %q records version %q but the set pins %q — a pin moved without re-running resolution",
					key, member.ArtifactID, member.Version, pin))
			}
		}
	}

	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintf(os.Stderr, "  %s\n", p)
		}
		return fmt.Errorf("%d bootstrap problem(s) in %s", len(problems), path)
	}

	fmt.Fprintf(stdout, "bootstrap closure OK: %d set(s), %d member(s) verified in %s\n", len(keys), checked, path)
	return nil
}
