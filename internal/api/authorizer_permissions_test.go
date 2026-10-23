package api

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// testResolverWithTenants mirrors testResolver, but also seeds a Tenant
// informer, for tests that exercise Authorizer.EffectivePermissions'
// Tenants partition.
func testResolverWithTenants(
	roles []*v1alpha1.VarroaRole,
	bindings []*v1alpha1.VarroaRoleBinding,
	tenants []*v1alpha1.Tenant,
) *rbac.Resolver {
	roleIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, r := range roles {
		_ = roleIdx.Add(r)
	}
	roleInf := &fakeInformer{indexer: roleIdx}

	bindingIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{rbac.BySubjectIndex: rbac.SubjectIndexFunc})
	for _, b := range bindings {
		_ = bindingIdx.Add(b)
	}
	bindingInf := &fakeInformer{indexer: bindingIdx}

	jrIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	jrInf := &fakeInformer{indexer: jrIdx}

	jrbIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{rbac.JenkinsBySubjectIndex: rbac.JenkinsSubjectIndexFunc})
	jrbInf := &fakeInformer{indexer: jrbIdx}

	ctrlIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	ctrlInf := &fakeInformer{indexer: ctrlIdx}

	tenantIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, tm := range tenants {
		_ = tenantIdx.Add(tm)
	}
	tenantInf := &fakeInformer{indexer: tenantIdx}

	return rbac.NewResolver(roleInf, bindingInf, jrInf, jrbInf, ctrlInf, tenantInf, false,
		[]string{"sub", "preferred_username"})
}

// TestEffectivePermissions_TenantsNeverNil proves /me/permissions carries a
// non-nil, empty Tenants array when the caller holds no tenant grant.
func TestEffectivePermissions_TenantsNeverNil(t *testing.T) {
	r := testResolverWithTenants(nil, nil, nil)
	a := NewAuthorizer(r, false)
	claims := &auth.Claims{Subject: "user1"}

	perms := a.EffectivePermissions(claims)
	if perms.Tenants == nil {
		t.Fatal("expected a non-nil empty Tenants array, got nil")
	}
	if len(perms.Tenants) != 0 {
		t.Errorf("expected no tenant entries, got %+v", perms.Tenants)
	}
}

// TestEffectivePermissions_SyntheticTenantAbsent proves a synthetic
// (single-controller) tenant the caller can reach through a namespace-scoped
// binding never appears in Tenants: that partition enumerates Tenant tenants
// only, and the namespace-scoped grant is still visible through Scopes.
func TestEffectivePermissions_SyntheticTenantAbsent(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "dev"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read", "write"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user1"}},
			RoleRef:  "dev",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
		}},
	}
	r := testResolverWithTenants(roles, bindings, nil)
	a := NewAuthorizer(r, false)
	claims := &auth.Claims{Subject: "user1"}

	perms := a.EffectivePermissions(claims)
	if len(perms.Tenants) != 0 {
		t.Errorf("expected the synthetic tenant team-a/<controller> to produce no Tenants entry, got %+v", perms.Tenants)
	}
	if len(perms.Scopes) != 1 {
		t.Fatalf("expected the namespace-scoped grant to remain visible through Scopes, got %d entries", len(perms.Scopes))
	}
}

// TestEffectivePermissions_ClusterWideNoPipelinesVerbEnumeratesNothing
// proves a cluster-wide binding to a role holding no pipelines verb
// contributes no Tenants entry for any Tenant, even though the binding
// matches every Tenant.
func TestEffectivePermissions_ClusterWideNoPipelinesVerbEnumeratesNothing(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "controllers-only"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user1"}},
			RoleRef:  "controllers-only",
			// Scope: nil = cluster-wide.
		}},
	}
	tenants := []*v1alpha1.Tenant{
		{ObjectMeta: metav1.ObjectMeta{Name: "payments"}},
	}
	r := testResolverWithTenants(roles, bindings, tenants)
	a := NewAuthorizer(r, false)
	claims := &auth.Claims{Subject: "user1"}

	perms := a.EffectivePermissions(claims)
	if len(perms.Tenants) != 0 {
		t.Errorf("expected a cluster-wide binding with no pipelines verb to enumerate nothing, got %+v", perms.Tenants)
	}
}

// TestEffectivePermissions_TwoGrantsOnOneTenantProduceTwoEntries proves two
// separate grants on one tenant — Tenant membership and a tenant-scoped
// binding — each produce their own Tenants entry, never merged.
func TestEffectivePermissions_TwoGrantsOnOneTenantProduceTwoEntries(t *testing.T) {
	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{RoleRef: "dev"},
		Status: v1alpha1.TenantStatus{
			GroupRef:   "tenant-payments",
			Conditions: []v1alpha1.TenantCondition{{Type: v1alpha1.TenantConditionRBACReady, Status: metav1.ConditionTrue}},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "tenant-scoped"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user1"}},
			RoleRef:  "reader",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "payments"}}},
		}},
	}
	r := testResolverWithTenants(pipelinesRoles(), bindings, []*v1alpha1.Tenant{tenant})
	a := NewAuthorizer(r, false)
	claims := &auth.Claims{Subject: "user1", Groups: []string{"tenant-payments"}}

	perms := a.EffectivePermissions(claims)
	var count int
	for _, tc := range perms.Tenants {
		if tc.Tenant == "payments" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("expected 2 Tenants entries for payments, got %d: %+v", count, perms.Tenants)
	}
}
