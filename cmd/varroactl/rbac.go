package main

import (
	"fmt"
	"strings"
)

func init() {
	registerNoun(addRBACNouns)
}

// addRBACNouns registers the four RBAC noun families via addCRDNoun.
// All are cluster-scoped: -n is rejected, -o name prints bare names.
func addRBACNouns(v *verbCommands) {
	// Roles
	addCRDNoun(v, crdNounOpts{
		Noun:         "role",
		Aliases:      []string{"roles"},
		Path:         "/roles",
		Namespaced:   false,
		DescribeFrom: true,
		Headers:      []string{"NAME", "JENKINS-ROLE", "API-RULES", "BUILTIN"},
		Columns:      roleColumns,
	})

	// Role bindings
	addCRDNoun(v, crdNounOpts{
		Noun:         "rolebinding",
		Aliases:      []string{"rolebindings", "rb"},
		Path:         "/rolebindings",
		Namespaced:   false,
		DescribeFrom: true,
		Headers:      []string{"NAME", "ROLE", "SUBJECTS", "SCOPE"},
		Columns:      roleBindingColumns,
	})

	// Jenkins roles — cluster-scoped
	addCRDNoun(v, crdNounOpts{
		Noun:          "jenkinsrole",
		Aliases:       []string{"jenkinsroles", "jr"},
		Path:          "/jenkinsroles",
		Namespaced:    false,
		ClusterScoped: true,
		DescribeFrom:  true,
		Headers:       []string{"NAME", "TYPE", "PERMISSIONS"},
		Columns:       jenkinsRoleColumns,
	})

	// Jenkins role bindings — cluster-scoped
	addCRDNoun(v, crdNounOpts{
		Noun:          "jenkinsrolebinding",
		Aliases:       []string{"jenkinsrolebindings", "jrb"},
		Path:          "/jenkinsrolebindings",
		Namespaced:    false,
		ClusterScoped: true,
		DescribeFrom:  true,
		Headers:       []string{"NAME", "ROLE", "SUBJECTS", "JENKINS-SCOPE"},
		Columns:       jenkinsRoleBindingColumns,
	})
}

// ---------------------------------------------------------------------------
// Column helpers
// ---------------------------------------------------------------------------

func roleColumns(item map[string]any) []string {
	// JENKINS-ROLE: spec.jenkinsRoleRef or "-"
	jenkinsRole := "-"
	if spec, ok := item["spec"].(map[string]any); ok {
		if jr, ok := spec["jenkinsRoleRef"].(string); ok && jr != "" {
			jenkinsRole = jr
		}
	}

	// API-RULES: count of spec.apiRules
	apiRules := "0"
	if spec, ok := item["spec"].(map[string]any); ok {
		if rules, ok := spec["apiRules"].([]any); ok {
			apiRules = fmt.Sprintf("%d", len(rules))
		}
	}

	// BUILTIN: true when label varroa.dev/builtin=true
	builtin := ""
	if meta, ok := item["metadata"].(map[string]any); ok {
		if labels, ok := meta["labels"].(map[string]any); ok {
			if v, ok := labels["varroa.dev/builtin"]; ok && fmt.Sprintf("%v", v) == "true" {
				builtin = "true"
			}
		}
	}

	return []string{itemName(item), jenkinsRole, apiRules, builtin}
}

func roleBindingColumns(item map[string]any) []string {
	// ROLE: spec.roleRef
	roleRef := ""
	if spec, ok := item["spec"].(map[string]any); ok {
		if rr, ok := spec["roleRef"].(string); ok {
			roleRef = rr
		}
	}

	// SUBJECTS: kind:name comma-joined, >3 → first3 +N more
	subjects := ""
	if spec, ok := item["spec"].(map[string]any); ok {
		if subs, ok := spec["subjects"].([]any); ok {
			var parts []string
			for _, s := range subs {
				if sm, ok := s.(map[string]any); ok {
					kind, _ := sm["kind"].(string)
					sname, _ := sm["name"].(string)
					if kind == "" {
						kind = "unknown"
					}
					parts = append(parts, kind+":"+sname)
				}
			}
			if len(parts) > 3 {
				subjects = strings.Join(parts[:3], ",") + fmt.Sprintf(" +%d more", len(parts)-3)
			} else {
				subjects = strings.Join(parts, ",")
			}
		}
	}

	// SCOPE: namespaces=a,b / selector / "-"
	scope := "-"
	if spec, ok := item["spec"].(map[string]any); ok {
		if ns, ok := spec["namespaces"].([]any); ok && len(ns) > 0 {
			nsStrs := make([]string, 0, len(ns))
			for _, n := range ns {
				nsStrs = append(nsStrs, fmt.Sprintf("%v", n))
			}
			scope = "namespaces=" + strings.Join(nsStrs, ",")
		} else if sel, ok := spec["namespaceSelector"].(map[string]any); ok && len(sel) > 0 {
			scope = "selector"
		}
	}

	return []string{itemName(item), roleRef, subjects, scope}
}

func jenkinsRoleColumns(item map[string]any) []string {
	// TYPE: spec.roleType default "Global"
	roleType := "Global"
	if spec, ok := item["spec"].(map[string]any); ok {
		if rt, ok := spec["roleType"].(string); ok && rt != "" {
			roleType = rt
		}
	}

	// PERMISSIONS: count of spec.permissions
	perms := "0"
	if spec, ok := item["spec"].(map[string]any); ok {
		if p, ok := spec["permissions"].([]any); ok {
			perms = fmt.Sprintf("%d", len(p))
		}
	}

	return []string{itemName(item), roleType, perms}
}

func jenkinsRoleBindingColumns(item map[string]any) []string {
	// ROLE: spec.roleRef
	roleRef := ""
	if spec, ok := item["spec"].(map[string]any); ok {
		if rr, ok := spec["roleRef"].(string); ok {
			roleRef = rr
		}
	}

	// SUBJECTS: kind:name comma-joined, >3 → first3 +N more
	subjects := ""
	if spec, ok := item["spec"].(map[string]any); ok {
		if subs, ok := spec["subjects"].([]any); ok {
			var parts []string
			for _, s := range subs {
				if sm, ok := s.(map[string]any); ok {
					kind, _ := sm["kind"].(string)
					sname, _ := sm["name"].(string)
					if kind == "" {
						kind = "unknown"
					}
					parts = append(parts, kind+":"+sname)
				}
			}
			if len(parts) > 3 {
				subjects = strings.Join(parts[:3], ",") + fmt.Sprintf(" +%d more", len(parts)-3)
			} else {
				subjects = strings.Join(parts, ",")
			}
		}
	}

	// JENKINS-SCOPE: spec.jenkinsScope.type default "Global", "Folder" shows "folder"
	jenkinsScope := "Global"
	if spec, ok := item["spec"].(map[string]any); ok {
		if js, ok := spec["jenkinsScope"].(map[string]any); ok {
			if jt, ok := js["type"].(string); ok && jt != "" {
				jenkinsScope = jt
			}
		}
	}

	return []string{itemName(item), roleRef, subjects, jenkinsScope}
}
