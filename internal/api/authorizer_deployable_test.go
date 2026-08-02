package api

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// --------------------------------------------------------------------------
// Tests for DeployableNamespaces
// --------------------------------------------------------------------------

// testResolverAndAuthorizer creates a Resolver and Authorizer pre-populated
// with roles and bindings for unit testing.
func testResolverAndAuthorizer(roles []*v1alpha1.VarroaRole, bindings []*v1alpha1.VarroaRoleBinding) *Authorizer {
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

	resolver := rbac.NewResolver(roleInf, bindingInf, jrInf, jrbInf, ctrlInf, false, []string{"sub", "preferred_username"}, []string{"groups"})
	return NewAuthorizer(resolver, false)
}

func makeCreateRole(name string) *v1alpha1.VarroaRole {
	return &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{
				{Resources: []string{"controllers"}, Verbs: []string{"create"}},
			},
		},
	}
}

func TestDeployableNamespaces_UnrestrictedScoped(t *testing.T) {
	// unrestricted=true, scoped=true → out = dedupeSort(managed)
	roles := []*v1alpha1.VarroaRole{makeCreateRole("admin")}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "admin-user"}},
				RoleRef:  "admin",
				// Scope: nil = cluster-wide ⇒ unrestricted
			},
		},
	}
	a := testResolverAndAuthorizer(roles, bindings)
	claims := &auth.Claims{Subject: "admin-user"}

	result := a.DeployableNamespaces(claims, []string{"team-a", "team-b"}, nil, "")
	if result.AllowFreeform {
		t.Error("expected AllowFreeform=false in unrestricted+scoped mode")
	}
	if len(result.Namespaces) != 2 || result.Namespaces[0] != "team-a" || result.Namespaces[1] != "team-b" {
		t.Errorf("expected [team-a team-b], got %v", result.Namespaces)
	}
	if result.DefaultNamespace != "team-a" {
		t.Errorf("expected defaultNamespace=team-a, got %q", result.DefaultNamespace)
	}
}

func TestDeployableNamespaces_UnrestrictedUnscoped(t *testing.T) {
	// unrestricted=true, scoped=false → out = dedupeSort(curated), allowFreeform=true
	roles := []*v1alpha1.VarroaRole{makeCreateRole("admin")}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "admin-user"}},
				RoleRef:  "admin",
			},
		},
	}
	a := testResolverAndAuthorizer(roles, bindings)
	claims := &auth.Claims{Subject: "admin-user"}

	curated := []string{"varroa", "team-x"}
	result := a.DeployableNamespaces(claims, nil, curated, "varroa")
	if !result.AllowFreeform {
		t.Error("expected AllowFreeform=true in unrestricted+unscoped mode")
	}
	if len(result.Namespaces) != 2 || result.Namespaces[0] != "team-x" || result.Namespaces[1] != "varroa" {
		t.Errorf("expected [team-x varroa], got %v", result.Namespaces)
	}
	if result.DefaultNamespace != "varroa" {
		t.Errorf("expected defaultNamespace=varroa, got %q", result.DefaultNamespace)
	}
}

func TestDeployableNamespaces_RestrictedScoped(t *testing.T) {
	// unrestricted=false, scoped=true → out = dedupeSort(intersect(explicit, managed))
	roles := []*v1alpha1.VarroaRole{makeCreateRole("dev")}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "dev-user"}},
				RoleRef:  "dev",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{"team-a", "ns-extra"},
				},
			},
		},
	}
	a := testResolverAndAuthorizer(roles, bindings)
	claims := &auth.Claims{Subject: "dev-user"}

	// explicit = [team-a, ns-extra], managed = [team-a, team-b]
	// intersect = [team-a]
	result := a.DeployableNamespaces(claims, []string{"team-a", "team-b"}, nil, "")
	if result.AllowFreeform {
		t.Error("expected AllowFreeform=false")
	}
	if len(result.Namespaces) != 1 || result.Namespaces[0] != "team-a" {
		t.Errorf("expected [team-a], got %v", result.Namespaces)
	}
	if result.DefaultNamespace != "team-a" {
		t.Errorf("expected defaultNamespace=team-a, got %q", result.DefaultNamespace)
	}
}

func TestDeployableNamespaces_RestrictedScoped_C2Drop(t *testing.T) {
	// C2: explicit ns not in managed should be dropped
	roles := []*v1alpha1.VarroaRole{makeCreateRole("dev")}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "dev-user"}},
				RoleRef:  "dev",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{"ns-a", "ns-c"},
				},
			},
		},
	}
	a := testResolverAndAuthorizer(roles, bindings)
	claims := &auth.Claims{Subject: "dev-user"}

	// explicit = [ns-a, ns-c], managed = [ns-a, ns-b]
	// intersect = [ns-a] (ns-c dropped)
	result := a.DeployableNamespaces(claims, []string{"ns-a", "ns-b"}, nil, "")
	if len(result.Namespaces) != 1 || result.Namespaces[0] != "ns-a" {
		t.Errorf("expected [ns-a] (C2 drop), got %v", result.Namespaces)
	}
}

func TestDeployableNamespaces_RestrictedUnscoped(t *testing.T) {
	// unrestricted=false, scoped=false → out = dedupeSort(explicit)
	roles := []*v1alpha1.VarroaRole{makeCreateRole("dev")}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "dev-user"}},
				RoleRef:  "dev",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{"team-a", "team-b"},
				},
			},
		},
	}
	a := testResolverAndAuthorizer(roles, bindings)
	claims := &auth.Claims{Subject: "dev-user"}

	result := a.DeployableNamespaces(claims, nil, nil, "")
	if result.AllowFreeform {
		t.Error("expected AllowFreeform=false")
	}
	if len(result.Namespaces) != 2 || result.Namespaces[0] != "team-a" || result.Namespaces[1] != "team-b" {
		t.Errorf("expected [team-a team-b], got %v", result.Namespaces)
	}
	if result.DefaultNamespace != "team-a" {
		t.Errorf("expected defaultNamespace=team-a, got %q", result.DefaultNamespace)
	}
}

func TestDeployableNamespaces_NilClaims(t *testing.T) {
	// nil claims → CreateScopeNamespaces returns (nil, false) → !unrestricted
	// → out = dedupeSort(explicit) where explicit is nil → []
	a := testResolverAndAuthorizer(nil, nil)
	result := a.DeployableNamespaces(nil, nil, nil, "")
	if result.AllowFreeform {
		t.Error("expected AllowFreeform=false for nil claims")
	}
	if result.Namespaces == nil {
		t.Error("expected non-nil Namespaces for nil claims")
	}
	if len(result.Namespaces) != 0 {
		t.Errorf("expected empty Namespaces for nil claims, got %v", result.Namespaces)
	}
	if result.DefaultNamespace != "" {
		t.Errorf("expected empty defaultNamespace, got %q", result.DefaultNamespace)
	}
}

func TestDeployableNamespaces_DefaultNamespaceSelection(t *testing.T) {
	// curatedDefault "team-a" is in explicit → defaultNamespace = "team-a"
	roles := []*v1alpha1.VarroaRole{makeCreateRole("dev")}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "dev-user"}},
				RoleRef:  "dev",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{"team-b", "team-a"},
				},
			},
		},
	}
	a := testResolverAndAuthorizer(roles, bindings)
	claims := &auth.Claims{Subject: "dev-user"}

	// explicit = [team-a, team-b] (after sorting), curatedDefault = "team-a"
	result := a.DeployableNamespaces(claims, nil, nil, "team-a")
	if result.DefaultNamespace != "team-a" {
		t.Errorf("expected defaultNamespace=team-a (in-set), got %q", result.DefaultNamespace)
	}
}

func TestDeployableNamespaces_DefaultNamespaceNotInSet(t *testing.T) {
	// curatedDefault "team-x" NOT in explicit → defaultNamespace = out[0] (sorted)
	roles := []*v1alpha1.VarroaRole{makeCreateRole("dev")}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "dev-user"}},
				RoleRef:  "dev",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{"team-b", "team-a"},
				},
			},
		},
	}
	a := testResolverAndAuthorizer(roles, bindings)
	claims := &auth.Claims{Subject: "dev-user"}

	result := a.DeployableNamespaces(claims, nil, nil, "not-here")
	if result.DefaultNamespace != "team-a" {
		t.Errorf("expected defaultNamespace=team-a (first sorted), got %q", result.DefaultNamespace)
	}
}

func TestDeployableNamespaces_DefaultNamespaceEmptyOut(t *testing.T) {
	// No explicit namespaces + no curated + no managed = empty out → defaultNamespace = ""
	a := testResolverAndAuthorizer(nil, nil)
	claims := &auth.Claims{Subject: "nobody"}

	result := a.DeployableNamespaces(claims, nil, nil, "varroa")
	if result.DefaultNamespace != "" {
		t.Errorf("expected empty defaultNamespace when out is empty, got %q", result.DefaultNamespace)
	}
}

func TestDeployableNamespaces_NamespacesNeverNil(t *testing.T) {
	a := testResolverAndAuthorizer(nil, nil)
	result := a.DeployableNamespaces(nil, nil, nil, "")
	if result.Namespaces == nil {
		t.Error("Namespaces must not be nil")
	}
}
