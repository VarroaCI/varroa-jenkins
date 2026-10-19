package bundle

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/internal/bundle/pipelinetemplate"
)

// ValidateCatalogItem validates the content of a catalog item against
// structural rules for the given item type. indexVersion is the catalog
// index entry's declared version; only the pipeline-template case uses it.
//
// Supported types and their requirements:
//   - plugin:            must unmarshal into a struct with a non-empty plugins list;
//     each entry must have artifactId and version.
//   - jcasc:              must unmarshal into a non-empty map.
//   - podtemplate:        must unmarshal into a non-empty list of pod-template objects.
//   - item:               must unmarshal into a mapping with an items key; an
//     empty or null items list is valid and contributes no job.
//   - rbac:               must unmarshal into a struct with a non-empty roles map.
//   - pipeline-template:  content is one PipelineTemplate document, validated by
//     internal/bundle/pipelinetemplate; declared variables' Type/AllowedValues are
//     validated for internal consistency, and a declared variable warns because
//     pipeline-template content never resolves a ${var} placeholder.
func ValidateCatalogItem(itemType string, content []byte, variables []CatalogVarDecl, indexVersion string) (valid bool, message string) {
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
		return validatePipelineTemplateContent(content, variables, indexVersion)
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

// An empty items list is valid. A cell then holds an empty tenant folder.
func validateItemContent(content []byte) (bool, string) {
	var doc map[string]any
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return false, err.Error()
	}
	if _, ok := doc["items"]; !ok {
		return false, "items key is missing"
	}
	var ic itemContent
	if err := yaml.Unmarshal(content, &ic); err != nil {
		return false, err.Error()
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

// validatePipelineTemplateContent validates a pipeline-template item's
// content as a PipelineTemplate document (design D2, L1 in D11). It also
// validates the item's declared CatalogVariable list for Type/AllowedValues
// consistency, and warns — without invalidating — about two conditions the
// document alone cannot show: a declared variable never reaches the
// template body, and the catalog index's declared version disagrees with
// the document's own spec.version.
func validatePipelineTemplateContent(content []byte, variables []CatalogVarDecl, indexVersion string) (bool, string) {
	findings, doc := pipelinetemplate.ValidateDocument(content)
	if len(findings) > 0 {
		msgs := make([]string, len(findings))
		for i, f := range findings {
			msgs[i] = f.String()
		}
		return false, "pipeline-template content must be a PipelineTemplate document: " + strings.Join(msgs, "; ")
	}

	if valid, msg := validateVariableTypes(variables); !valid {
		return false, msg
	}

	var warnings []string
	for _, v := range variables {
		warnings = append(warnings, fmt.Sprintf(
			"variable %q does not reach the pipeline-template body: pipeline-templates.yaml is never resolved by ${var} substitution",
			v.Name))
	}
	if indexVersion != "" && doc.Spec.Version != "" && indexVersion != doc.Spec.Version {
		warnings = append(warnings, fmt.Sprintf(
			"catalog index declares version %q but the document declares spec.version %q; the document value is used",
			indexVersion, doc.Spec.Version))
	}
	return true, strings.Join(warnings, "; ")
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
