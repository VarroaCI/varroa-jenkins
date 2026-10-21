package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUserPasswordSetEvent_Shape pins the password-set event. It names the
// account and never the password.
func TestUserPasswordSetEvent_Shape(t *testing.T) {
	e := UserPasswordSetEvent("varroa-system", "rescue")
	if e.Type != "user.password_set" {
		t.Errorf("Type = %q, want user.password_set", e.Type)
	}
	if want := "User rescue password set in varroa-system"; e.Message != want {
		t.Errorf("Message = %q, want %q", e.Message, want)
	}
	if e.Namespace != "varroa-system" {
		t.Errorf("Namespace = %q, want varroa-system", e.Namespace)
	}
	if e.Actor != "" || e.Source != "" {
		t.Errorf("builder set Actor/Source: %+v", e)
	}
}

func TestUserActivityEvent_Shape(t *testing.T) {
	cases := []struct {
		change UserChange
		want   string
	}{
		{UserCreated, "user.created"},
		{UserUpdated, "user.updated"},
		{UserDeleted, "user.deleted"},
	}
	for _, c := range cases {
		e := UserActivityEvent(c.change, "varroa-system", "rescue")
		if e.Type != c.want {
			t.Errorf("%s: Type = %q, want %q", c.change, e.Type, c.want)
		}
		wantMsg := "User rescue " + string(c.change) + " in varroa-system"
		if e.Message != wantMsg {
			t.Errorf("%s: Message = %q, want %q", c.change, e.Message, wantMsg)
		}
		if e.Namespace != "varroa-system" {
			t.Errorf("%s: Namespace = %q, want varroa-system", c.change, e.Namespace)
		}
		if e.Controller != "" {
			t.Errorf("%s: Controller = %q, want empty (routes to _global)", c.change, e.Controller)
		}
		if e.Actor != "" || e.Source != "" {
			t.Errorf("%s: Actor/Source = %q/%q, want empty (caller sets them)", c.change, e.Actor, e.Source)
		}
	}
}

func TestRESTUserWrites_PublishActivity(t *testing.T) {
	s, sink := publishTestServer(t)
	s.deps.IdentityConfig = IdentityConfig{Mode: "oidc", LocalAccounts: true}

	create := withClaims(httptest.NewRequest(http.MethodPost, "/users",
		strings.NewReader(`{"username":"rescue","password":"correct-horse","purpose":"break-glass"}`)))
	cw := httptest.NewRecorder()
	s.HandleUsers(cw, create)
	if cw.Code != http.StatusCreated {
		t.Fatalf("create: status %d, want 201: %s", cw.Code, cw.Body.String())
	}

	update := withClaims(httptest.NewRequest(http.MethodPut, "/users/rescue",
		strings.NewReader(`{"email":"rescue@example.com","displayName":"Rescue"}`)))
	uw := httptest.NewRecorder()
	s.HandleUpdateUser(uw, update, "rescue")
	if uw.Code != http.StatusOK {
		t.Fatalf("update: status %d, want 200: %s", uw.Code, uw.Body.String())
	}

	del := withClaims(httptest.NewRequest(http.MethodDelete, "/users/rescue", nil))
	dw := httptest.NewRecorder()
	s.HandleDeleteUser(dw, del)
	if dw.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d, want 204: %s", dw.Code, dw.Body.String())
	}

	want := []struct {
		change UserChange
		typ    string
	}{
		{UserCreated, "user.created"},
		{UserUpdated, "user.updated"},
		{UserDeleted, "user.deleted"},
	}
	if len(sink.events) != len(want) {
		t.Fatalf("events = %d, want %d: %+v", len(sink.events), len(want), sink.events)
	}
	for i, w := range want {
		got := sink.events[i]
		expect := UserActivityEvent(w.change, "varroa-system", "rescue")
		if got.Type != w.typ {
			t.Errorf("event %d: Type = %q, want %q", i, got.Type, w.typ)
		}
		if got.Message != expect.Message || got.Namespace != expect.Namespace {
			t.Errorf("event %d: Message/Namespace = %q/%q, want %q/%q",
				i, got.Message, got.Namespace, expect.Message, expect.Namespace)
		}
		if got.Source != "user" {
			t.Errorf("event %d: Source = %q, want user", i, got.Source)
		}
		if got.Actor != ActorFrom(publishClaims) {
			t.Errorf("event %d: Actor = %q, want %q", i, got.Actor, ActorFrom(publishClaims))
		}
	}
}

func TestRESTUserWrites_FailedWriteNoActivity(t *testing.T) {
	s, sink := publishTestServer(t)
	s.deps.IdentityConfig = IdentityConfig{Mode: "oidc", LocalAccounts: true}

	r := withClaims(httptest.NewRequest(http.MethodDelete, "/users/missing", nil))
	w := httptest.NewRecorder()
	s.HandleDeleteUser(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("delete missing: status %d, want 404", w.Code)
	}
	if len(sink.events) != 0 {
		t.Errorf("events = %+v, want none", sink.events)
	}
}
