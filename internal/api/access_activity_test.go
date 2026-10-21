package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// TestAccessActivityEvent_Shape pins the type and the message of every
// access event. The message holds no actor.
func TestAccessActivityEvent_Shape(t *testing.T) {
	kinds := []AccessKind{AccessGroup, AccessVarroaRole, AccessVarroaRoleBinding, AccessJenkinsRole, AccessJenkinsRoleBinding}
	changes := []AccessChange{AccessCreated, AccessUpdated, AccessDeleted}
	for _, k := range kinds {
		for _, c := range changes {
			e := AccessActivityEvent(k, c, "n")
			if want := string(k) + "." + string(c); e.Type != want {
				t.Errorf("%s/%s: Type = %q, want %q", k, c, e.Type, want)
			}
			if want := accessKindLabel[k] + " n " + string(c); e.Message != want {
				t.Errorf("%s/%s: Message = %q, want %q", k, c, e.Message, want)
			}
			if strings.Contains(e.Message, " by ") {
				t.Errorf("%s/%s: Message %q carries an actor", k, c, e.Message)
			}
			if e.Actor != "" || e.Source != "" || e.Cluster != "" {
				t.Errorf("%s/%s: builder set Actor/Source/Cluster: %+v", k, c, e)
			}
		}
	}
}

// TestRESTGroupWrites_PublishActivity pins that a REST group create and
// delete each publish one event built by AccessActivityEvent.
func TestRESTGroupWrites_PublishActivity(t *testing.T) {
	srv, sink := publishTestServer(t)
	srv.deps.IdentityConfig = IdentityConfig{Mode: "oidc", LocalAccounts: true}

	create := withClaims(httptest.NewRequest(http.MethodPost, "/groups", strings.NewReader(`{"name":"g1"}`)))
	w := httptest.NewRecorder()
	srv.HandleGroups(w, create)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status %d: %s", w.Code, w.Body.String())
	}

	del := withClaims(httptest.NewRequest(http.MethodDelete, "/groups/g1", nil))
	w = httptest.NewRecorder()
	srv.HandleGroupDispatch(w, del)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d: %s", w.Code, w.Body.String())
	}

	if len(sink.events) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(sink.events), sink.events)
	}
	wantActor := ActorFrom(publishClaims)
	for i, c := range []AccessChange{AccessCreated, AccessDeleted} {
		want := AccessActivityEvent(AccessGroup, c, "g1")
		got := sink.events[i]
		if got.Type != want.Type || got.Message != want.Message {
			t.Errorf("event %d = %+v, want type/message of %+v", i, got, want)
		}
		if got.Source != "user" || got.Actor != wantActor || got.Severity != "info" {
			t.Errorf("event %d Source/Actor/Severity = %q/%q/%q, want user/%q/info", i, got.Source, got.Actor, got.Severity, wantActor)
		}
	}
}

// TestRESTGroupDelete_RefusedWriteNoActivity pins that a refused delete
// publishes no event. The fake store deletes a missing name without error,
// so the refusal comes from the authorizer.
func TestRESTGroupDelete_RefusedWriteNoActivity(t *testing.T) {
	srv, sink := publishTestServer(t)
	srv.deps.IdentityConfig = IdentityConfig{Mode: "oidc", LocalAccounts: true}
	srv.deps.Authorizer = nil

	del := withClaims(httptest.NewRequest(http.MethodDelete, "/groups/g1", nil))
	w := httptest.NewRecorder()
	srv.HandleGroupDispatch(w, del)
	if w.Code != http.StatusForbidden {
		t.Fatalf("delete without an authorizer: status %d, want 403", w.Code)
	}
	if len(sink.events) != 0 {
		t.Errorf("events = %+v, want none", sink.events)
	}
}

// TestRESTVarroaRoleWrites_MessageMatchesBuilder pins that the REST
// VarroaRole writes publish the shared message and the user source.
func TestRESTVarroaRoleWrites_MessageMatchesBuilder(t *testing.T) {
	srv, sink := publishTestServer(t)
	create := withClaims(httptest.NewRequest(http.MethodPost, "/roles", strings.NewReader(`{"metadata":{"name":"role-a"}}`)))
	srv.handleVarroaRoles(httptest.NewRecorder(), create)
	update := withClaims(httptest.NewRequest(http.MethodPut, "/roles/role-a", strings.NewReader(`{"metadata":{"name":"role-a"}}`)))
	srv.handleVarroaRoleDispatch(httptest.NewRecorder(), update)
	del := withClaims(httptest.NewRequest(http.MethodDelete, "/roles/role-a", nil))
	srv.handleVarroaRoleDispatch(httptest.NewRecorder(), del)

	assertAccessEvents(t, sink.events, AccessVarroaRole, "role-a")
}

// TestRESTVarroaRoleBindingWrites_MessageMatchesBuilder is the binding
// counterpart of TestRESTVarroaRoleWrites_MessageMatchesBuilder.
func TestRESTVarroaRoleBindingWrites_MessageMatchesBuilder(t *testing.T) {
	srv, sink := publishTestServer(t)
	create := withClaims(httptest.NewRequest(http.MethodPost, "/rolebindings", strings.NewReader(`{"metadata":{"name":"binding-a"}}`)))
	srv.handleVarroaRoleBindings(httptest.NewRecorder(), create)
	update := withClaims(httptest.NewRequest(http.MethodPut, "/rolebindings/binding-a", strings.NewReader(`{"metadata":{"name":"binding-a"}}`)))
	srv.handleVarroaRoleBindingDispatch(httptest.NewRecorder(), update)
	del := withClaims(httptest.NewRequest(http.MethodDelete, "/rolebindings/binding-a", nil))
	srv.handleVarroaRoleBindingDispatch(httptest.NewRecorder(), del)

	assertAccessEvents(t, sink.events, AccessVarroaRoleBinding, "binding-a")
}

// assertAccessEvents checks create, update, and delete events for one
// access object against the shared builder.
func assertAccessEvents(t *testing.T, events []activity.Event, kind AccessKind, name string) {
	t.Helper()
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3: %+v", len(events), events)
	}
	for i, c := range []AccessChange{AccessCreated, AccessUpdated, AccessDeleted} {
		want := AccessActivityEvent(kind, c, name)
		got := events[i]
		if got.Type != want.Type || got.Message != want.Message {
			t.Errorf("event %d = %+v, want type/message of %+v", i, got, want)
		}
		if got.Source != "user" || got.Severity != "info" {
			t.Errorf("event %d Source/Severity = %q/%q, want user/info", i, got.Source, got.Severity)
		}
	}
}
