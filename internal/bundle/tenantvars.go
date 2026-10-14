package bundle

import (
	"fmt"
	"sort"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
)

// ParseVariables reads the "name: value" lines of a content ConfigMap's
// variables.yaml. It skips blank lines and "#" comments. The controller's
// own bundle and a cell tenant's pod-template lift both use it, so both
// read one format the same way.
func ParseVariables(varYAML string) Variables {
	vars := make(Variables)
	for _, line := range strings.Split(varYAML, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			vars[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return vars
}

// VarsReplacer builds one replacer that swaps each "${name}" of vars
// for its value. A strings.Replacer makes one pass and never scans a
// replacement again. ResolveVars loops over a map, so a value that
// names another variable would resolve on some ticks and not on others,
// and the lift would then flip between keeping and skipping the template.
// With one pass, such a value always keeps its "${" and is always skipped.
func VarsReplacer(vars Variables) *strings.Replacer {
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([]string, 0, 2*len(names))
	for _, name := range names {
		pairs = append(pairs, "${"+name+"}", vars[name])
	}
	return strings.NewReplacer(pairs...)
}

// RemainingVarRefs scans content for remaining ${var} placeholders.
// JCasC literal escapes (^${var}) are skipped: Jenkins strips the caret and
// the value is not Varroa's to resolve. JCasC secret-source references
// (${readFile:...}, ${base64:...}) are also skipped: they must reach Jenkins
// byte-for-byte so configuration-as-code's own SecretSourceResolver can
// resolve them at Jenkins startup.
func RemainingVarRefs(allowJcascSecretSources bool, contents ...string) []string {
	var found []string
	seen := make(map[string]bool)
	for _, c := range contents {
		for {
			start := strings.Index(c, "${")
			if start < 0 {
				break
			}
			rel := strings.Index(c[start:], "}")
			if rel < 0 {
				break
			}
			end := start + rel
			escaped := start > 0 && c[start-1] == '^'
			varName := c[start+2 : end]
			if !escaped && (!allowJcascSecretSources || !IsJCascSecretSourceRef(varName)) && !seen[varName] {
				found = append(found, varName)
				seen[varName] = true
			}
			c = c[end+1:]
		}
	}
	return found
}

// ParseTenantItems returns the items.yaml of a tenant bundle as the cell
// reads it. It resolves the tenant's own variables.yaml in one pass, then
// fails on any "${" reference left over, then parses with no unknown key.
// data is the content ConfigMap; a nil or empty items.yaml gives an empty
// manifest.
func ParseTenantItems(data map[string]string) (*items.Manifest, error) {
	resolved := VarsReplacer(ParseVariables(data["variables.yaml"])).Replace(data["items.yaml"])
	if unresolved := RemainingVarRefs(false, resolved); len(unresolved) > 0 {
		return nil, fmt.Errorf("items.yaml holds unresolved variables: %s", strings.Join(unresolved, ", "))
	}
	manifest, err := items.ParseStrict(resolved)
	if err != nil {
		return nil, fmt.Errorf("parse tenant items.yaml: %w", err)
	}
	return manifest, nil
}
