package items

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Parse parses items YAML content into an Manifest and validates each item.
func Parse(yamlContent string) (*Manifest, error) {
	if strings.TrimSpace(yamlContent) == "" {
		return &Manifest{}, nil
	}

	var manifest Manifest
	if err := yaml.Unmarshal([]byte(yamlContent), &manifest); err != nil {
		return nil, fmt.Errorf("parse items.yaml: %w", err)
	}

	if err := validateItems(manifest.Items, ""); err != nil {
		return nil, err
	}

	return &manifest, nil
}

// EffectiveRemoveStrategy returns the remove strategy from the manifest,
// defaulting to "none" if not set.
func (m *Manifest) EffectiveRemoveStrategy() string {
	if m.RemoveStrategy != nil && m.RemoveStrategy.Items != "" {
		return m.RemoveStrategy.Items
	}
	return RemoveNone
}

// validateItems recursively validates all items.
func validateItems(items []Item, parent string) error {
	for i := range items {
		if err := items[i].Validate(); err != nil {
			if parent != "" {
				return fmt.Errorf("in %s: %w", parent, err)
			}
			return err
		}
		prefix := items[i].Name
		if parent != "" {
			prefix = parent + "/" + items[i].Name
		}
		if !items[i].IsFolder() && len(items[i].Items) > 0 {
			return fmt.Errorf("item %q (kind=%s): nested items only allowed on folders", items[i].Name, items[i].Kind)
		}
		if err := validateItems(items[i].Items, prefix); err != nil {
			return err
		}
	}
	return nil
}

// Flatten returns all items in depth-first order (folders before their children).
// Each item is paired with its full path (e.g. "parent-folder/child-job").
// If the manifest has a root path, it is prepended to all paths.
func (m *Manifest) Flatten() []ItemPath {
	var result []ItemPath
	flattenItems(m.Items, "", &result)
	if m.Root != "" {
		root := strings.Trim(m.Root, "/")
		for i := range result {
			result[i].Path = root + "/" + result[i].Path
		}
	}
	return result
}

// ItemPath pairs an item with its full slash-separated path.
type ItemPath struct {
	Item Item
	Path string
}

func flattenItems(items []Item, parent string, result *[]ItemPath) {
	for _, item := range items {
		path := item.Name
		if parent != "" {
			path = parent + "/" + item.Name
		}
		*result = append(*result, ItemPath{Item: item, Path: path})
		if item.IsFolder() {
			flattenItems(item.Items, path, result)
		}
	}
}
