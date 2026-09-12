package rbac

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// --------------------------------------------------------------------------
// D3's three guards: scopeMatches, controllerScopeMatches, and the
// EffectiveAPICapabilities / CreateScopeNamespaces empty-target skip.
// --------------------------------------------------------------------------

// TestScopeMatches_TenantScopeDeniesEveryController proves the early return
// this change adds to scopeMatches: a tenant-only scope answers false for
// every namespace and controller pair, including the cluster-wide
// (empty-argument) query that a tenant scope would otherwise fall through
// to matching.
func TestScopeMatches_TenantScopeDeniesEveryController(t *testing.T) {
	r := testResolver(nil, nil, nil, false)
	scope := &v1alpha1.VarroaRoleBindingScope{
		Tenants: []v1alpha1.TenantScope{{Name: "payments"}},
	}

	cases := []struct {
		namespace, controller string
	}{
		{"team-a", "ci"},
		{"team-b", "other"},
		{"", ""},
	}
	for _, c := range cases {
		matches, err := r.scopeMatches(scope, c.namespace, c.controller)
		if err != nil {
			t.Fatalf("scopeMatches(%q, %q): unexpected error: %v", c.namespace, c.controller, err)
		}
		if matches {
			t.Errorf("scopeMatches(%q, %q) = true, want false", c.namespace, c.controller)
		}
	}
}

// TestEffectiveAPICapabilities_TenantScopeGrantsNoControllerVerb proves the
// rbac-level mechanism behind "CanReadController denies every controller for
// a caller whose only binding is tenant-scoped": EffectiveAPICapabilities,
// called with a concrete namespace and controller name exactly as the
// per-controller gates call it, grants nothing from a tenant-scoped binding.
func TestEffectiveAPICapabilities_TenantScopeGrantsNoControllerVerb(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "developer"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "payments-only"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "pay-eng"}},
				RoleRef:  "developer",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Tenants: []v1alpha1.TenantScope{{Name: "payments"}},
				},
			},
		},
	}
	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Groups: []string{"pay-eng"}}

	caps := r.EffectiveAPICapabilities(claims, "team-pay", "ci")
	if hasVerb(caps, "controllers", "read") {
		t.Errorf("expected no controllers:read from a tenant-scoped binding, got %v", caps)
	}
}

// TestControllerScopeMatches_TenantScopeContributesNoRoleAssignment proves
// the JenkinsRoleBinding path: a controllerScope carrying tenants matches no
// controller, so it contributes no role assignment.
func TestControllerScopeMatches_TenantScopeContributesNoRoleAssignment(t *testing.T) {
	scope := &v1alpha1.VarroaRoleBindingScope{
		Tenants: []v1alpha1.TenantScope{{Name: "payments"}},
	}
	ctrl := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-pay"}}

	if controllerScopeMatches(scope, ctrl) {
		t.Errorf("controllerScopeMatches must return false for a scope carrying tenants")
	}
}

// TestJenkinsRoleAssignments_TenantScopeContributesNoAssignment proves the
// controllerScopeMatches guard end to end through JenkinsRoleAssignments: a
// JenkinsRoleBinding whose controllerScope carries tenants contributes no
// permission for any controller.
func TestJenkinsRoleAssignments_TenantScopeContributesNoAssignment(t *testing.T) {
	jenkinsRoles := []*v1alpha1.JenkinsRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "builder"},
			Spec:       v1alpha1.JenkinsRoleSpec{Permissions: []string{"hudson.model.Item.Build"}},
		},
	}
	jenkinsRoleBindings := []*v1alpha1.JenkinsRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "payments-builders"},
			Spec: v1alpha1.JenkinsRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "pay-eng"}},
				RoleRef:  "builder",
				ControllerScope: &v1alpha1.VarroaRoleBindingScope{
					Tenants: []v1alpha1.TenantScope{{Name: "payments"}},
				},
			},
		},
	}
	r := testResolverWithJenkins(nil, nil, jenkinsRoles, jenkinsRoleBindings, nil, false)
	ctrl := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-pay"}}

	assignments, err := r.JenkinsRoleAssignments(ctrl)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, a := range assignments {
		if a.RoleName == "varroa:builder" {
			t.Errorf("expected the tenant-scoped JenkinsRoleBinding to contribute no assignment, got %+v", a)
		}
	}
}

// TestEffectiveAPICapabilities_TenantScopeSkippedBeforeEmptyTargetShortcut
// proves task 4.7a: a caller whose only binding is tenant-scoped is denied
// catalogitems:read and composedbundles:update even though the catalog and
// bundle gates call EffectiveAPICapabilities with two empty strings, which
// would otherwise bypass scopeMatches entirely.
func TestEffectiveAPICapabilities_TenantScopeSkippedBeforeEmptyTargetShortcut(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "catalog-admin"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"catalogitems"}, Verbs: []string{"read"}},
					{Resources: []string{"composedbundles"}, Verbs: []string{"update"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "payments-only"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "pay-eng"}},
				RoleRef:  "catalog-admin",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Tenants: []v1alpha1.TenantScope{{Name: "payments"}},
				},
			},
		},
	}
	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Groups: []string{"pay-eng"}}

	caps := r.EffectiveAPICapabilities(claims, "", "")
	if hasVerb(caps, "catalogitems", "read") {
		t.Errorf("expected no catalogitems:read, got %v", caps)
	}
	if hasVerb(caps, "composedbundles", "update") {
		t.Errorf("expected no composedbundles:update, got %v", caps)
	}
}

// TestCreateScopeNamespaces_TenantScopeContributesNothing proves the same
// skip applies to CreateScopeNamespaces, which walks the caller's bindings
// directly rather than through scopeMatches.
func TestCreateScopeNamespaces_TenantScopeContributesNothing(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "creator"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"create"}}},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "payments-only"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "pay-eng"}},
				RoleRef:  "creator",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Tenants: []v1alpha1.TenantScope{{Name: "payments"}},
				},
			},
		},
	}
	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Groups: []string{"pay-eng"}}

	namespaces, unrestricted := r.CreateScopeNamespaces(claims)
	if unrestricted {
		t.Errorf("expected unrestricted=false, a tenant-scoped binding grants no create authority")
	}
	if len(namespaces) != 0 {
		t.Errorf("expected no namespaces, got %v", namespaces)
	}
}
