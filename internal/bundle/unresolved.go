package bundle

import (
	"sort"
	"strings"
)

// FindUnresolvedVariables returns the distinct ${var} names present in content
// that are neither keys of userVars nor members of InjectedVariableNames, sorted.
// allowJcascSecretSources additionally skips JCasC secret-source refs
// (${readFile:...} etc.) — pass true only for content applied through
// configuration-as-code (jenkins.yaml, rbac.yaml), where Jenkins itself
// resolves them.
func FindUnresolvedVariables(content string, userVars map[string]string, allowJcascSecretSources bool) []string {
	if content == "" {
		return []string{}
	}

	injected := make(map[string]bool, len(InjectedVariableNames))
	for _, v := range InjectedVariableNames {
		injected[v] = true
	}

	found := make(map[string]bool)
	remaining := content
	for {
		start := strings.Index(remaining, "${")
		if start < 0 {
			break
		}
		rel := strings.Index(remaining[start:], "}")
		if rel < 0 {
			break
		}
		end := start + rel
		// Skip JCasC literal escapes (^${var}).
		escaped := start > 0 && remaining[start-1] == '^'
		varName := remaining[start+2 : end]
		if !escaped && (!allowJcascSecretSources || !IsJCascSecretSourceRef(varName)) {
			if _, ok := userVars[varName]; !ok && !injected[varName] && !found[varName] {
				found[varName] = true
			}
		}
		remaining = remaining[end+1:]
	}

	result := make([]string, 0, len(found))
	for v := range found {
		result = append(result, v)
	}
	sort.Strings(result)
	return result
}
