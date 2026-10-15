package activity

import "github.com/varroaci/varroa-jenkins/internal/rbac"

// Scope is a superset of what the caller may read from persisted activity
// history. The per-event predicate (Authorizer.CanReadActivityEvent) stays
// authoritative, so a Scope wider than the caller's real grant cannot
// disclose a row: it only narrows what a SQL reader has to scan.
type Scope struct {
	All        bool     // no row restriction; every controller and every global event
	Global     bool     // rows with an empty controller are admitted
	Namespaces []string // rows in these namespaces are admitted
}

// Admits reports whether e falls inside scope, matching the SQL clause a
// reader pushes down for a non-All scope:
// (namespace = ANY(Namespaces) OR (Global AND controller = ”)).
func (s Scope) Admits(e Event) bool {
	if s.All {
		return true
	}
	if s.Global && e.Controller == "" {
		return true
	}
	for _, ns := range s.Namespaces {
		if ns == e.Namespace {
			return true
		}
	}
	return false
}

// hasControllersRead reports whether caps grants the "read" verb on the
// "controllers" resource, honoring the "*" resource/verb wildcards the
// same way internal/api's hasVerb does.
func hasControllersRead(caps rbac.APICapabilitySet) bool {
	if caps["*"] != nil && (caps["*"]["*"] || caps["*"]["read"]) {
		return true
	}
	if caps["controllers"] != nil && (caps["controllers"]["*"] || caps["controllers"]["read"]) {
		return true
	}
	return false
}

// ScopeFromPermissions builds the activity read Scope from D5's algorithm.
// perms is the caller's scope-partitioned capabilities
// (Authorizer.EffectivePermissions); canReadGlobal must equal
// Authorizer.CanReadGlobalActivity for the same caller — a cluster-wide
// controllers:read grant, which also admits every controller.
//
// The binding model has exactly three shapes (VarroaRoleBindingScope): a
// nil scope (cluster-wide), a Namespaces list, and a ControllerSelector.
// There is no binding that names one controller directly, so a
// selector-scoped grant falls back to the whole table (case 3) — the
// per-event predicate does the whole job for that caller.
func ScopeFromPermissions(perms rbac.PermissionScopes, canReadGlobal bool) Scope {
	if canReadGlobal {
		return Scope{All: true, Global: true}
	}
	var s Scope
	for _, sc := range perms.Scopes {
		if !hasControllersRead(sc.Capabilities) {
			continue
		}
		if sc.HasControllerSelector {
			s.All = true
			continue
		}
		s.Namespaces = append(s.Namespaces, sc.Namespaces...)
	}
	return s
}
