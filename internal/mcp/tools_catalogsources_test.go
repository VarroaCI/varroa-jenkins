package mcp

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// scopedDeveloperDeps builds api.Dependencies with an Authorizer whose caller
// is bound to a varroa:developer VarroaRoleBinding scoped to namespace
// "team-a", granting catalogsources create/update/delete.
func scopedDeveloperDeps() *api.Dependencies {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "developer"},
			Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
				{Resources: []string{"catalogsources"}, Verbs: []string{"read", "create", "update", "delete"}},
			}},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped-developer"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "dev-user"}},
				RoleRef:  "developer",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
			},
		},
	}
	store := crdstore.NewFake()
	// Pre-seed a catalog source so update/delete tests can find it.
	crdstore.MustSeed(store, &v1alpha1.CatalogSource{
		ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "team-a"},
		Spec:       v1alpha1.CatalogSourceSpec{RepoURL: "https://example.com/repo.git"},
	})
	return &api.Dependencies{
		Client:     &stubClient{},
		Authorizer: api.NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false),
		Store:      store,
	}
}

var mcpScopedDevClaims = &auth.Claims{Subject: "dev-user"}

// requireAccessDenied asserts the tool result is specifically the authz
// denial, not some unrelated error (tool wiring, stub client, ...).
func requireAccessDenied(t *testing.T, tr toolResult) {
	t.Helper()
	if !tr.IsError {
		t.Fatal("expected access-denied error, got success")
	}
	if len(tr.Content) == 0 || !strings.Contains(tr.Content[0].Text, "access denied") {
		t.Errorf("expected access-denied message, got: %v", tr.Content)
	}
}

func TestMCPCreateCatalogSource_ScopedDeveloper(t *testing.T) {
	deps := scopedDeveloperDeps()
	handler := NewHandler(deps)

	ownNs := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "create_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-a",
			"name":      "src",
			"repoURL":   "https://example.com/repo.git",
		},
	}, mcpScopedDevClaims)
	tr := parseToolResult(t, ownNs.Result)
	if tr.IsError {
		t.Errorf("create in bound namespace should succeed, got error: %v", tr.Content)
	}

	otherNs := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "create_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-b",
			"name":      "src",
			"repoURL":   "https://example.com/repo.git",
		},
	}, mcpScopedDevClaims)
	requireAccessDenied(t, parseToolResult(t, otherNs.Result))
}

func TestMCPUpdateCatalogSource_ScopedDeveloper(t *testing.T) {
	deps := scopedDeveloperDeps()
	handler := NewHandler(deps)

	ownNs := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-a",
			"name":      "src",
		},
	}, mcpScopedDevClaims)
	tr := parseToolResult(t, ownNs.Result)
	if tr.IsError {
		t.Errorf("update in bound namespace should succeed, got error: %v", tr.Content)
	}

	otherNs := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-b",
			"name":      "src",
		},
	}, mcpScopedDevClaims)
	requireAccessDenied(t, parseToolResult(t, otherNs.Result))
}

func TestMCPDeleteCatalogSource_ScopedDeveloper(t *testing.T) {
	deps := scopedDeveloperDeps()
	handler := NewHandler(deps)

	ownNs := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "delete_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-a",
			"name":      "src",
		},
	}, mcpScopedDevClaims)
	tr := parseToolResult(t, ownNs.Result)
	if tr.IsError {
		t.Errorf("delete in bound namespace should succeed, got error: %v", tr.Content)
	}

	otherNs := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "delete_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-b",
			"name":      "src",
		},
	}, mcpScopedDevClaims)
	requireAccessDenied(t, parseToolResult(t, otherNs.Result))
}

func TestMCPSyncCatalogSource_ScopedDeveloper(t *testing.T) {
	deps := scopedDeveloperDeps()
	handler := NewHandler(deps)

	ownNs := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "sync_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-a",
			"name":      "src",
		},
	}, mcpScopedDevClaims)
	tr := parseToolResult(t, ownNs.Result)
	if tr.IsError {
		t.Errorf("sync in bound namespace should succeed, got error: %v", tr.Content)
	}

	otherNs := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "sync_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-b",
			"name":      "src",
		},
	}, mcpScopedDevClaims)
	requireAccessDenied(t, parseToolResult(t, otherNs.Result))
}
