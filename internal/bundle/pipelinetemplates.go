package bundle

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/internal/bundle/pipelinetemplate"
)

// pipelineTemplatesWrapper is the shape composePipelineTemplates writes to
// pipeline-templates.yaml: a list of PipelineTemplate documents under one
// top-level key.
type pipelineTemplatesWrapper struct {
	Templates []pipelinetemplate.Document `yaml:"templates"`
}

// ParsePipelineTemplates returns the documents of templatesYAML. A blank
// templatesYAML returns nil and no error.
func ParsePipelineTemplates(templatesYAML string) ([]pipelinetemplate.Document, error) {
	if strings.TrimSpace(templatesYAML) == "" {
		return nil, nil
	}
	var wrapper pipelineTemplatesWrapper
	if err := yaml.Unmarshal([]byte(templatesYAML), &wrapper); err != nil {
		return nil, fmt.Errorf("parse pipeline templates for injection: %w", err)
	}
	return wrapper.Templates, nil
}

// RenderPipelineTemplates writes docs in the pipeline-templates.yaml shape.
// It returns "" when docs is empty.
func RenderPipelineTemplates(docs []pipelinetemplate.Document) (string, error) {
	if len(docs) == 0 {
		return "", nil
	}
	out, err := yaml.Marshal(pipelineTemplatesWrapper{Templates: docs})
	if err != nil {
		return "", fmt.Errorf("render pipeline templates: %w", err)
	}
	return string(out), nil
}

// InjectPipelineTemplates appends every document in templatesYAML to
// unclassified.varroaPipelineTemplates.templates in jenkinsYAML. It stamps
// folder on each appended entry. It never replaces an existing entry, like
// InjectTenantClouds. The controller's own bundle and each placed tenant's
// fragment can therefore each call this once, in turn. A blank
// templatesYAML returns jenkinsYAML unchanged.
//
// Call this only after ResolveVars, and only after the unresolved-variable
// check. A template body holds its own ${...} Groovy text. That text is
// not a catalog variable, and it must not pass through either step.
func InjectPipelineTemplates(jenkinsYAML, templatesYAML, folder string) (string, error) {
	templates, err := ParsePipelineTemplates(templatesYAML)
	if err != nil {
		return "", err
	}
	if len(templates) == 0 {
		return jenkinsYAML, nil
	}

	var doc map[string]any
	if strings.TrimSpace(jenkinsYAML) != "" {
		if err := yaml.Unmarshal([]byte(jenkinsYAML), &doc); err != nil {
			return "", fmt.Errorf("parse jenkins yaml for pipeline template injection: %w", err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}

	unclassified, ok := doc["unclassified"].(map[string]any)
	if !ok {
		unclassified = map[string]any{}
		doc["unclassified"] = unclassified
	}

	store, ok := unclassified["varroaPipelineTemplates"].(map[string]any)
	if !ok {
		store = map[string]any{}
		unclassified["varroaPipelineTemplates"] = store
	}

	var existing []any
	if raw, exists := store["templates"]; exists {
		list, ok := raw.([]any)
		if !ok {
			return "", fmt.Errorf("jcasc: \"unclassified.varroaPipelineTemplates.templates\" is not a list (got %T)", raw)
		}
		existing = list
	}

	byIdentity := existingTemplatesByIdentity(existing)
	for _, t := range templates {
		entry, err := pipelineTemplateEntry(t, folder)
		if err != nil {
			return "", err
		}
		nameVersion := t.Metadata.Name + "@" + t.Spec.Version
		skip := false
		for _, prior := range byIdentity[nameVersion] {
			priorFolder, _ := prior["folder"].(string)
			if !overlappingFolders(priorFolder, folder) {
				// Disjoint sibling folders (design D7): the plugin never
				// resolves one tenant's template from another tenant's
				// folder, so their content is never compared and both
				// entries always coexist.
				continue
			}
			if templateEntryContentEqual(prior, entry) {
				// Overlapping folders and identical content is a
				// harmless duplicate (design D5): skip it rather than
				// injecting the same template twice.
				skip = true
				break
			}
			return "", fmt.Errorf(
				"pipeline template %q: %s and %s deliver different content for the same name and version",
				nameVersion, fragmentSourceLabel(prior["folder"]), fragmentSourceLabel(folder))
		}
		if skip {
			continue
		}
		existing = append(existing, entry)
		byIdentity[nameVersion] = append(byIdentity[nameVersion], entry)
	}
	store["templates"] = existing

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal jenkins yaml after pipeline template injection: %w", err)
	}
	return string(out), nil
}

// pipelineTemplateEntry renders one PipelineTemplate document into the
// flattened JCasC shape design D7 defines: parameters, uiSchema, and
// jenkinsfile travel as strings, so JCasC never interprets a template's own
// YAML. Exactly one of jenkinsfile and scmDefinition is set, mirroring the
// document's own exactly-one-of rule.
func pipelineTemplateEntry(doc pipelinetemplate.Document, folder string) (map[string]any, error) {
	parametersYAML, err := yaml.Marshal(doc.Spec.Parameters)
	if err != nil {
		return nil, fmt.Errorf("marshal template %q parameters: %w", doc.Metadata.Name, err)
	}
	entry := map[string]any{
		"name":       doc.Metadata.Name,
		"version":    doc.Spec.Version,
		"folder":     folder,
		"parameters": string(parametersYAML),
		"uiSchema":   "",
	}
	if doc.Spec.UISchema != nil {
		uiSchemaYAML, err := yaml.Marshal(doc.Spec.UISchema)
		if err != nil {
			return nil, fmt.Errorf("marshal template %q uiSchema: %w", doc.Metadata.Name, err)
		}
		entry["uiSchema"] = string(uiSchemaYAML)
	}
	if doc.Spec.SCMDefinition != nil {
		entry["scmDefinition"] = map[string]any{
			"remote":        doc.Spec.SCMDefinition.Remote,
			"credentialsId": doc.Spec.SCMDefinition.CredentialsID,
			"scriptPath":    doc.Spec.SCMDefinition.ScriptPath,
			"revision":      doc.Spec.SCMDefinition.Revision,
		}
	} else {
		entry["jenkinsfile"] = escapeJCasCVariables(doc.Spec.Jenkinsfile)
	}
	return entry, nil
}

// escapeJCasCVariables protects a pipeline body from JCasC's own variable
// substitution. JCasC resolves every "${name}" inside a string value
// against its secret sources and drops an unresolved one, so a body's
// "${varroaParams.greeting}" would reach Jenkins as an empty string. The
// "^${" form is JCasC's literal escape: Jenkins receives "${" again. A
// body that already carries the escape keeps it.
func escapeJCasCVariables(body string) string {
	if !strings.Contains(body, "${") {
		return body
	}
	var b strings.Builder
	b.Grow(len(body) + 8)
	for i := 0; i < len(body); i++ {
		if body[i] == '$' && i+1 < len(body) && body[i+1] == '{' {
			if i == 0 || body[i-1] != '^' {
				b.WriteByte('^')
			}
		}
		b.WriteByte(body[i])
	}
	return b.String()
}

// existingTemplatesByIdentity indexes already-injected template entries by
// "name@version", each key holding every entry sharing that name and
// version regardless of folder, so a fragment injected now can be checked
// against every prior entry at the same name and version: the controller's
// own bundle and each placed tenant's bundle each call
// InjectPipelineTemplates once, in turn, and design D5's identity rule
// applies across all of those calls, not only within one bundle's own
// inputs. overlappingFolders decides which prior entries actually share a
// runtime scope with the new one and must therefore carry identical
// content or collide; a disjoint prior entry is never compared.
func existingTemplatesByIdentity(existing []any) map[string][]map[string]any {
	byIdentity := make(map[string][]map[string]any, len(existing))
	for _, e := range existing {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		name, _ := entry["name"].(string)
		version, _ := entry["version"].(string)
		nameVersion := name + "@" + version
		byIdentity[nameVersion] = append(byIdentity[nameVersion], entry)
	}
	return byIdentity
}

// overlappingFolders reports whether two template entries' folders share a
// runtime scope, per design D7: an empty folder is controller-wide and
// reaches every tenant folder, so it overlaps every folder, not only the
// empty one, and two equal folders trivially overlap. Two different
// non-empty folders are disjoint sibling tenants, and the plugin never
// resolves a template across them.
func overlappingFolders(a, b string) bool {
	return a == b || a == "" || b == ""
}

// templateEntryContentEqual reports whether two template entries carry the
// same content, ignoring folder: design D5's identity rule compares every
// key but folder, since folder is part of an entry's identity, not its
// content.
func templateEntryContentEqual(a, b map[string]any) bool {
	return reflect.DeepEqual(withoutFolder(a), withoutFolder(b))
}

// withoutFolder returns a copy of entry with the "folder" key removed.
func withoutFolder(entry map[string]any) map[string]any {
	out := make(map[string]any, len(entry))
	for k, v := range entry {
		if k == "folder" {
			continue
		}
		out[k] = v
	}
	return out
}

// fragmentSourceLabel renders folder (a template entry's "folder" value,
// or the string passed to InjectPipelineTemplates) as a name for an error
// naming the two colliding sources.
func fragmentSourceLabel(folder any) string {
	f, _ := folder.(string)
	if f == "" {
		return "the controller's own bundle"
	}
	return fmt.Sprintf("tenant %q's bundle", f)
}
