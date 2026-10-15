package controller

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
)

// tenantPodTemplateLift is what liftTenantPodTemplates returns for one
// tenant: the stamped pod templates in bundle order, and the
// BundleKeysIgnored and PodTemplateSkipped warnings.
type tenantPodTemplateLift struct {
	Templates []bundle.TenantCloudTemplate
	Warnings  []v1alpha1.TenantWarning
}

// tenantPodTemplateStampedKeys are the keys the operator writes on a lifted
// template, plus "labels". The kubernetes plugin attribute is the singular
// "label", and a "labels" key fails the JCasC reload of the whole cell.
var tenantPodTemplateStampedKeys = []string{"name", "namespace", "serviceAccount", "label", "labels"}

// tenantPodTemplateEntry is one raw entry of a kubernetes cloud's templates
// list, with its path in the tenant's jenkins.yaml.
type tenantPodTemplateEntry struct {
	path  string
	value any
}

// liftTenantPodTemplates reads the pod templates of every kubernetes cloud
// in a tenant's composed jenkins.yaml, in bundle order, and stamps each one
// for the tenant cloud. It resolves the tenant's own bundle variables in
// each template once, before every other rule, and never lifts a template
// that still holds "${" after that pass. The cell's ResolveVars and its
// unresolved-variable check see the lifted text, so a leftover reference
// could read a cell value or fail the cell for every tenant. It records one
// BundleKeysIgnored warning for jenkins.yaml content it does not use, and
// one PodTemplateSkipped warning for each template it does not lift. It
// never returns an error: one tenant's content must not fail the cell.
func liftTenantPodTemplates(team, agentNamespace, jenkinsYAML string, vars bundle.Variables) tenantPodTemplateLift {
	var lift tenantPodTemplateLift
	if strings.TrimSpace(jenkinsYAML) == "" {
		return lift
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(jenkinsYAML), &doc); err != nil {
		lift.Warnings = append(lift.Warnings, v1alpha1.TenantWarning{
			Tenant:  team,
			Reason:  v1alpha1.TeamReasonBundleKeysIgnored,
			Message: fmt.Sprintf("tenant bundle key %q does not parse: %v", "jenkins.yaml", err),
		})
		return lift
	}

	entries, residue := collectTenantPodTemplates(doc)
	if len(residue) > 0 {
		sort.Strings(residue)
		lift.Warnings = append(lift.Warnings, v1alpha1.TenantWarning{
			Tenant: team,
			Reason: v1alpha1.TeamReasonBundleKeysIgnored,
			Message: fmt.Sprintf("tenant bundle key %q is ignored on a cell except its pod templates: %s",
				"jenkins.yaml", strings.Join(residue, ", ")),
		})
	}

	skip := func(label, cause string) {
		lift.Warnings = append(lift.Warnings, v1alpha1.TenantWarning{
			Tenant:  team,
			Reason:  v1alpha1.TeamReasonPodTemplateSkipped,
			Message: label + ": " + cause,
		})
	}

	type kept struct {
		name string
		raw  map[string]any
	}
	var keptTemplates []kept
	seen := map[string]bool{}
	resolver := bundle.VarsReplacer(vars)
	for _, e := range entries {
		if _, ok := e.value.(map[string]any); !ok {
			skip(e.path, "entry is not a map")
			continue
		}
		resolved := resolveTemplateStrings(e.value, resolver).(map[string]any)

		label := e.path
		name, nameIsString := resolved["name"].(string)
		if nameIsString && name != "" {
			label = fmt.Sprintf("pod template %q", name)
		}

		if holdsInterpolation(resolved) {
			skip(label, `a value or key holds "${" after the tenant's variables resolve`)
			continue
		}
		rawName, hasName := resolved["name"]
		switch {
		case !hasName || rawName == nil || (nameIsString && name == ""):
			skip(label, "name is missing or empty")
			continue
		case !nameIsString:
			skip(label, "name is not a string")
			continue
		}
		if name == "default" {
			skip(label, `the name "default" is reserved`)
			continue
		}
		if l, ok := resolved["label"]; ok {
			if _, isString := l.(string); !isString {
				skip(label, "label is not a string")
				continue
			}
		}
		if seen[name] {
			skip(label, "an earlier template has the same name")
			continue
		}
		seen[name] = true
		keptTemplates = append(keptTemplates, kept{name: name, raw: resolved})
	}

	for _, k := range keptTemplates {
		tenantLabel, _ := k.raw["label"].(string)
		raw := make(map[string]any, len(k.raw))
		for key, v := range k.raw {
			raw[key] = v
		}
		for _, key := range tenantPodTemplateStampedKeys {
			delete(raw, key)
		}
		if inherit, ok := raw["inheritFrom"].(string); ok {
			raw["inheritFrom"] = rewriteInheritFrom(team, inherit, seen)
		}
		lift.Templates = append(lift.Templates, bundle.TenantCloudTemplate{
			Name:           team + "-" + k.name,
			Namespace:      agentNamespace,
			ServiceAccount: tenantAgentServiceAccountName(team),
			Labels:         rewriteTenantLabel(team, tenantLabel),
			Raw:            raw,
		})
	}
	return lift
}

// collectTenantPodTemplates walks jenkins.clouds in list order and each
// kubernetes cloud's templates list in list order. It returns every
// template entry with its path, and every residue path: content that the
// lift does not use. The composer's minimal cloud holds the key
// "templates" only, so any other key is residue.
func collectTenantPodTemplates(doc map[string]any) ([]tenantPodTemplateEntry, []string) {
	var (
		entries []tenantPodTemplateEntry
		residue []string
	)
	for key := range doc {
		if key != "jenkins" {
			residue = append(residue, key)
		}
	}
	rawJenkins, ok := doc["jenkins"]
	if !ok {
		return nil, residue
	}
	jenkins, ok := rawJenkins.(map[string]any)
	if !ok {
		return nil, append(residue, "jenkins")
	}
	for key := range jenkins {
		if key != "clouds" {
			residue = append(residue, "jenkins."+key)
		}
	}
	rawClouds, ok := jenkins["clouds"]
	if !ok {
		return nil, residue
	}
	clouds, ok := rawClouds.([]any)
	if !ok {
		return nil, append(residue, "jenkins.clouds")
	}
	for i, rawCloud := range clouds {
		cloudPath := fmt.Sprintf("jenkins.clouds[%d]", i)
		cloud, ok := rawCloud.(map[string]any)
		if !ok {
			residue = append(residue, cloudPath)
			continue
		}
		k8s, ok := cloud["kubernetes"].(map[string]any)
		if !ok {
			residue = append(residue, cloudPath)
			continue
		}
		for key := range cloud {
			if key != "kubernetes" {
				residue = append(residue, cloudPath+"."+key)
			}
		}
		for key := range k8s {
			if key != "templates" {
				residue = append(residue, cloudPath+".kubernetes."+key)
			}
		}
		rawTemplates, ok := k8s["templates"]
		if !ok {
			continue
		}
		templates, ok := rawTemplates.([]any)
		if !ok {
			residue = append(residue, cloudPath+".kubernetes.templates")
			continue
		}
		for j, t := range templates {
			entries = append(entries, tenantPodTemplateEntry{
				path:  fmt.Sprintf("%s.kubernetes.templates[%d]", cloudPath, j),
				value: t,
			})
		}
	}
	return entries, residue
}

// resolveTemplateStrings returns a deep copy of v with the tenant's
// variables replaced in every string value in its maps and lists. Map keys
// stay as written.
func resolveTemplateStrings(v any, resolver *strings.Replacer) any {
	switch t := v.(type) {
	case string:
		return resolver.Replace(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = resolveTemplateStrings(val, resolver)
		}
		return out
	case map[any]any:
		out := make(map[any]any, len(t))
		for k, val := range t {
			out[k] = resolveTemplateStrings(val, resolver)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = resolveTemplateStrings(val, resolver)
		}
		return out
	default:
		return v
	}
}

// holdsInterpolation reports whether any string value or map key in v
// holds "${". An escaped "^${" counts too, because ResolveVars replaces a
// reference inside it.
func holdsInterpolation(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.Contains(t, "${")
	case map[string]any:
		for k, val := range t {
			if strings.Contains(k, "${") || holdsInterpolation(val) {
				return true
			}
		}
	case map[any]any:
		for k, val := range t {
			if holdsInterpolation(k) || holdsInterpolation(val) {
				return true
			}
		}
	case []any:
		for _, val := range t {
			if holdsInterpolation(val) {
				return true
			}
		}
	}
	return false
}

// rewriteTenantLabel renders a lifted template's label: the reserved node
// label the agent fence reads, then one "<team>-<token>" for each token of
// the tenant's label, in the tenant's order. It drops a tenant token that
// starts with "varroa-tenant-", so a tenant cannot claim another tenant's
// reserved label.
func rewriteTenantLabel(team, tenantLabel string) string {
	tokens := []string{tenantNodeLabel(team)}
	for _, tok := range strings.Fields(tenantLabel) {
		if strings.HasPrefix(tok, "varroa-tenant-") {
			continue
		}
		tokens = append(tokens, team+"-"+tok)
	}
	return strings.Join(tokens, " ")
}

// rewriteInheritFrom renames each token of an inheritFrom value that names
// a kept template to that template's "<team>-<name>" form. The lift renames
// every kept template, so a token left as written names nothing on the
// tenant cloud. Every other token stays as written.
func rewriteInheritFrom(team, inheritFrom string, keptNames map[string]bool) string {
	tokens := strings.Fields(inheritFrom)
	for i, tok := range tokens {
		if keptNames[tok] {
			tokens[i] = team + "-" + tok
		}
	}
	return strings.Join(tokens, " ")
}
