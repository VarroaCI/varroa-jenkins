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
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/auth/identity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// This file uses recordingActivitySink, defined in
// auth_oidc_handlers_test.go. Each test wires a fresh one into
// deps.ActivityPublisher and asserts the handler calls notifyActivity
// unconditionally, with no dependency on any other field of Dependencies.

// adminAuthorizer grants every capability, so this file's tests exercise
// the publish call, not the authorization gate in front of it.
func adminAuthorizer() *Authorizer {
	roles := []*v1alpha1.VarroaRole{{
		ObjectMeta: metav1.ObjectMeta{Name: "admin"},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}}},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{{
		ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
		Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "admin"}}, RoleRef: "admin"},
	}}
	return NewAuthorizer(testResolver(roles, bindings), false)
}

var publishClaims = &auth.Claims{Subject: "admin", Email: "admin@example.com"}

// fakeReconcilerAPI is an all-success controller.ReconcilerAPI double.
type fakeReconcilerAPI struct{}

func (fakeReconcilerAPI) TriggerReconcile(_, _, _ string) {}
func (fakeReconcilerAPI) WakeController(_, _, _ string)   {}
func (fakeReconcilerAPI) Reprovision(_, _, _ string)      {}
func (fakeReconcilerAPI) ApproveRestart(_ context.Context, _, _, _, _, _ string) error {
	return nil
}
func (fakeReconcilerAPI) ApproveDeletion(_ context.Context, _, _, _, _ string) error {
	return nil
}
func (fakeReconcilerAPI) Hibernate(_ context.Context, _, _, _ string) error { return nil }
func (fakeReconcilerAPI) Wake(_ context.Context, _, _, _ string) error      { return nil }

// fakeBroodForCreate implements Brood with only Create/LocalCluster doing
// real work — enough for handleCreateController to reach notifyActivity.
type fakeBroodForCreate struct{ local string }

func (f *fakeBroodForCreate) LocalCluster() string { return f.local }
func (f *fakeBroodForCreate) Clusters(_ context.Context) ([]ClusterInfo, error) {
	return nil, nil
}
func (f *fakeBroodForCreate) IsKnown(_ context.Context, _ string) bool { return true }
func (f *fakeBroodForCreate) ListAll(_ context.Context, _, _ string) ([]ClusterController, []ClusterFanoutStatus, error) {
	return nil, nil, nil
}
func (f *fakeBroodForCreate) Get(_ context.Context, _, _, _ string) (*v1alpha1.Controller, error) {
	return nil, nil
}
func (f *fakeBroodForCreate) Create(_ context.Context, cluster string, req ControllersCreateArgs) (*v1alpha1.Controller, []bus.Check, error) {
	cr := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Namespace: req.Namespace}}
	_ = json.Unmarshal(req.Controller, cr)
	return cr, nil, nil
}
func (f *fakeBroodForCreate) Preflight(_ context.Context, _ string, _ ControllersCreateArgs) ([]bus.Check, error) {
	return nil, nil
}
func (f *fakeBroodForCreate) Update(_ context.Context, _, _, _ string, _ json.RawMessage, _ string, _ bool) (*v1alpha1.Controller, []bus.Check, []bus.UnappliedRemoval, error) {
	return nil, nil, nil, nil
}
func (f *fakeBroodForCreate) Delete(_ context.Context, _, _, _ string) error    { return nil }
func (f *fakeBroodForCreate) DeletePod(_ context.Context, _, _, _ string) error { return nil }
func (f *fakeBroodForCreate) Drain(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (f *fakeBroodForCreate) DrainCancel(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (f *fakeBroodForCreate) StateOf(_ context.Context, _ string) (string, error) { return "", nil }
func (f *fakeBroodForCreate) DiscoverNamespaces(_ context.Context, _ string) (*bus.NamespacesListResponse, error) {
	return nil, nil
}

// publishTestServer builds a Server with an admin Authorizer, a recording
// ActivityPublisher, a fake Reconciler, and a fake ResourceClient/Store —
// enough for every one of the 17 notifyActivity call sites to be reached.
func publishTestServer(t *testing.T) (*Server, *recordingActivitySink) {
	t.Helper()
	sink := &recordingActivitySink{}
	fc := newFakeResourceClient()
	deps := &Dependencies{
		Authorizer:        adminAuthorizer(),
		ActivityPublisher: sink,
		Reconciler:        fakeReconcilerAPI{},
		Client:            fc,
		Store:             storeFromFake(fc),
		Logger:            slog.Default(),
		OperatorNamespace: "varroa-system",
	}
	return NewServer(deps), sink
}

func withClaims(r *http.Request) *http.Request {
	return r.WithContext(contextWithClaims(r.Context(), publishClaims))
}

func TestNotifyActivity_HandleLogout(t *testing.T) {
	srv, sink := publishTestServer(t)
	r := withClaims(httptest.NewRequest(http.MethodPost, "/logout", nil))
	srv.HandleLogout(httptest.NewRecorder(), r)
	assertPublished(t, sink, "logout")
}

func TestNotifyActivity_HandleUpdatePreferences(t *testing.T) {
	srv, sink := publishTestServer(t)
	seedMeUser(t, srv, "1")
	body := `{"theme":"dark"}`
	r := withClaims(httptest.NewRequest(http.MethodPut, "/me/preferences", strings.NewReader(body)))
	srv.HandleUpdatePreferences(httptest.NewRecorder(), r)
	assertPublished(t, sink, "preferences.updated")
}

// seedMeUser stores the User that GET /me and PUT /me/preferences share for
// publishClaims, at the given resourceVersion.
func seedMeUser(t *testing.T, srv *Server, rv string) {
	t.Helper()
	name := identity.UserResourceName(publishClaims, auth.AuthMode(srv.deps.IdentityConfig.Mode))
	crdstore.MustSeed(srv.deps.Store.(*crdstore.Fake), &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "varroa-system", ResourceVersion: rv},
	})
}

// TestHandleUpdatePreferences_WritesStatusOfMeUser pins that a preferences
// write lands in status.preferences of the User that GET /me reads, and that
// a concurrent write to that User survives it.
func TestHandleUpdatePreferences_WritesStatusOfMeUser(t *testing.T) {
	srv, _ := publishTestServer(t)
	ctx := context.Background()
	st := srv.deps.Store.(*crdstore.Fake)
	seedMeUser(t, srv, "1")
	name := identity.UserResourceName(publishClaims, auth.AuthMode(srv.deps.IdentityConfig.Mode))
	gvr, _ := crdstore.GVRFor[v1alpha1.User]()
	fired := false
	st.SetAfterGet(gvr, func(ns, n string) {
		if fired {
			return
		}
		fired = true
		if _, err := st.PatchObjectMetaAt(ctx, gvr, ns, n, "1", map[string]any{
			"annotations": map[string]any{v1alpha1.AnnotationOIDCFormerIdentifiers: `["bob"]`},
		}); err != nil {
			t.Errorf("concurrent patch: %v", err)
		}
	})

	w := httptest.NewRecorder()
	r := withClaims(httptest.NewRequest(http.MethodPut, "/me/preferences", strings.NewReader(`{"theme":"dark"}`)))
	srv.HandleUpdatePreferences(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s; want 200", w.Code, w.Body.String())
	}
	got, err := crdstore.Get[v1alpha1.User](ctx, st, name, "varroa-system")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Preferences == nil || got.Status.Preferences.Theme != "dark" {
		t.Fatalf("status.preferences = %+v, want theme dark", got.Status.Preferences)
	}
	if got.Annotations[v1alpha1.AnnotationOIDCFormerIdentifiers] != `["bob"]` {
		t.Errorf("former identifiers annotation = %q, want the concurrent value", got.Annotations[v1alpha1.AnnotationOIDCFormerIdentifiers])
	}
}

// TestHandleUpdatePreferences_NoUserIs404 pins that the handler does not
// create a User when none exists for the caller.
func TestHandleUpdatePreferences_NoUserIs404(t *testing.T) {
	srv, _ := publishTestServer(t)
	w := httptest.NewRecorder()
	r := withClaims(httptest.NewRequest(http.MethodPut, "/me/preferences", strings.NewReader(`{"theme":"dark"}`)))
	srv.HandleUpdatePreferences(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestNotifyActivity_HandleReconcile(t *testing.T) {
	srv, sink := publishTestServer(t)
	r := withClaims(httptest.NewRequest(http.MethodPost, "/controllers/ns/ctrl/reconcile", nil))
	srv.handleReconcile(httptest.NewRecorder(), r, "core", "ns", "ctrl")
	assertPublished(t, sink, "reconcile.triggered")
}

func TestNotifyActivity_HandleApproveRestart(t *testing.T) {
	srv, sink := publishTestServer(t)
	r := withClaims(httptest.NewRequest(http.MethodPost, "/controllers/ns/ctrl/approve", strings.NewReader(`{"action":"restart"}`)))
	srv.handleApproveRestart(httptest.NewRecorder(), r, "core", "ns", "ctrl")
	assertPublished(t, sink, "restart.approved")
}

func TestNotifyActivity_HandleApproveDeletion(t *testing.T) {
	srv, sink := publishTestServer(t)
	r := withClaims(httptest.NewRequest(http.MethodPost, "/controllers/ns/ctrl/approve-deletion", strings.NewReader(`{"path":"jobs/foo"}`)))
	srv.handleApproveDeletion(httptest.NewRecorder(), r, "core", "ns", "ctrl")
	assertPublished(t, sink, "deletion.approved")
}

func TestNotifyActivity_HandleReprovision(t *testing.T) {
	srv, sink := publishTestServer(t)
	r := withClaims(httptest.NewRequest(http.MethodPost, "/controllers/ns/ctrl/reprovision", nil))
	srv.handleReprovision(httptest.NewRecorder(), r, "core", "ns", "ctrl")
	assertPublished(t, sink, "controller.reprovisioned")
}

func TestNotifyActivity_HandleRestartController(t *testing.T) {
	srv, sink := publishTestServer(t)
	r := withClaims(httptest.NewRequest(http.MethodPost, "/controllers/ns/ctrl/restart", nil))
	srv.handleRestartController(httptest.NewRecorder(), r, "core", "ns", "ctrl")
	assertPublished(t, sink, "controller.restarted")
}

func TestNotifyActivity_HandleHibernate(t *testing.T) {
	srv, sink := publishTestServer(t)
	r := withClaims(httptest.NewRequest(http.MethodPost, "/clusters/core/controllers/ns/ctrl/hibernate", nil))
	srv.handleHibernate(httptest.NewRecorder(), r, "core", "ns", "ctrl")
	assertPublished(t, sink, "controller.hibernated")
}

func TestNotifyActivity_HandleWake(t *testing.T) {
	srv, sink := publishTestServer(t)
	r := withClaims(httptest.NewRequest(http.MethodPost, "/clusters/core/controllers/ns/ctrl/wake", nil))
	srv.handleWake(httptest.NewRecorder(), r, "core", "ns", "ctrl")
	assertPublished(t, sink, "controller.woken")
}

func TestNotifyActivity_HandleCreateController(t *testing.T) {
	srv, sink := publishTestServer(t)
	srv.deps.Brood = &fakeBroodForCreate{local: "core"} // cluster below != local, so ValidateBundleRef is skipped
	body := `{"apiVersion":"varroa.dev/v1alpha1","kind":"Controller","metadata":{"name":"ctrl"}}`
	r := withClaims(httptest.NewRequest(http.MethodPost, "/controllers/ns", strings.NewReader(body)))
	srv.handleCreateController(httptest.NewRecorder(), r, "remote", "ns")
	assertPublished(t, sink, "controller.created")
}

func TestNotifyActivity_HandleDeleteController(t *testing.T) {
	srv, sink := publishTestServer(t)
	r := withClaims(httptest.NewRequest(http.MethodDelete, "/controllers/ns/ctrl", nil))
	srv.handleDeleteController(httptest.NewRecorder(), r, "core", "ns", "ctrl")
	assertPublished(t, sink, "controller.deleted")
}

func TestNotifyActivity_VarroaRoleCreate(t *testing.T) {
	srv, sink := publishTestServer(t)
	body := `{"metadata":{"name":"role-a"}}`
	r := withClaims(httptest.NewRequest(http.MethodPost, "/roles", strings.NewReader(body)))
	srv.handleVarroaRoles(httptest.NewRecorder(), r)
	assertPublished(t, sink, "varroarole.created")
}

func TestNotifyActivity_VarroaRoleUpdate(t *testing.T) {
	srv, sink := publishTestServer(t)
	body := `{"metadata":{"name":"role-a"}}`
	r := withClaims(httptest.NewRequest(http.MethodPut, "/roles/role-a", strings.NewReader(body)))
	srv.handleVarroaRoleDispatch(httptest.NewRecorder(), r)
	assertPublished(t, sink, "varroarole.updated")
}

func TestNotifyActivity_VarroaRoleDelete(t *testing.T) {
	srv, sink := publishTestServer(t)
	r := withClaims(httptest.NewRequest(http.MethodDelete, "/roles/role-a", nil))
	srv.handleVarroaRoleDispatch(httptest.NewRecorder(), r)
	assertPublished(t, sink, "varroarole.deleted")
}

func TestNotifyActivity_VarroaRoleBindingCreate(t *testing.T) {
	srv, sink := publishTestServer(t)
	body := `{"metadata":{"name":"binding-a"}}`
	r := withClaims(httptest.NewRequest(http.MethodPost, "/rolebindings", strings.NewReader(body)))
	srv.handleVarroaRoleBindings(httptest.NewRecorder(), r)
	assertPublished(t, sink, "varroarolebinding.created")
}

func TestNotifyActivity_VarroaRoleBindingUpdate(t *testing.T) {
	srv, sink := publishTestServer(t)
	body := `{"metadata":{"name":"binding-a"}}`
	r := withClaims(httptest.NewRequest(http.MethodPut, "/rolebindings/binding-a", strings.NewReader(body)))
	srv.handleVarroaRoleBindingDispatch(httptest.NewRecorder(), r)
	assertPublished(t, sink, "varroarolebinding.updated")
}

func TestNotifyActivity_VarroaRoleBindingDelete(t *testing.T) {
	srv, sink := publishTestServer(t)
	r := withClaims(httptest.NewRequest(http.MethodDelete, "/rolebindings/binding-a", nil))
	srv.handleVarroaRoleBindingDispatch(httptest.NewRecorder(), r)
	assertPublished(t, sink, "varroarolebinding.deleted")
}

func assertPublished(t *testing.T, sink *recordingActivitySink, wantType string) {
	t.Helper()
	if len(sink.events) != 1 {
		t.Fatalf("got %d published events, want 1: %+v", len(sink.events), sink.events)
	}
	if sink.events[0].Type != wantType {
		t.Errorf("published Type = %q, want %q", sink.events[0].Type, wantType)
	}
}
