package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// adminTestAuthorizer builds an Authorizer that knows two subjects: an admin
// (group "admins", wildcard */*) and an operator (group "ops", controllers
// create only). This pins the security-critical distinction that the admin
// Settings sections are gated on varroa:admin, not on controllers:create.
func adminTestAuthorizer() *Authorizer {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "admin"},
			Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "operator"},
			Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read", "create"}}}},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
			Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "admins"}}, RoleRef: "admin"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "operator-binding"},
			Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "ops"}}, RoleRef: "operator"},
		},
	}
	return NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false)
}

var (
	adminClaims    = &auth.Claims{Subject: "admin-user", Email: "admin@test.com", Groups: []string{"admins"}}
	operatorClaims = &auth.Claims{Subject: "op-user", Groups: []string{"ops"}}
)

func TestIsAdmin_OperatorIsNotAdmin(t *testing.T) {
	a := adminTestAuthorizer()
	if !a.IsAdmin(adminClaims) {
		t.Error("admin (wildcard */*) should satisfy IsAdmin")
	}
	if a.IsAdmin(operatorClaims) {
		t.Error("operator must NOT satisfy IsAdmin")
	}
	// Sanity: the operator CAN create controllers — exactly why the old gate
	// was wrong for admin-only sections.
	if !a.CanCreateController(operatorClaims, "", "") {
		t.Error("operator is expected to have controllers:create")
	}
}

func TestIdentitySettings_AdminGate(t *testing.T) {
	srv := NewServer(&Dependencies{
		Authorizer:     adminTestAuthorizer(),
		IdentityConfig: IdentityConfig{Mode: "local", CookieDomain: "example.com", DefaultRead: true},
		Logger:         slog.Default(),
	})

	// Operator is forbidden.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/identity-settings", nil)
	srv.HandleIdentitySettings(w, req.WithContext(contextWithClaims(req.Context(), operatorClaims)))
	if w.Code != http.StatusForbidden {
		t.Errorf("operator: expected 403, got %d", w.Code)
	}

	// Admin is allowed and the secret is never present.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/identity-settings", nil)
	srv.HandleIdentitySettings(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)))
	if w.Code != http.StatusOK {
		t.Fatalf("admin: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); contains(body, "secret") || contains(body, "clientSecret") {
		t.Errorf("identity-settings must never expose a secret, got: %s", body)
	}
}

func TestListUsers_AdminGate(t *testing.T) {
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{
		Client:            client,
		Store:             storeFromFake(client),
		Authorizer:        adminTestAuthorizer(),
		IdentityConfig:    IdentityConfig{Mode: "local"},
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	srv.HandleUsers(w, req.WithContext(contextWithClaims(req.Context(), operatorClaims)))
	if w.Code != http.StatusForbidden {
		t.Errorf("operator listing users: expected 403, got %d", w.Code)
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/users", nil)
	srv.HandleUsers(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)))
	if w.Code != http.StatusOK {
		t.Errorf("admin listing users: expected 200, got %d", w.Code)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
