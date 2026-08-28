package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// scopedDeveloperAuthorizer builds an Authorizer for a caller whose
// varroa:developer VarroaRoleBinding is scoped to namespace "team-a" only,
// granting catalogsources read/create/update/delete plus catalogitems read.
func scopedDeveloperAuthorizer() *Authorizer {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "developer"},
			Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
				{Resources: []string{"catalogsources"}, Verbs: []string{"read", "create", "update", "delete"}},
				{Resources: []string{"catalogitems"}, Verbs: []string{"read"}},
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
	return NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false)
}

var scopedDevClaims = &auth.Claims{Subject: "dev-user"}

func newCatalogSourcesTestServer() (*Server, *scopeTestConfigBrood) {
	brood := &scopeTestConfigBrood{}
	srv := NewServer(&Dependencies{
		Authorizer:  scopedDeveloperAuthorizer(),
		ConfigBrood: brood,
		Logger:      slog.Default(),
	})
	return srv, brood
}

func TestDispatchCatalogSources_ScopedDeveloper_CreateOwnNamespace(t *testing.T) {
	srv, brood := newCatalogSourcesTestServer()
	body, _ := json.Marshal(map[string]interface{}{"metadata": map[string]string{"name": "src"}})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/clusters/core/catalogsources?namespace=team-a", strings.NewReader(string(body)))
	srv.dispatchCatalogSources(w, req.WithContext(contextWithClaims(req.Context(), scopedDevClaims)), "core", nil)

	if w.Code != http.StatusCreated {
		t.Errorf("create own-ns: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if !brood.createCatalogSourceCalled {
		t.Error("ConfigBrood.CreateCatalogSource was not called")
	}
}

func TestDispatchCatalogSources_ScopedDeveloper_CreateOtherNamespace(t *testing.T) {
	srv, brood := newCatalogSourcesTestServer()
	body, _ := json.Marshal(map[string]interface{}{"metadata": map[string]string{"name": "src"}})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/clusters/core/catalogsources?namespace=team-b", strings.NewReader(string(body)))
	srv.dispatchCatalogSources(w, req.WithContext(contextWithClaims(req.Context(), scopedDevClaims)), "core", nil)

	if w.Code != http.StatusForbidden {
		t.Errorf("create other-ns: expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if brood.createCatalogSourceCalled {
		t.Error("ConfigBrood.CreateCatalogSource must not be called when authz denies")
	}
}

func TestDispatchCatalogSources_ScopedDeveloper_PutOwnNamespace(t *testing.T) {
	srv, brood := newCatalogSourcesTestServer()
	body, _ := json.Marshal(map[string]interface{}{"metadata": map[string]string{"name": "src"}})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/clusters/core/catalogsources/team-a/src", strings.NewReader(string(body)))
	srv.dispatchCatalogSources(w, req.WithContext(contextWithClaims(req.Context(), scopedDevClaims)), "core", []string{"team-a", "src"})

	if w.Code != http.StatusOK {
		t.Errorf("put own-ns: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !brood.updateCatalogSourceCalled {
		t.Error("ConfigBrood.UpdateCatalogSource was not called")
	}
}

func TestDispatchCatalogSources_ScopedDeveloper_PutOtherNamespace(t *testing.T) {
	srv, brood := newCatalogSourcesTestServer()
	body, _ := json.Marshal(map[string]interface{}{"metadata": map[string]string{"name": "src"}})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/clusters/core/catalogsources/team-b/src", strings.NewReader(string(body)))
	srv.dispatchCatalogSources(w, req.WithContext(contextWithClaims(req.Context(), scopedDevClaims)), "core", []string{"team-b", "src"})

	if w.Code != http.StatusForbidden {
		t.Errorf("put other-ns: expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if brood.updateCatalogSourceCalled {
		t.Error("ConfigBrood.UpdateCatalogSource must not be called when authz denies")
	}
}

func TestDispatchCatalogSources_ScopedDeveloper_DeleteOwnNamespace(t *testing.T) {
	srv, brood := newCatalogSourcesTestServer()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/clusters/core/catalogsources/team-a/src", nil)
	srv.dispatchCatalogSources(w, req.WithContext(contextWithClaims(req.Context(), scopedDevClaims)), "core", []string{"team-a", "src"})

	if w.Code != http.StatusNoContent {
		t.Errorf("delete own-ns: expected 204, got %d: %s", w.Code, w.Body.String())
	}
	if !brood.deleteCatalogSourceCalled {
		t.Error("ConfigBrood.DeleteCatalogSource was not called")
	}
}

func TestDispatchCatalogSources_ScopedDeveloper_DeleteOtherNamespace(t *testing.T) {
	srv, brood := newCatalogSourcesTestServer()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/clusters/core/catalogsources/team-b/src", nil)
	srv.dispatchCatalogSources(w, req.WithContext(contextWithClaims(req.Context(), scopedDevClaims)), "core", []string{"team-b", "src"})

	if w.Code != http.StatusForbidden {
		t.Errorf("delete other-ns: expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if brood.deleteCatalogSourceCalled {
		t.Error("ConfigBrood.DeleteCatalogSource must not be called when authz denies")
	}
}

func TestDispatchCatalogSources_ScopedDeveloper_SyncOwnNamespace(t *testing.T) {
	srv, brood := newCatalogSourcesTestServer()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/clusters/core/catalogsources/team-a/src/sync", nil)
	srv.dispatchCatalogSources(w, req.WithContext(contextWithClaims(req.Context(), scopedDevClaims)), "core", []string{"team-a", "src", "sync"})

	if w.Code != http.StatusAccepted {
		t.Errorf("sync own-ns: expected 202, got %d: %s", w.Code, w.Body.String())
	}
	if !brood.syncCatalogSourceCalled {
		t.Error("ConfigBrood.SyncCatalogSource was not called")
	}
}

func TestDispatchCatalogSources_ScopedDeveloper_SyncOtherNamespace(t *testing.T) {
	srv, brood := newCatalogSourcesTestServer()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/clusters/core/catalogsources/team-b/src/sync", nil)
	srv.dispatchCatalogSources(w, req.WithContext(contextWithClaims(req.Context(), scopedDevClaims)), "core", []string{"team-b", "src", "sync"})

	if w.Code != http.StatusForbidden {
		t.Errorf("sync other-ns: expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if brood.syncCatalogSourceCalled {
		t.Error("ConfigBrood.SyncCatalogSource must not be called when authz denies")
	}
}

// TestDispatchCatalogSources_ScopedDeveloper_GetReachableRegardlessOfNamespace
// pins down that the single-item GET (read) stays namespace-blind: a scoped
// developer bound only to team-a can still read a CatalogSource in team-b.
func TestDispatchCatalogSources_ScopedDeveloper_GetReachableRegardlessOfNamespace(t *testing.T) {
	srv, _ := newCatalogSourcesTestServer()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/clusters/core/catalogsources/team-b/src", nil)
	srv.dispatchCatalogSources(w, req.WithContext(contextWithClaims(req.Context(), scopedDevClaims)), "core", []string{"team-b", "src"})

	if w.Code != http.StatusOK {
		t.Errorf("get other-ns (read stays global): expected 200, got %d: %s", w.Code, w.Body.String())
	}
}
