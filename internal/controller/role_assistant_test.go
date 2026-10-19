package controller

import (
	"context"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestBuiltinRoles_AssistantUse proves the assistant use verb sits on the
// operator, developer, and viewer roles, and that admin holds it through its
// wildcard rule.
func TestBuiltinRoles_AssistantUse(t *testing.T) {
	roles := make(map[string]*v1alpha1.VarroaRole)
	for _, role := range builtinRoles() {
		roles[role.Name] = role
	}
	for _, name := range []string{"operator", "developer", "viewer"} {
		role, ok := roles[name]
		if !ok {
			t.Fatalf("builtinRoles() missing role %s", name)
		}
		rule := findAPIRule(role.Spec.APIRules, "assistant")
		if rule == nil {
			t.Fatalf("role %s: expected an assistant rule, found none", name)
		}
		if !stringSlicesEqual(rule.Verbs, []string{"use"}) {
			t.Errorf("role %s: assistant verbs = %v, want [use]", name, rule.Verbs)
		}
	}
}

// TestRoleReconciler_RepairsAssistantRule proves the reconciler restores a
// built-in role whose assistant rule was removed.
func TestRoleReconciler_RepairsAssistantRule(t *testing.T) {
	ctx := context.Background()
	st := crdstore.NewFake()

	want := buildRoleForTest(t, "viewer")
	drifted := want.DeepCopy()
	kept := make([]v1alpha1.APIRule, 0, len(drifted.Spec.APIRules))
	for _, r := range drifted.Spec.APIRules {
		if len(r.Resources) == 1 && r.Resources[0] == "assistant" {
			continue
		}
		kept = append(kept, r)
	}
	drifted.Spec.APIRules = kept
	crdstore.MustSeed(st, drifted)

	r := NewRoleReconciler(nil, st)
	r.Logger = testLogger()
	if err := r.reconcileVarroaRoles(ctx); err != nil {
		t.Fatalf("reconcileVarroaRoles: %v", err)
	}

	got, err := crdstore.Get[v1alpha1.VarroaRole](ctx, st, "viewer", "")
	if err != nil {
		t.Fatalf("get viewer role: %v", err)
	}
	rule := findAPIRule(got.Spec.APIRules, "assistant")
	if rule == nil {
		t.Fatal("expected the reconciler to restore the assistant rule, found none")
	}
	if !stringSlicesEqual(rule.Verbs, []string{"use"}) {
		t.Errorf("restored assistant verbs = %v, want [use]", rule.Verbs)
	}
}
