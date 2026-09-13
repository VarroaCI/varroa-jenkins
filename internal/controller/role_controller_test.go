package controller

import (
	"context"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func TestBuiltinJenkinsRoles_IncludesSystemOperator(t *testing.T) {
	roles := builtinJenkinsRoles()

	var found *v1alpha1.JenkinsRole
	for _, r := range roles {
		if r.Name == "varroa-system-operator" {
			found = r
			break
		}
	}
	if found == nil {
		t.Fatal("expected builtinJenkinsRoles() to include varroa-system-operator")
	}
	if found.Spec.RoleType != "Global" {
		t.Errorf("expected RoleType=Global, got %s", found.Spec.RoleType)
	}
	if len(found.Spec.Permissions) != 1 || found.Spec.Permissions[0] != "hudson.model.Hudson.Administer" {
		t.Errorf("expected exactly [hudson.model.Hudson.Administer], got %v", found.Spec.Permissions)
	}
	if found.Labels[v1alpha1.LabelBuiltin] != "true" {
		t.Errorf("expected LabelBuiltin=true, got %v", found.Labels)
	}
}

// findAPIRule returns the APIRule for the given resource, or nil if the role
// carries no rule for that resource.
func findAPIRule(rules []v1alpha1.APIRule, resource string) *v1alpha1.APIRule {
	for i := range rules {
		for _, r := range rules[i].Resources {
			if r == resource {
				return &rules[i]
			}
		}
	}
	return nil
}

func TestBuiltinRoles_PipelinesVerbMatrix(t *testing.T) {
	roles := make(map[string]*v1alpha1.VarroaRole)
	for _, role := range builtinRoles() {
		roles[role.Name] = role
	}

	cases := []struct {
		role  string
		verbs []string
	}{
		{"operator", []string{"read", "write", "admin"}},
		{"developer", []string{"read", "write"}},
		{"viewer", []string{"read"}},
	}
	for _, tc := range cases {
		role, ok := roles[tc.role]
		if !ok {
			t.Fatalf("builtinRoles() missing role %s", tc.role)
		}
		rule := findAPIRule(role.Spec.APIRules, "pipelines")
		if rule == nil {
			t.Fatalf("role %s: expected a pipelines rule, found none", tc.role)
		}
		if !stringSlicesEqual(rule.Verbs, tc.verbs) {
			t.Errorf("role %s: pipelines verbs = %v, want %v", tc.role, rule.Verbs, tc.verbs)
		}
	}

	admin, ok := roles["admin"]
	if !ok {
		t.Fatal("builtinRoles() missing role admin")
	}
	if rule := findAPIRule(admin.Spec.APIRules, "pipelines"); rule != nil {
		t.Errorf("role admin: expected no pipelines rule (wildcard already covers it), got %v", rule)
	}
}

// TestBuiltinRoles_RunnersVerbMatrix proves the built-in roles carry the
// runners verb set the runner-registration delta requires: admin and
// operator hold all four verbs (admin via the wildcard rule), developer
// and viewer hold read only.
func TestBuiltinRoles_RunnersVerbMatrix(t *testing.T) {
	roles := make(map[string]*v1alpha1.VarroaRole)
	for _, role := range builtinRoles() {
		roles[role.Name] = role
	}

	cases := []struct {
		role  string
		verbs []string
	}{
		{"operator", []string{"read", "create", "update", "delete"}},
		{"developer", []string{"read"}},
		{"viewer", []string{"read"}},
	}
	for _, tc := range cases {
		role, ok := roles[tc.role]
		if !ok {
			t.Fatalf("builtinRoles() missing role %s", tc.role)
		}
		rule := findAPIRule(role.Spec.APIRules, "runners")
		if rule == nil {
			t.Fatalf("role %s: expected a runners rule, found none", tc.role)
		}
		if !stringSlicesEqual(rule.Verbs, tc.verbs) {
			t.Errorf("role %s: runners verbs = %v, want %v", tc.role, rule.Verbs, tc.verbs)
		}
	}

	admin, ok := roles["admin"]
	if !ok {
		t.Fatal("builtinRoles() missing role admin")
	}
	if rule := findAPIRule(admin.Spec.APIRules, "runners"); rule != nil {
		t.Errorf("role admin: expected no runners rule (wildcard already covers it), got %v", rule)
	}
}

// TestRoleReconciler_RewritesDriftedPipelinesRule proves the reconciler
// repairs a built-in role whose pipelines rule was removed, matching D10's
// statement that the existing apiRulesEqual drift check needs no new
// migration path.
func TestRoleReconciler_RewritesDriftedPipelinesRule(t *testing.T) {
	ctx := context.Background()
	st := crdstore.NewFake()

	want := buildRoleForTest(t, "developer")
	drifted := want.DeepCopy()
	drifted.Spec.APIRules = drifted.Spec.APIRules[:len(drifted.Spec.APIRules)-1] // drop the pipelines rule
	crdstore.MustSeed(st, drifted)

	r := NewRoleReconciler(nil, st)
	r.Logger = testLogger()
	if err := r.reconcileVarroaRoles(ctx); err != nil {
		t.Fatalf("reconcileVarroaRoles: %v", err)
	}

	got, err := crdstore.Get[v1alpha1.VarroaRole](ctx, st, "developer", "")
	if err != nil {
		t.Fatalf("get developer role: %v", err)
	}
	rule := findAPIRule(got.Spec.APIRules, "pipelines")
	if rule == nil {
		t.Fatal("expected the reconciler to restore the pipelines rule, found none")
	}
	if !stringSlicesEqual(rule.Verbs, []string{"read", "write"}) {
		t.Errorf("restored pipelines verbs = %v, want [read write]", rule.Verbs)
	}
}

// TestBuiltinRoles_CellPoolVerbMatrix proves the cell-pool verbs: operator
// holds read and update, admin holds both through its wildcard and carries
// no cellpools rule, and developer and viewer carry no rule at all.
// CellPool.status.tenants lists every tenant of the pool, and the Teams API
// is admin-only, so a read verb for a developer or a viewer would disclose
// the tenant list to a caller who may not list Teams.
func TestBuiltinRoles_CellPoolVerbMatrix(t *testing.T) {
	roles := make(map[string]*v1alpha1.VarroaRole)
	for _, role := range builtinRoles() {
		roles[role.Name] = role
	}

	operator, ok := roles["operator"]
	if !ok {
		t.Fatal("builtinRoles() missing role operator")
	}
	rule := findAPIRule(operator.Spec.APIRules, "cellpools")
	if rule == nil {
		t.Fatal("role operator: expected a cellpools rule, found none")
	}
	if !stringSlicesEqual(rule.Verbs, []string{"read", "update"}) {
		t.Errorf("role operator: cellpools verbs = %v, want [read update]", rule.Verbs)
	}

	for _, name := range []string{"admin", "developer", "viewer"} {
		role, ok := roles[name]
		if !ok {
			t.Fatalf("builtinRoles() missing role %s", name)
		}
		if rule := findAPIRule(role.Spec.APIRules, "cellpools"); rule != nil {
			t.Errorf("role %s: expected no cellpools rule, got %v", name, rule)
		}
	}
}

// TestRoleReconciler_RewritesDriftedCellPoolRule proves the reconciler
// repairs a built-in role whose cellpools rule was removed, so an upgraded
// install converges with no migration step.
func TestRoleReconciler_RewritesDriftedCellPoolRule(t *testing.T) {
	ctx := context.Background()
	st := crdstore.NewFake()

	want := buildRoleForTest(t, "operator")
	drifted := want.DeepCopy()
	kept := make([]v1alpha1.APIRule, 0, len(drifted.Spec.APIRules))
	for _, r := range drifted.Spec.APIRules {
		if len(r.Resources) == 1 && r.Resources[0] == "cellpools" {
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

	got, err := crdstore.Get[v1alpha1.VarroaRole](ctx, st, "operator", "")
	if err != nil {
		t.Fatalf("get operator role: %v", err)
	}
	rule := findAPIRule(got.Spec.APIRules, "cellpools")
	if rule == nil {
		t.Fatal("expected the reconciler to restore the cellpools rule, found none")
	}
	if !stringSlicesEqual(rule.Verbs, []string{"read", "update"}) {
		t.Errorf("restored cellpools verbs = %v, want [read update]", rule.Verbs)
	}
}

// buildRoleForTest returns the canonical built-in role definition by name,
// failing the test if no such role exists.
func buildRoleForTest(t *testing.T, name string) *v1alpha1.VarroaRole {
	t.Helper()
	for _, role := range builtinRoles() {
		if role.Name == name {
			return role
		}
	}
	t.Fatalf("builtinRoles() missing role %s", name)
	return nil
}
