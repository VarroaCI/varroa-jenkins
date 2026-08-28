package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// fakeActionReconciler is a ReconcilerAPI fake for the hibernate/wake action
// handlers. Only the two request-reply actions record their calls; the rest
// are no-ops.
type fakeActionReconciler struct {
	hibernateErr error
	wakeErr      error
	hibernate    int
	wake         int
}

func (f *fakeActionReconciler) TriggerReconcile(_, _, _ string) {}
func (f *fakeActionReconciler) WakeController(_, _, _ string)   {}
func (f *fakeActionReconciler) Reprovision(_, _, _ string)      {}
func (f *fakeActionReconciler) ApproveRestart(_ context.Context, _, _, _, _ string) error {
	return nil
}
func (f *fakeActionReconciler) ApproveDeletion(_ context.Context, _, _, _, _ string) error {
	return nil
}
func (f *fakeActionReconciler) Hibernate(_ context.Context, _, _, _ string) error {
	f.hibernate++
	return f.hibernateErr
}
func (f *fakeActionReconciler) Wake(_ context.Context, _, _, _ string) error {
	f.wake++
	return f.wakeErr
}

// manageOnlyAuthorizer grants exactly controllers:manage and nothing else, so
// the tests can prove the action routes admit a manage-holding caller without
// requiring update/approve-restart.
func manageOnlyAuthorizer() *Authorizer {
	roles := []*v1alpha1.VarroaRole{{
		ObjectMeta: metav1.ObjectMeta{Name: "manager"},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"manage"}}}},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{{
		ObjectMeta: metav1.ObjectMeta{Name: "manager-binding"},
		Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "managers"}}, RoleRef: "manager"},
	}}
	return NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false)
}

var managerClaims = &auth.Claims{Subject: "mgr", Groups: []string{"managers"}}

func newActionTestServer() (*Server, *fakeActionReconciler) {
	client := newFakeResourceClient()
	client.namespaces["team-a"] = true
	rec := &fakeActionReconciler{}
	srv := NewServer(&Dependencies{
		Client:            client,
		Store:             storeFromFake(client),
		Authorizer:        manageOnlyAuthorizer(),
		Brood:             newFakeBrood(client),
		Reconciler:        rec,
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
	})
	return srv, rec
}

func postAction(t *testing.T, srv *Server, claims *auth.Claims, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if claims != nil {
		req = req.WithContext(contextWithClaims(req.Context(), claims))
	}
	w := httptest.NewRecorder()
	srv.handleClusterDispatch(w, req)
	return w
}

// TestHibernateWakeAdmitManageScope pins the authorization seam: both action
// routes use the per-controller manage check directly (not the PATCH body
// scope), so a caller holding only controllers:manage is admitted.
func TestHibernateWakeAdmitManageScope(t *testing.T) {
	for _, action := range []string{"hibernate", "wake"} {
		t.Run(action, func(t *testing.T) {
			srv, rec := newActionTestServer()
			path := "/clusters/core/controllers/team-a/ci/" + action
			w := postAction(t, srv, managerClaims, path)
			if w.Code != http.StatusAccepted {
				t.Fatalf("manage caller POST %s = %d, want 202: %s", action, w.Code, w.Body.String())
			}
			if action == "hibernate" && rec.hibernate != 1 {
				t.Errorf("hibernate calls = %d, want 1", rec.hibernate)
			}
			if action == "wake" && rec.wake != 1 {
				t.Errorf("wake calls = %d, want 1", rec.wake)
			}
		})
	}

	// A caller without manage must be refused before the reconciler is reached.
	for _, action := range []string{"hibernate", "wake"} {
		t.Run(action+"-forbidden", func(t *testing.T) {
			srv, rec := newActionTestServer()
			path := "/clusters/core/controllers/team-a/ci/" + action
			w := postAction(t, srv, operatorClaims, path)
			if w.Code != http.StatusForbidden {
				t.Fatalf("non-manage caller POST %s = %d, want 403: %s", action, w.Code, w.Body.String())
			}
			if rec.hibernate != 0 || rec.wake != 0 {
				t.Error("reconciler must not be reached for a forbidden caller")
			}
		})
	}
}

// TestHibernateStoppedReturns409 pins the conflict mapping: the operator's
// "conflict" refusal (ErrControllerStopped) surfaces as HTTP 409.
func TestHibernateStoppedReturns409(t *testing.T) {
	srv, _ := newActionTestServer()
	srv.deps.Reconciler = &fakeActionReconciler{
		hibernateErr: &controller.ActionError{Code: bus.CodeConflict, Msg: "controller is stopped (spec.powerState=Stopped)"},
	}
	w := postAction(t, srv, managerClaims, "/clusters/core/controllers/team-a/ci/hibernate")
	if w.Code != http.StatusConflict {
		t.Fatalf("hibernate against a Stopped controller = %d, want 409: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if body["error"] == "" {
		t.Error("409 body must carry the operator's refusal message")
	}
}

// TestControllerDetailCarriesHibernation pins the detail projection: a
// hibernated controller's detail response carries hibernated and hibernatedAt
// while powerState keeps reporting the spec intent.
func TestControllerDetailCarriesHibernation(t *testing.T) {
	client := newFakeResourceClient()
	client.namespaces["team-a"] = true
	client.controllers = map[string]*v1alpha1.Controller{}
	hibernatedAt := metav1.NewTime(time.Date(2026, 7, 20, 12, 30, 0, 0, time.UTC))
	client.controllers["ci"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{PowerState: "Running"},
		Status: v1alpha1.ControllerStatus{
			Phase:        v1alpha1.ControllerPhaseHibernated,
			Hibernated:   true,
			HibernatedAt: &hibernatedAt,
		},
	}
	srv := NewServer(&Dependencies{
		Client:            client,
		Store:             storeFromFake(client),
		Authorizer:        adminTestAuthorizer(),
		Brood:             newFakeBrood(client),
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
	})
	crdstore.MustSeed(client.crdStore, client.controllers["ci"])

	req := httptest.NewRequest(http.MethodGet, "/clusters/core/controllers/team-a/ci", nil)
	req = req.WithContext(contextWithClaims(req.Context(), adminClaims))
	w := httptest.NewRecorder()
	srv.handleControllerDetail(w, req, "core", "team-a", "ci")
	if w.Code != http.StatusOK {
		t.Fatalf("detail = %d, want 200: %s", w.Code, w.Body.String())
	}
	var detail map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail["hibernated"] != true {
		t.Errorf("hibernated = %v, want true", detail["hibernated"])
	}
	gotAt, _ := detail["hibernatedAt"].(string)
	parsedAt, err := time.Parse(time.RFC3339, gotAt)
	if err != nil {
		t.Fatalf("hibernatedAt %q is not RFC 3339: %v", gotAt, err)
	}
	if !parsedAt.Equal(hibernatedAt.Time) {
		t.Errorf("hibernatedAt = %v, want %v", parsedAt, hibernatedAt.Time)
	}
	if detail["powerState"] != "Running" {
		t.Errorf("powerState = %v, want Running (spec intent)", detail["powerState"])
	}
}
