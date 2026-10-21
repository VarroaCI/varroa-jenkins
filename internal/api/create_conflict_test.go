package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// The create routes write through crdstore.Create. A name that already exists
// answers 409 and leaves the stored object unchanged.

func TestHandleCreateGroup_ExistingNameAnswers409(t *testing.T) {
	srv, sink := publishTestServer(t)
	srv.deps.IdentityConfig = IdentityConfig{Mode: "local"}
	st := srv.deps.Store.(*crdstore.Fake)
	crdstore.MustSeed(st, &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{Name: "g1"},
		Spec:       v1alpha1.GroupSpec{Members: []string{"alice"}},
	})

	w := httptest.NewRecorder()
	srv.handleCreateGroup(w, withClaims(httptest.NewRequest(http.MethodPost, "/groups", strings.NewReader(`{"name":"g1","members":["bob"]}`))))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %s; want 409", w.Code, w.Body.String())
	}
	got, err := crdstore.Get[v1alpha1.Group](context.Background(), st, "g1", "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Spec.Members) != 1 || got.Spec.Members[0] != "alice" {
		t.Errorf("members = %v, want [alice] kept", got.Spec.Members)
	}
	if len(sink.events) != 0 {
		t.Errorf("events = %+v, want none for a refused create", sink.events)
	}
}

// TestHandleUpdateGroup_EditsMembers pins that PUT /groups/{name} changes the
// member list of an existing group, and answers 404 for a missing one.
func TestHandleUpdateGroup_EditsMembers(t *testing.T) {
	srv, sink := publishTestServer(t)
	srv.deps.IdentityConfig = IdentityConfig{Mode: "local"}
	st := srv.deps.Store.(*crdstore.Fake)
	crdstore.MustSeed(st, &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{Name: "g1"},
		Spec:       v1alpha1.GroupSpec{Members: []string{"alice"}},
	})

	w := httptest.NewRecorder()
	srv.HandleGroupDispatch(w, withClaims(httptest.NewRequest(http.MethodPut, "/groups/g1", strings.NewReader(`{"displayName":"Devs","members":["alice","bob"]}`))))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s; want 200", w.Code, w.Body.String())
	}
	got, err := crdstore.Get[v1alpha1.Group](context.Background(), st, "g1", "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Spec.Members) != 2 || got.Spec.DisplayName != "Devs" {
		t.Errorf("group = %+v, want members [alice bob] and displayName Devs", got.Spec)
	}
	if len(sink.events) != 1 {
		t.Errorf("events = %d, want 1 for the update", len(sink.events))
	}

	missing := httptest.NewRecorder()
	srv.HandleGroupDispatch(missing, withClaims(httptest.NewRequest(http.MethodPut, "/groups/nope", strings.NewReader(`{"members":[]}`))))
	if missing.Code != http.StatusNotFound {
		t.Errorf("missing group status = %d, want 404", missing.Code)
	}
}

func TestRESTVarroaRoleCreate_ExistingNameAnswers409(t *testing.T) {
	srv, sink := publishTestServer(t)
	st := srv.deps.Store.(*crdstore.Fake)
	crdstore.MustSeed(st, &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "role-a"},
		Spec:       v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "keep"},
	})

	w := httptest.NewRecorder()
	srv.handleVarroaRoles(w, withClaims(httptest.NewRequest(http.MethodPost, "/roles", strings.NewReader(`{"metadata":{"name":"role-a"},"spec":{"jenkinsRoleRef":"replace"}}`))))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %s; want 409", w.Code, w.Body.String())
	}
	got, err := crdstore.Get[v1alpha1.VarroaRole](context.Background(), st, "role-a", "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.JenkinsRoleRef != "keep" {
		t.Errorf("jenkinsRoleRef = %q, want keep", got.Spec.JenkinsRoleRef)
	}
	if len(sink.events) != 0 {
		t.Errorf("events = %+v, want none for a refused create", sink.events)
	}
}

func TestRESTVarroaRoleBindingCreate_ExistingNameAnswers409(t *testing.T) {
	srv, sink := publishTestServer(t)
	st := srv.deps.Store.(*crdstore.Fake)
	crdstore.MustSeed(st, &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "binding-a"},
		Spec:       v1alpha1.VarroaRoleBindingSpec{RoleRef: "keep"},
	})

	w := httptest.NewRecorder()
	srv.handleVarroaRoleBindings(w, withClaims(httptest.NewRequest(http.MethodPost, "/rolebindings", strings.NewReader(`{"metadata":{"name":"binding-a"},"spec":{"roleRef":"replace"}}`))))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %s; want 409", w.Code, w.Body.String())
	}
	got, err := crdstore.Get[v1alpha1.VarroaRoleBinding](context.Background(), st, "binding-a", "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.RoleRef != "keep" {
		t.Errorf("roleRef = %q, want keep", got.Spec.RoleRef)
	}
	if len(sink.events) != 0 {
		t.Errorf("events = %+v, want none for a refused create", sink.events)
	}
}

func TestTeamHandlers_AdminCreate_ExistingNameAnswers409(t *testing.T) {
	srv, _ := publishTestServer(t)
	st := srv.deps.Store.(*crdstore.Fake)
	crdstore.MustSeed(st, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "new-team"},
		Spec:       v1alpha1.TeamSpec{Members: []string{"alice"}},
	})

	w := httptest.NewRecorder()
	srv.handleCreateTeam(w, withClaims(httptest.NewRequest(http.MethodPost, "/teams", strings.NewReader(`{"name":"new-team","members":["bob"],"namespaces":["ns1"]}`))))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %s; want 409", w.Code, w.Body.String())
	}
	got, err := crdstore.Get[v1alpha1.Team](context.Background(), st, "new-team", "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Spec.Members) != 1 || got.Spec.Members[0] != "alice" {
		t.Errorf("members = %v, want [alice] kept", got.Spec.Members)
	}
}

func TestRunnerHandlers_CreateExistingNameAnswers409(t *testing.T) {
	srv, store, ctx := runnerAdminTestServer(t)
	seedRunnerTeam(t, store)
	seedRunner(t, store, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "win-1"},
		Spec:       v1alpha1.RunnerSpec{Team: "payments", Launcher: "inbound", RemoteFS: "/var/j"},
	})

	body := `{"name":"win-1","spec":{"team":"payments","launcher":"inbound","remoteFS":"/var/k"}}`
	req := httptest.NewRequest(http.MethodPost, "/runners", strings.NewReader(body)).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleRunners(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %s; want 409", w.Code, w.Body.String())
	}
	got, err := crdstore.Get[v1alpha1.Runner](context.Background(), store, "win-1", "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.RemoteFS != "/var/j" {
		t.Errorf("remoteFS = %q, want /var/j kept", got.Spec.RemoteFS)
	}
}
