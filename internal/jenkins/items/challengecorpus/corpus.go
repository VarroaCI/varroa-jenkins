// Package challengecorpus loads the challenge corpus manifest and checks it
// against the case directories on disk. Every case directory must have one
// manifest entry, and every entry must have one directory.
package challengecorpus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Case is one entry in manifest.yaml.
type Case struct {
	Name   string `yaml:"name"`
	Tier   string `yaml:"tier"`
	Status string `yaml:"status"`
}

// Manifest is the parsed content of manifest.yaml.
type Manifest struct {
	Cases []Case `yaml:"cases"`
}

var (
	validTiers    = map[string]bool{"breadth": true, "depth": true, "negative": true}
	validStatuses = map[string]bool{"supported": true, "known-gap": true}
)

// Dir returns base/cases/<tier>/<name>.
func (c Case) Dir(base string) string {
	return filepath.Join(base, "cases", c.Tier, c.Name)
}

// Load reads base/manifest.yaml and checks it against the base/cases tree.
// It reports every problem it finds in one error.
func Load(base string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(base, "manifest.yaml"))
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	var mf Manifest
	if err := yaml.Unmarshal(data, &mf); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest: %w", err)
	}

	var problems []string
	registered := make(map[string]bool)
	for i, c := range mf.Cases {
		if c.Name == "" || c.Tier == "" {
			problems = append(problems, fmt.Sprintf("manifest entry %d: name and tier are required", i))
			continue
		}
		if c.Name == "." || c.Name == ".." || strings.ContainsAny(c.Name, `/\`) {
			problems = append(problems, fmt.Sprintf("manifest entry %d: name must be one directory component", i))
			continue
		}
		key := c.Tier + "/" + c.Name
		if !validTiers[c.Tier] {
			problems = append(problems, fmt.Sprintf("manifest entry %s: unknown tier %q", key, c.Tier))
		}
		if !validStatuses[c.Status] {
			problems = append(problems, fmt.Sprintf("manifest entry %s: unknown status %q", key, c.Status))
		}
		if registered[key] {
			problems = append(problems, fmt.Sprintf("manifest entry %s: duplicate entry", key))
		}
		registered[key] = true
		info, err := os.Stat(c.Dir(base))
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("manifest entry %s: case directory cases/%s/%s does not exist", key, c.Tier, c.Name))
		case !info.IsDir():
			problems = append(problems, fmt.Sprintf("manifest entry %s: cases/%s/%s is not a directory", key, c.Tier, c.Name))
		}
	}

	tiers, err := os.ReadDir(filepath.Join(base, "cases"))
	if err != nil {
		return Manifest{}, fmt.Errorf("read cases directory: %w", err)
	}
	for _, t := range tiers {
		if !t.IsDir() {
			continue
		}
		if !validTiers[t.Name()] {
			problems = append(problems, fmt.Sprintf("cases/%s: unknown tier directory", t.Name()))
			continue
		}
		cases, err := os.ReadDir(filepath.Join(base, "cases", t.Name()))
		if err != nil {
			return Manifest{}, fmt.Errorf("read cases/%s: %w", t.Name(), err)
		}
		for _, c := range cases {
			if !c.IsDir() {
				continue
			}
			if !registered[t.Name()+"/"+c.Name()] {
				problems = append(problems, fmt.Sprintf("cases/%s/%s: case directory has no manifest.yaml entry", t.Name(), c.Name()))
			}
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return Manifest{}, errors.New(strings.Join(problems, "\n"))
	}
	return mf, nil
}
