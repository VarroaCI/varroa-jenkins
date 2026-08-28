package bundle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// CatalogIndex represents a parsed catalog index from catalog.yaml or
// directory convention scanning.
type CatalogIndex struct {
	APIVersion string             `yaml:"apiVersion"`
	Name       string             `yaml:"name"`
	Items      []CatalogIndexItem `yaml:"items"`
}

// CatalogIndexItem describes a single item in a template catalog.
type CatalogIndexItem struct {
	Type        string           `yaml:"type"` // podtemplate, plugin, item, jcasc, rbac, pipeline-template
	Name        string           `yaml:"name"`
	DisplayName string           `yaml:"displayName,omitempty"`
	Description string           `yaml:"description,omitempty"`
	Path        string           `yaml:"path"`
	Version     string           `yaml:"version,omitempty"`
	Tags        []string         `yaml:"tags,omitempty"`
	Variables   []CatalogVarDecl `yaml:"variables,omitempty"`
	Requires    []string         `yaml:"requires,omitempty"`
}

// CatalogVarDecl declares a variable that a catalog item requires.
type CatalogVarDecl struct {
	Name          string   `yaml:"name"`
	Default       string   `yaml:"default,omitempty"`
	Description   string   `yaml:"description,omitempty"`
	Required      bool     `yaml:"required,omitempty"`
	Type          string   `yaml:"type,omitempty"`
	AllowedValues []string `yaml:"allowedValues,omitempty"`
}

// ParseCatalogIndex reads the catalog index from root. It first attempts to
// parse <root>/catalog.yaml. If that file does not exist, it falls back to
// scanning the directory convention. If the directory convention finds nothing,
// an empty index is returned with no error.
func ParseCatalogIndex(root string) (*CatalogIndex, error) {
	catalogPath := filepath.Join(root, "catalog.yaml")
	// catalog.yaml itself must stay inside root: a committed symlink at this
	// fixed name would otherwise let the repo point the read anywhere on the
	// operator's filesystem. A missing file falls through to the directory
	// convention below, exactly as before.
	if resolved, rErr := ResolveContainedPath(root, "catalog.yaml"); rErr == nil {
		catalogPath = resolved
	} else if !errors.Is(rErr, os.ErrNotExist) {
		return nil, fmt.Errorf("catalog.yaml: %w", rErr)
	}
	data, err := os.ReadFile(catalogPath)
	if err == nil {
		var idx CatalogIndex
		if err := yaml.Unmarshal(data, &idx); err != nil {
			return nil, fmt.Errorf("parse catalog.yaml: %w", err)
		}
		// Reject unknown item types and non-local paths in explicit catalog.yaml.
		for _, item := range idx.Items {
			if !validCatalogItemType(item.Type) {
				return nil, fmt.Errorf("item %q: unknown type %q", item.Name, item.Type)
			}
			if err := validateRelPath(item.Path); err != nil {
				return nil, fmt.Errorf("item %q: %w", item.Name, err)
			}
		}
		return &idx, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read catalog.yaml: %w", err)
	}

	// Fall back to directory convention
	return scanDirectoryConvention(root)
}

// scanDirectoryConvention scans well-known subdirectories under root and builds
// a CatalogIndex from the YAML files found in each. Recognised directory names
// and their corresponding types:
//
//	pod-templates/       -> podtemplate
//	plugins/             -> plugin
//	items/               -> item
//	jcasc/               -> jcasc
//	rbac/                -> rbac
//	pipeline-templates/  -> pipeline-template
//
// Directories that do not exist are silently skipped. Unknown type directories
// cause an error. If no items are found, an empty index is returned.
func scanDirectoryConvention(root string) (*CatalogIndex, error) {
	type dirMapping struct {
		dirName  string
		itemType string
	}

	mappings := []dirMapping{
		{"pod-templates", "podtemplate"},
		{"plugins", "plugin"},
		{"items", "item"},
		{"jcasc", "jcasc"},
		{"rbac", "rbac"},
		{"pipeline-templates", "pipeline-template"},
	}

	var items []CatalogIndexItem

	for _, m := range mappings {
		dirPath := filepath.Join(root, m.dirName)

		entries, err := os.ReadDir(dirPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			// Unknown directory entries at the root level are an error if they
			// look like one of the mapping directories but can't be read.
			if os.IsPermission(err) {
				return nil, fmt.Errorf("read directory %s: %w", m.dirName, err)
			}
			continue
		}

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			ext := filepath.Ext(name)
			if ext != ".yaml" && ext != ".yml" {
				continue
			}

			basename := strings.TrimSuffix(name, ext)
			relPath := filepath.Join(m.dirName, name)

			items = append(items, CatalogIndexItem{
				Type: m.itemType,
				Name: basename,
				Path: relPath,
			})
		}
	}

	return &CatalogIndex{Items: items}, nil
}

// validCatalogItemType returns true if t is a recognised catalog item type.
func validCatalogItemType(t string) bool {
	switch t {
	case "podtemplate", "plugin", "item", "jcasc", "rbac", "pipeline-template":
		return true
	default:
		return false
	}
}
