package controller

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestBuiltinRoles_TeamAgentVerbs proves the operator role carries the
// cluster-wide Team agent verbs, and that developer and viewer carry neither.
func TestBuiltinRoles_TeamAgentVerbs(t *testing.T) {
	roles := make(map[string]*v1alpha1.VarroaRole)
	for _, role := range builtinRoles() {
		roles[role.Name] = role
	}
	op := roles["operator"]
	if op == nil {
		t.Fatal("builtinRoles() missing the operator role")
	}
	if rule := findAPIRule(op.Spec.APIRules, "teamagents"); rule == nil || !stringSlicesEqual(rule.Verbs, []string{"unattended"}) {
		t.Errorf("operator teamagents rule = %+v, want [unattended]", rule)
	}
	if rule := findAPIRule(op.Spec.APIRules, "agentsessions"); rule == nil || !stringSlicesEqual(rule.Verbs, []string{"read"}) {
		t.Errorf("operator agentsessions rule = %+v, want [read]", rule)
	}
	for _, name := range []string{"developer", "viewer"} {
		role := roles[name]
		if role == nil {
			t.Fatalf("builtinRoles() missing role %s", name)
		}
		if findAPIRule(role.Spec.APIRules, "teamagents") != nil || findAPIRule(role.Spec.APIRules, "agentsessions") != nil {
			t.Errorf("role %s must not carry a Team agent verb", name)
		}
	}
}
