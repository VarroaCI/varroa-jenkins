package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/preflight"
)

// A path-mode draft whose host does not equal the dashboard host must fail
// preflight the same way create (ValidateIngress) rejects it — the rule
// lives in the BFF, and the operator-side preflight cannot see
// DashboardHost, so the handler mirrors it as the "ingress-mode" check.
func TestHandlePreflightController_PathModeIngressCheck(t *testing.T) {
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "creator"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read", "create"}}},
		},
	}
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide-creator"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
			RoleRef:  "creator",
		},
	}
	r := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	fc := newFakeResourceClient()
	fc.controllers = make(map[string]*v1alpha1.Controller)
	fc.namespaces["team-a"] = true
	srv := NewServer(&Dependencies{
		Authorizer:        NewAuthorizer(r, false),
		Client:            fc,
		Store:             storeFromFake(fc),
		Logger:            slog.Default(),
		Brood:             newFakeBrood(fc),
		DashboardHost:     "varroa.example.com",
		OperatorNamespace: "varroa-system",
	})
	claims := userAClaims()

	run := func(t *testing.T, spec string) []preflight.Check {
		t.Helper()
		body := fmt.Sprintf(`{"metadata":{"name":"newctrl","namespace":"team-a"},"spec":%s}`, spec)
		req := httptest.NewRequest(http.MethodPost, "/clusters/core/controllers/team-a/preflight", strings.NewReader(body)).
			WithContext(authContext(claims))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.handlePreflightController(w, req, "core", "team-a")
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Checks []preflight.Check `json:"checks"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v; body: %s", err, w.Body.String())
		}
		return resp.Checks
	}

	find := func(checks []preflight.Check) *preflight.Check {
		for i := range checks {
			if checks[i].ID == "ingress-mode" {
				return &checks[i]
			}
		}
		return nil
	}

	t.Run("path mode with wrong host fails naming the dashboard host", func(t *testing.T) {
		c := find(run(t, `{"version":"2.570.0","ingressSpec":{"mode":"path","host":"newctrl.example.com"}}`))
		if c == nil {
			t.Fatal("no ingress-mode check in response")
		}
		if c.Status != "fail" {
			t.Fatalf("expected fail, got %q (%s)", c.Status, c.Message)
		}
		if !strings.Contains(c.Message, "varroa.example.com") {
			t.Fatalf("message should name the expected dashboard host, got %q", c.Message)
		}
	})

	t.Run("path mode with the dashboard host passes", func(t *testing.T) {
		c := find(run(t, `{"version":"2.570.0","ingressSpec":{"mode":"path","host":"varroa.example.com"}}`))
		if c == nil {
			t.Fatal("no ingress-mode check in response")
		}
		if c.Status != "pass" {
			t.Fatalf("expected pass, got %q (%s)", c.Status, c.Message)
		}
	})

	t.Run("explicit subdomain mode passes", func(t *testing.T) {
		c := find(run(t, `{"version":"2.570.0","ingressSpec":{"mode":"subdomain"}}`))
		if c == nil {
			t.Fatal("no ingress-mode check in response")
		}
		if c.Status != "pass" {
			t.Fatalf("expected pass, got %q (%s)", c.Status, c.Message)
		}
	})

	t.Run("no ingressSpec emits no ingress-mode check", func(t *testing.T) {
		if c := find(run(t, `{"version":"2.570.0"}`)); c != nil {
			t.Fatalf("expected no ingress-mode check, got %q (%s)", c.Status, c.Message)
		}
	})
}
