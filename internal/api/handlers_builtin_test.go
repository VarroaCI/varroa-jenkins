package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestHandleBuiltinRoles_ResolvesJenkinsRole proves the built-in list takes
// jenkinsPermissions from the JenkinsRole. A role without one answers an
// empty array, never null.
func TestHandleBuiltinRoles_ResolvesJenkinsRole(t *testing.T) {
	fake := crdstore.NewFake()
	builtin := map[string]string{v1alpha1.LabelBuiltin: "true"}
	crdstore.MustSeed(fake,
		&v1alpha1.VarroaRole{
			ObjectMeta: metav1.ObjectMeta{Name: "operator", Labels: builtin},
			Spec:       v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "varroa-operator"},
		},
		&v1alpha1.VarroaRole{
			ObjectMeta: metav1.ObjectMeta{Name: "viewer", Labels: builtin},
			Spec:       v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "varroa-viewer"},
		},
		&v1alpha1.JenkinsRole{
			ObjectMeta: metav1.ObjectMeta{Name: "varroa-operator"},
			Spec:       v1alpha1.JenkinsRoleSpec{Permissions: []string{"a", "b"}},
		},
	)

	srv := NewServer(&Dependencies{
		Authorizer:        adminTestAuthorizer(),
		Store:             fake,
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
		IdentityConfig:    IdentityConfig{Mode: "local"},
	})
	req := httptest.NewRequest(http.MethodGet, "/builtin-roles", nil)
	req = req.WithContext(contextWithClaims(context.Background(), adminClaims))
	w := httptest.NewRecorder()
	srv.HandleBuiltinRoles(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var env struct {
		Items []struct {
			Name               string   `json:"name"`
			JenkinsPermissions []string `json:"jenkinsPermissions"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	perms := make(map[string][]string)
	for _, item := range env.Items {
		perms[item.Name] = item.JenkinsPermissions
	}
	if got := strings.Join(perms["operator"], ","); got != "a,b" {
		t.Errorf("operator jenkinsPermissions = %q, want a,b", got)
	}
	if _, ok := perms["viewer"]; !ok {
		t.Fatalf("viewer missing from body: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"jenkinsPermissions":null`) {
		t.Errorf("body carries null jenkinsPermissions: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"jenkinsPermissions":[]`) {
		t.Errorf("viewer jenkinsPermissions is not an empty array: %s", w.Body.String())
	}
}
