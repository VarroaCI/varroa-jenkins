package controller

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestBuiltinRoles_TenantAgentVerbs proves the operator role carries the
// cluster-wide Tenant agent verbs, and that developer and viewer carry neither.
func TestBuiltinRoles_TenantAgentVerbs(t *testing.T) {
	roles := make(map[string]*v1alpha1.VarroaRole)
	for _, role := range builtinRoles() {
		roles[role.Name] = role
	}
	op := roles["operator"]
	if op == nil {
		t.Fatal("builtinRoles() missing the operator role")
	}
	if rule := findAPIRule(op.Spec.APIRules, "tenantagents"); rule == nil || !stringSlicesEqual(rule.Verbs, []string{"unattended"}) {
		t.Errorf("operator tenantagents rule = %+v, want [unattended]", rule)
	}
	if rule := findAPIRule(op.Spec.APIRules, "agentsessions"); rule == nil || !stringSlicesEqual(rule.Verbs, []string{"read"}) {
		t.Errorf("operator agentsessions rule = %+v, want [read]", rule)
	}
	for _, name := range []string{"developer", "viewer"} {
		role := roles[name]
		if role == nil {
			t.Fatalf("builtinRoles() missing role %s", name)
		}
		if findAPIRule(role.Spec.APIRules, "tenantagents") != nil || findAPIRule(role.Spec.APIRules, "agentsessions") != nil {
			t.Errorf("role %s must not carry a Tenant agent verb", name)
		}
	}
}
