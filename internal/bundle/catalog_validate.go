package bundle

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
)

// ValidateCatalogItem validates the content of a catalog item against
// structural rules for the given item type.
//
// Supported types and their requirements:
//   - plugin:            must unmarshal into a struct with a non-empty plugins list;
//     each entry must have artifactId and version.
//   - jcasc:              must unmarshal into a non-empty map.
//   - podtemplate:        must unmarshal into a non-empty list of pod-template objects.
//   - item:               must unmarshal into a struct with a non-empty items list.
//   - rbac:               must unmarshal into a struct with a non-empty roles map.
//   - pipeline-template:  must unmarshal into an items.Manifest whose items are all
//     kind=pipeline or kind=multibranch, validated via (*items.Item).Validate(); declared
//     variables' Type/AllowedValues are also validated for internal consistency.
func ValidateCatalogItem(itemType string, content []byte, variables []CatalogVarDecl) (valid bool, message string) {
	switch itemType {
	case "plugin":
		return validatePluginContent(content)
	case "jcasc":
		return validateJCasCContent(content)
	case "podtemplate":
		return validatePodTemplateContent(content)
	case "item":
		return validateItemContent(content)
	case "rbac":
		return validateRBACContent(content)
	case "pipeline-template":
		return validatePipelineTemplateContent(content, variables)
	default:
		return false, fmt.Sprintf("unknown type: %s", itemType)
	}
}

// pluginContent is the expected structure of a plugins.yaml file.
type pluginContent struct {
	Plugins []struct {
		ArtifactID string `yaml:"artifactId"`
		Version    string `yaml:"version"`
	} `yaml:"plugins"`
}

func validatePluginContent(content []byte) (bool, string) {
	var pc pluginContent
	if err := yaml.Unmarshal(content, &pc); err != nil {
		return false, err.Error()
	}
	if len(pc.Plugins) == 0 {
		return false, "plugins list is empty"
	}
	for i, p := range pc.Plugins {
		if p.ArtifactID == "" {
			return false, fmt.Sprintf("plugin[%d]: artifactId is required", i)
		}
		if p.Version == "" {
			return false, fmt.Sprintf("plugin[%d]: version is required", i)
		}
	}
	return true, ""
}

func validateJCasCContent(content []byte) (bool, string) {
	var doc map[string]any
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return false, err.Error()
	}
	if len(doc) == 0 {
		return false, "jcasc content is empty"
	}
	return true, ""
}

func validatePodTemplateContent(content []byte) (bool, string) {
	var entries []map[string]any
	if err := yaml.Unmarshal(content, &entries); err != nil {
		return false, err.Error()
	}
	if len(entries) == 0 {
		return false, "pod template list is empty"
	}
	return true, ""
}

// itemContent is the expected structure of an items.yaml file.
type itemContent struct {
	Items []map[string]any `yaml:"items"`
}

func validateItemContent(content []byte) (bool, string) {
	var ic itemContent
	if err := yaml.Unmarshal(content, &ic); err != nil {
		return false, err.Error()
	}
	if len(ic.Items) == 0 {
		return false, "items list is empty"
	}
	return true, ""
}

// rbacContent is the expected structure of an rbac.yaml file.
type rbacContent struct {
	Roles map[string]any `yaml:"roles"`
}

func validateRBACContent(content []byte) (bool, string) {
	var rc rbacContent
	if err := yaml.Unmarshal(content, &rc); err != nil {
		return false, err.Error()
	}
	if len(rc.Roles) == 0 {
		return false, "roles map is empty"
	}
	return true, ""
}

func validatePipelineTemplateContent(content []byte, variables []CatalogVarDecl) (bool, string) {
	var m items.Manifest
	if err := yaml.Unmarshal(content, &m); err != nil {
		return false, err.Error()
	}
	if len(m.Items) == 0 {
		return false, "items list is empty"
	}
	for i := range m.Items {
		kind := m.Items[i].Kind
		if kind != "pipeline" && kind != "multibranch" {
			return false, fmt.Sprintf("item[%d] %q: pipeline-template requires kind=pipeline or kind=multibranch, got %q", i, m.Items[i].Name, kind)
		}
		if err := m.Items[i].Validate(); err != nil {
			return false, err.Error()
		}
	}
	return validateVariableTypes(variables)
}

// validateVariableTypes checks Type/AllowedValues internal consistency on a
// set of declared catalog variables. It imposes no per-item-type restriction
// itself — callers decide which CatalogItemType's validation wires it in.
// Currently only the pipeline-template case (above) calls this.
func validateVariableTypes(variables []CatalogVarDecl) (bool, string) {
	for _, v := range variables {
		switch v.Type {
		case "", "string", "number", "boolean", "credentials":
		default:
			return false, fmt.Sprintf("variable %q: unknown type %q", v.Name, v.Type)
		}
		if len(v.AllowedValues) > 0 && v.Type != "" && v.Type != "string" && v.Type != "number" {
			return false, fmt.Sprintf("variable %q: allowedValues is not valid for type %q", v.Name, v.Type)
		}
	}
	return true, ""
}
