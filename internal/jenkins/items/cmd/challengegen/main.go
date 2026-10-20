package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/items/challengecorpus"
)

type bundleYAML struct {
	ID          string   `yaml:"id"`
	APIVersion  string   `yaml:"apiVersion"`
	Version     string   `yaml:"version"`
	Description string   `yaml:"description"`
	JCasc       []string `yaml:"jcasc"`
	Plugins     []string `yaml:"plugins"`
	Items       []string `yaml:"items"`
}

type pluginEntry struct {
	ArtifactID string `yaml:"artifactId"`
	Version    string `yaml:"version"`
}

type pluginsYAML struct {
	Plugins []pluginEntry `yaml:"plugins"`
}

func main() {
	base := "internal/jenkins/items/testdata/challenge"
	outDir := filepath.Join(base, "bundle-challenge")

	// Load the manifest and check it against the case directories.
	mf, err := challengecorpus.Load(base)
	if err != nil {
		die("challenge corpus: %v", err)
	}

	// Collect supported cases.
	var wrappers []items.Item
	for _, c := range mf.Cases {
		if c.Status != "supported" {
			continue
		}
		itemsData, err := os.ReadFile(filepath.Join(c.Dir(base), "items.yaml"))
		if err != nil {
			die("read items.yaml for %s/%s: %v", c.Tier, c.Name, err)
		}
		m, err := items.Parse(string(itemsData))
		if err != nil {
			die("parse items.yaml for %s/%s: %v", c.Tier, c.Name, err)
		}
		wrapper := items.Item{
			Kind:  "folder",
			Name:  "challenge-" + c.Name,
			Items: m.Items,
		}
		wrappers = append(wrappers, wrapper)
	}

	// The generator owns every file in outDir. A removal first makes a file it no longer writes show as a deletion.
	if err := os.RemoveAll(outDir); err != nil {
		die("remove %s: %v", outDir, err)
	}
	// Write bundle-challenge/items.yaml.
	if err := os.MkdirAll(outDir, 0755); err != nil {
		die("mkdir %s: %v", outDir, err)
	}
	bundleManifest := items.Manifest{
		Items: wrappers,
	}
	itemsData, err := yaml.Marshal(bundleManifest)
	if err != nil {
		die("marshal bundle items: %v", err)
	}
	// yaml.Marshal puts "items:\n" but we need proper structure.
	// Write as a raw YAML document.
	// The yaml.Marshal of Manifest works fine since the struct has yaml tags.
	if err := os.WriteFile(filepath.Join(outDir, "items.yaml"), itemsData, 0644); err != nil {
		die("write items.yaml: %v", err)
	}

	// Write bundle.yaml.
	bundle := bundleYAML{
		ID:          "bundle-challenge",
		APIVersion:  "2",
		Version:     "1",
		Description: "Generated challenge corpus bundle (items-challenge-corpus)",
		JCasc:       []string{"jenkins.yaml"},
		Plugins:     []string{"plugins.yaml"},
		Items:       []string{"items.yaml"},
	}
	bundleData, err := yaml.Marshal(bundle)
	if err != nil {
		die("marshal bundle.yaml: %v", err)
	}
	// Prepend a YAML separator for the bundle format.
	finalBundle := append([]byte("---\n"), bundleData...)
	if err := os.WriteFile(filepath.Join(outDir, "bundle.yaml"), finalBundle, 0644); err != nil {
		die("write bundle.yaml: %v", err)
	}

	// Write jenkins.yaml (minimal).
	jenkinsData := []byte(`---
jenkins:
  systemMessage: "Varroa Challenge Corpus — do not use for production"
`)
	if err := os.WriteFile(filepath.Join(outDir, "jenkins.yaml"), jenkinsData, 0644); err != nil {
		die("write jenkins.yaml: %v", err)
	}

	// Write plugins.yaml (D8 plugin set). Bundle schema requires {artifactId, version}
	// entries (the bundle validator rejects bare-string plugin entries).
	pluginIDs := []string{
		"cloudbees-folder",
		"envinject",
		"workflow-job",
		"workflow-cps",
		"workflow-multibranch",
		"branch-api",
		"git",
		"git-client",
		"github-branch-source",
		"gitea",
		"pipeline-groovy-lib",
		"pipeline-model-definition",
		"credentials",
		"plain-credentials",
		"ssh-credentials",
		"junit",
		"mailer",
	}
	plugins := pluginsYAML{Plugins: make([]pluginEntry, 0, len(pluginIDs))}
	for _, id := range pluginIDs {
		plugins.Plugins = append(plugins.Plugins, pluginEntry{ArtifactID: id, Version: "latest"})
	}
	pluginsData, err := yaml.Marshal(plugins)
	if err != nil {
		die("marshal plugins.yaml: %v", err)
	}
	finalPlugins := append([]byte("---\n"), pluginsData...)
	if err := os.WriteFile(filepath.Join(outDir, "plugins.yaml"), finalPlugins, 0644); err != nil {
		die("write plugins.yaml: %v", err)
	}

	fmt.Println("Generated bundle-challenge/ with", len(wrappers), "supported cases")
}

func die(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
