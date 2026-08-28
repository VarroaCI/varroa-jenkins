package bundle

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest represents a bundle.yaml file as defined by the CloudBees
// Configuration as Code bundle specification.
type Manifest struct {
	ID                  string              `yaml:"id"`
	Version             string              `yaml:"version"`
	APIVersion          string              `yaml:"apiVersion"`
	Description         string              `yaml:"description,omitempty"`
	Parent              string              `yaml:"parent,omitempty"`
	AvailabilityPattern string              `yaml:"availabilityPattern,omitempty"`
	Jcasc               []string            `yaml:"jcasc"`
	JcascMergeStrategy  string              `yaml:"jcascMergeStrategy,omitempty"`
	Plugins             []string            `yaml:"plugins,omitempty"`
	Items               []string            `yaml:"items,omitempty"`
	Rbac                []string            `yaml:"rbac,omitempty"`
	Variables           []string            `yaml:"variables,omitempty"`
	ItemRemoveStrategy  *ItemRemoveStrategy `yaml:"itemRemoveStrategy,omitempty"`
	RbacRemoveStrategy  string              `yaml:"rbacRemoveStrategy,omitempty"`
}

// ItemRemoveStrategy defines how existing items and RBAC assignments are
// handled when applying an items.yaml manifest.
type ItemRemoveStrategy struct {
	Items string `yaml:"items,omitempty"`
	Rbac  string `yaml:"rbac,omitempty"`
}

// ParseManifest reads and validates a bundle.yaml file from bundleDir.
func ParseManifest(bundleDir string) (*Manifest, error) {
	path := filepath.Join(bundleDir, "bundle.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bundle.yaml is required but missing: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("bundle.yaml is empty")
	}

	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("invalid bundle.yaml: %w", err)
	}

	if err := m.validate(bundleDir); err != nil {
		return nil, err
	}

	return &m, nil
}

func (m *Manifest) validate(bundleDir string) error {
	// Required fields
	if strings.TrimSpace(m.ID) == "" {
		return fmt.Errorf("bundle.yaml: id is required")
	}
	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("bundle.yaml: version is required")
	}
	if strings.TrimSpace(m.APIVersion) == "" {
		return fmt.Errorf("bundle.yaml: apiVersion is required")
	}

	// apiVersion must be "1" or "2"
	if m.APIVersion != "1" && m.APIVersion != "2" {
		return fmt.Errorf("bundle.yaml: apiVersion must be \"1\" or \"2\", got %q", m.APIVersion)
	}

	// jcasc must reference at least one file
	if len(m.Jcasc) == 0 {
		return fmt.Errorf("bundle.yaml: jcasc must reference at least one jenkins.yaml file")
	}

	// jcascMergeStrategy
	if m.JcascMergeStrategy != "" &&
		m.JcascMergeStrategy != "errorOnConflict" &&
		m.JcascMergeStrategy != "override" {
		return fmt.Errorf("bundle.yaml: jcascMergeStrategy must be \"errorOnConflict\" or \"override\", got %q", m.JcascMergeStrategy)
	}

	// itemRemoveStrategy.items
	if m.ItemRemoveStrategy != nil {
		items := m.ItemRemoveStrategy.Items
		if items != "" && items != "none" && items != "sync" && items != "remove-supported" && items != "remove-all" {
			return fmt.Errorf("bundle.yaml: itemRemoveStrategy.items must be one of \"none\", \"sync\", \"remove-supported\", \"remove-all\", got %q", items)
		}
		rbac := m.ItemRemoveStrategy.Rbac
		if rbac != "" && rbac != "sync" && rbac != "update" {
			return fmt.Errorf("bundle.yaml: itemRemoveStrategy.rbac must be \"sync\" or \"update\", got %q", rbac)
		}
	}

	// rbacRemoveStrategy
	if m.RbacRemoveStrategy != "" &&
		m.RbacRemoveStrategy != "sync" &&
		m.RbacRemoveStrategy != "update" {
		return fmt.Errorf("bundle.yaml: rbacRemoveStrategy must be \"sync\" or \"update\", got %q", m.RbacRemoveStrategy)
	}

	// Validate all referenced files exist
	allPaths := append([]string{}, m.Jcasc...)
	allPaths = append(allPaths, m.Plugins...)
	allPaths = append(allPaths, m.Items...)
	allPaths = append(allPaths, m.Rbac...)
	allPaths = append(allPaths, m.Variables...)

	for _, p := range allPaths {
		if _, err := ResolveContainedPath(bundleDir, p); err != nil {
			return fmt.Errorf("bundle.yaml: referenced file %q: %w", p, err)
		}
	}

	return nil
}

// readFiles reads and concatenates the content of all files referenced by
// the given paths. If a path is a directory, all .yaml/.yml files in it
// (non-recursive) are read. If paths is empty, returns an empty string.
func readFiles(bundleDir string, paths []string) (string, error) {
	if len(paths) == 0 {
		return "", nil
	}
	var parts []string
	for _, p := range paths {
		fullPath := filepath.Join(bundleDir, p)
		fi, err := os.Stat(fullPath)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", p, err)
		}
		if fi.IsDir() {
			entries, err := os.ReadDir(fullPath)
			if err != nil {
				return "", fmt.Errorf("read dir %s: %w", p, err)
			}
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				name := e.Name()
				ext := filepath.Ext(name)
				if ext == ".yaml" || ext == ".yml" {
					safePath, err := ResolveContainedPath(fullPath, name)
					if err != nil {
						return "", fmt.Errorf("%s/%s: %w", p, name, err)
					}
					data, err := os.ReadFile(safePath)
					if err != nil {
						return "", fmt.Errorf("read %s/%s: %w", p, name, err)
					}
					parts = append(parts, string(data))
				}
			}
		} else {
			data, err := os.ReadFile(fullPath)
			if err != nil {
				return "", fmt.Errorf("read %s: %w", p, err)
			}
			parts = append(parts, string(data))
		}
	}
	return strings.Join(parts, "\n"), nil
}

// mergeJcasc reads and merges multiple JCasC YAML files according to the
// specified strategy. For a single file, it returns the content directly.
func mergeJcasc(bundleDir string, paths []string, strategy string) (string, error) {
	if len(paths) == 1 {
		data, err := os.ReadFile(filepath.Join(bundleDir, paths[0]))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", paths[0], err)
		}
		return string(data), nil
	}

	var merged map[string]any
	for _, p := range paths {
		data, err := os.ReadFile(filepath.Join(bundleDir, p))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", p, err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return "", fmt.Errorf("parse %s: %w", p, err)
		}
		if merged == nil {
			merged = doc
		} else {
			if err := mergeMaps(merged, doc, strategy); err != nil {
				return "", fmt.Errorf("merge %s: %w", p, err)
			}
		}
	}

	out, err := yaml.Marshal(merged)
	if err != nil {
		return "", fmt.Errorf("marshal merged jcasc: %w", err)
	}
	return string(out), nil
}

// mergeMaps merges src into dst. With "errorOnConflict", it returns an error
// if both maps define the same key. With "override", src wins on conflict.
func mergeMaps(dst, src map[string]any, strategy string) error {
	for k, srcVal := range src {
		if dstVal, exists := dst[k]; exists {
			dstMap, dstIsMap := dstVal.(map[string]any)
			srcMap, srcIsMap := srcVal.(map[string]any)
			if dstIsMap && srcIsMap {
				if err := mergeMaps(dstMap, srcMap, strategy); err != nil {
					return fmt.Errorf("%s: %w", k, err)
				}
				continue
			}
			if strategy == "errorOnConflict" {
				return fmt.Errorf("duplicate key %q (use jcascMergeStrategy: override to allow)", k)
			}
		}
		dst[k] = srcVal
	}
	return nil
}
