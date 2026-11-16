package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
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
		Headers:      []string{"NAME", "ROLE", "SUBJECTS", "SCOPE", "OWNER"},
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
		Headers:       []string{"NAME", "ROLE", "SUBJECTS", "JENKINS-SCOPE", "CONTROLLER-SCOPE", "CONTROLLER"},
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

	return []string{itemName(item), roleRef, subjects, roleBindingScope(item), roleBindingOwner(item)}
}

// roleBindingScope returns the SCOPE cell of a VarroaRoleBinding: the
// spec.scope text, "Cluster-wide" when spec.scope is absent or names no part,
// or "-" when spec.scope cannot be read. The words match formatScope in
// frontend/src/pages/RoleBindings.tsx. Change both together.
func roleBindingScope(item map[string]any) string {
	spec, _ := item["spec"].(map[string]any)
	raw := spec["scope"]
	if raw == nil {
		return "Cluster-wide"
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return "-"
	}
	var sc v1alpha1.VarroaRoleBindingScope
	if err := json.Unmarshal(b, &sc); err != nil {
		return "-"
	}
	return formatBindingScope(&sc)
}

// formatBindingScope joins the namespace, selector, and tenant parts of sc
// with one space, or returns "Cluster-wide" when sc names no part.
// Label keys are sorted, because the dashboard shows them in JSON key order.
func formatBindingScope(sc *v1alpha1.VarroaRoleBindingScope) string {
	var parts []string
	if len(sc.Namespaces) > 0 {
		parts = append(parts, "ns:"+strings.Join(sc.Namespaces, ","))
	}
	if sel := sc.ControllerSelector; sel != nil {
		var labels []string
		for _, k := range slices.Sorted(maps.Keys(sel.MatchLabels)) {
			if v := sel.MatchLabels[k]; v != "" {
				labels = append(labels, k+"="+v)
			}
		}
		if len(labels) > 0 {
			parts = append(parts, "sel:"+strings.Join(labels, ","))
		} else if len(sel.MatchExpressions) > 0 {
			parts = append(parts, "Selector")
		}
	}
	for _, t := range sc.Tenants {
		p := "tenant:" + t.Name
		if len(t.Jobs) > 0 {
			p += " jobs:" + strings.Join(t.Jobs, ",")
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return "Cluster-wide"
	}
	return strings.Join(parts, " ")
}

// roleBindingOwner returns "<kind>/<namespace>/<name>" of the object whose
// reconciler writes the binding, or "-" for every other binding.
// v1alpha1.VarroaRoleBindingOwner is the one rule. Do not test the name here.
func roleBindingOwner(item map[string]any) string {
	b := &v1alpha1.VarroaRoleBinding{}
	b.Name = itemName(item)
	if meta, ok := item["metadata"].(map[string]any); ok {
		b.Labels = stringMap(meta["labels"])
		b.Annotations = stringMap(meta["annotations"])
	}
	o, ok := v1alpha1.VarroaRoleBindingOwner(b)
	if !ok {
		return "-"
	}
	return o.Kind + "/" + o.Namespace + "/" + o.Name
}

// stringMap returns the string values of v, a JSON object, or nil when v is
// not an object.
func stringMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		if s, ok := val.(string); ok {
			out[k] = s
		}
	}
	return out
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

	return []string{itemName(item), roleRef, subjects, jenkinsRoleBindingScope(item), jenkinsRoleBindingControllerScope(item), jenkinsRoleBindingController(item)}
}

// jenkinsRoleBindingController returns "<namespace>/<name>" of the controller
// whose spec.rbacSpec writes the binding, or "-" for every other binding.
// v1alpha1.RBACSpecBindingOwner is the one rule. Do not test the name here.
func jenkinsRoleBindingController(item map[string]any) string {
	b := &v1alpha1.JenkinsRoleBinding{}
	b.Name = itemName(item)
	if meta, ok := item["metadata"].(map[string]any); ok {
		b.Labels = stringMap(meta["labels"])
	}
	ns, name, ok := v1alpha1.RBACSpecBindingOwner(b)
	if !ok {
		return "-"
	}
	return ns + "/" + name
}

// jenkinsRoleBindingScope returns the JENKINS-SCOPE cell of a JenkinsRoleBinding.
// An absent spec.jenkinsScope is Global. A Folder or Pattern scope names its
// folder or its pattern. A Folder scope adds its propagate value when that
// value covers more than the folder. The cell is "-" when spec.jenkinsScope
// cannot be read or names no type. The words match formatJenkinsScope in
// frontend/src/pages/JenkinsRoleBindings.tsx. Change both together.
func jenkinsRoleBindingScope(item map[string]any) string {
	spec, _ := item["spec"].(map[string]any)
	raw := spec["jenkinsScope"]
	if raw == nil {
		return v1alpha1.JenkinsScopeGlobal
	}
	var js v1alpha1.JenkinsScope
	if !decodeSpecValue(raw, &js) || js.Type == "" {
		return "-"
	}
	switch {
	case js.Type == v1alpha1.JenkinsScopeFolder && js.Folder != "":
		text := "Folder: " + js.Folder
		if js.Propagate == "Children" || js.Propagate == "Subtree" {
			text += " (" + js.Propagate + ")"
		}
		return text
	case js.Type == v1alpha1.JenkinsScopePattern && js.Pattern != "":
		return "Pattern: " + js.Pattern
	}
	return js.Type
}

// jenkinsRoleBindingControllerScope returns the CONTROLLER-SCOPE cell of a
// JenkinsRoleBinding. An absent spec.controllerScope is "Cluster-wide". The
// cell is "-" when spec.controllerScope cannot be read. The words match
// formatControllerScope in frontend/src/pages/JenkinsRoleBindings.tsx.
// Change both together.
func jenkinsRoleBindingControllerScope(item map[string]any) string {
	spec, _ := item["spec"].(map[string]any)
	raw := spec["controllerScope"]
	if raw == nil {
		return "Cluster-wide"
	}
	var sc v1alpha1.VarroaRoleBindingScope
	if !decodeSpecValue(raw, &sc) {
		return "-"
	}
	return formatControllerScope(&sc)
}

// formatControllerScope joins the namespace and selector parts of sc with one
// space. It returns "Cluster-wide" when sc names no namespace and no selector
// term, because such a scope matches every controller. A label with an empty
// value prints as "<key>=", because the selector still requires it. Label keys
// are sorted. A scope with tenants matches no controller, so it returns "-".
func formatControllerScope(sc *v1alpha1.VarroaRoleBindingScope) string {
	if len(sc.Tenants) > 0 {
		return "-"
	}
	var parts []string
	if len(sc.Namespaces) > 0 {
		parts = append(parts, "ns:"+strings.Join(sc.Namespaces, ","))
	}
	if sel := sc.ControllerSelector; sel != nil {
		if len(sel.MatchLabels) > 0 {
			labels := make([]string, 0, len(sel.MatchLabels))
			for _, k := range slices.Sorted(maps.Keys(sel.MatchLabels)) {
				labels = append(labels, k+"="+sel.MatchLabels[k])
			}
			parts = append(parts, "sel:"+strings.Join(labels, ","))
		} else if len(sel.MatchExpressions) > 0 {
			parts = append(parts, "Selector")
		}
	}
	if len(parts) == 0 {
		return "Cluster-wide"
	}
	return strings.Join(parts, " ")
}

// decodeSpecValue decodes raw, one value of a JSON document, into out. It
// returns false when raw does not fit the type of out.
func decodeSpecValue(raw, out any) bool {
	b, err := json.Marshal(raw)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, out) == nil
}
