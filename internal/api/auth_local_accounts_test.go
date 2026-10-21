package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/auth/local"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/signing"
)

// localUserStore is an in-memory user store for the local provider.
type localUserStore struct {
	users  map[string]*v1alpha1.User
	groups []*v1alpha1.Group
}

func (s *localUserStore) GetUserCRD(_ context.Context, name, _ string) (*v1alpha1.User, error) {
	if u, ok := s.users[name]; ok {
		return u, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "users"}, name)
}
func (s *localUserStore) ListGroupCRDs(_ context.Context) ([]*v1alpha1.Group, error) {
	return s.groups, nil
}
func (s *localUserStore) PatchUserStatus(_ context.Context, _, _ string, _ *v1alpha1.UserStatus) error {
	return nil
}
func (s *localUserStore) ApplyUserCRD(_ context.Context, _ *v1alpha1.User) error { return nil }

func (s *localUserStore) add(name, password string, purpose v1alpha1.UserPurpose) {
	hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	s.users[name] = &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.UserSpec{Purpose: purpose},
		Status:     v1alpha1.UserStatus{Credentials: &v1alpha1.UserCredentials{PasswordHash: string(hash)}},
	}
}

// besideOIDCServer builds a Server with local accounts beside OIDC.
func besideOIDCServer(t *testing.T) (*Server, *localUserStore, *recordingActivitySink) {
	t.Helper()
	signer, err := signing.New()
	if err != nil {
		t.Fatal(err)
	}
	store := &localUserStore{users: map[string]*v1alpha1.User{}}
	lp := local.New(signer, store, "varroa-system", "https://varroa.example.com", "varroa", time.Hour, "")
	lp.SetBesideOIDC(true)
	sink := &recordingActivitySink{}
	deps := &Dependencies{
		Auth:              auth.NewMultiProvider(&mockOIDCProvider{}, lp, "https://varroa.example.com"),
		Local:             lp,
		ActivityPublisher: sink,
		Logger:            testLoggerOIDC(t),
		IdentityConfig:    IdentityConfig{Mode: "oidc", LocalAccounts: true},
	}
	return NewServer(deps), store, sink
}

func postLogin(s *Server, user, pass string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	w := httptest.NewRecorder()
	s.HandleLogin(w, httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(body)))
	return w
}

func TestHandleAuthConfig_LocalAccounts(t *testing.T) {
	s, _, _ := besideOIDCServer(t)
	w := httptest.NewRecorder()
	s.HandleAuthConfig(w, httptest.NewRequest(http.MethodGet, "/auth-config", nil))
	var got authConfigResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Mode != "oidc" || !got.LocalAccounts {
		t.Errorf("auth-config = %+v, want {oidc true}", got)
	}

	plain := NewServer(&Dependencies{Auth: &mockOIDCProvider{}})
	w = httptest.NewRecorder()
	plain.HandleAuthConfig(w, httptest.NewRequest(http.MethodGet, "/auth-config", nil))
	if !strings.Contains(w.Body.String(), `"localAccounts":false`) {
		t.Errorf("OIDC-only body = %s, want localAccounts false", w.Body.String())
	}
}

func TestHandleLogin_BesideOIDC_PublishesAuditEvent(t *testing.T) {
	for _, tc := range []struct {
		purpose  v1alpha1.UserPurpose
		password string
		severity string
	}{
		{v1alpha1.UserPurposeBreakGlass, "correct-horse", "warning"},
		{v1alpha1.UserPurposeService, "ci-secret-1", "info"},
	} {
		s, store, sink := besideOIDCServer(t)
		store.add("acct", tc.password, tc.purpose)
		w := postLogin(s, "acct", tc.password)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", tc.purpose, w.Code, w.Body.String())
		}
		if len(sink.events) != 1 {
			t.Fatalf("%s: events = %+v, want one", tc.purpose, sink.events)
		}
		e := sink.events[0]
		if e.Type != "auth.local.login" || e.Severity != tc.severity || e.Actor != "local:acct" || e.Reason != string(tc.purpose) {
			t.Errorf("%s: event = %+v", tc.purpose, e)
		}
	}
}

func TestHandleLogin_BesideOIDC_FailedAttempt(t *testing.T) {
	s, store, sink := besideOIDCServer(t)
	store.add("rescue", "correct-horse", v1alpha1.UserPurposeBreakGlass)
	store.add("nopurpose", "correct-horse", "")

	if w := postLogin(s, "rescue", "wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", w.Code)
	}
	if len(sink.events) != 1 || sink.events[0].Type != "auth.local.login_failed" || sink.events[0].Severity != "warning" {
		t.Fatalf("events = %+v, want one auth.local.login_failed warning", sink.events)
	}

	// An unknown name or an account without a purpose publishes nothing,
	// so the event never confirms that a name exists.
	postLogin(s, "ghost", "wrong")
	if w := postLogin(s, "nopurpose", "correct-horse"); w.Code != http.StatusUnauthorized {
		t.Fatalf("no purpose: status %d, want 401", w.Code)
	}
	if len(sink.events) != 1 {
		t.Errorf("events = %+v, want only the first event", sink.events)
	}
}

func TestHandleLogin_EmptyFields(t *testing.T) {
	s, store, _ := besideOIDCServer(t)
	store.add("acct", "correct-horse", v1alpha1.UserPurposeBreakGlass)
	for _, body := range []string{
		`{"username":"","password":"x"}`,
		`{"username":"a","password":""}`,
		`{}`,
	} {
		w := httptest.NewRecorder()
		s.HandleLogin(w, httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status %d, want 400", body, w.Code)
		}
		if !strings.Contains(w.Body.String(), "username and password required") {
			t.Errorf("body %s: response = %s", body, w.Body.String())
		}
	}
}

func TestHandleLogin_LocalOnly_NoAuditEvent(t *testing.T) {
	signer, err := signing.New()
	if err != nil {
		t.Fatal(err)
	}
	store := &localUserStore{users: map[string]*v1alpha1.User{}}
	store.add("alice", "correct-horse", "")
	lp := local.New(signer, store, "varroa-system", "https://varroa.example.com", "varroa", time.Hour, "")
	sink := &recordingActivitySink{}
	s := NewServer(&Dependencies{
		Auth: lp, Local: lp, ActivityPublisher: sink, Logger: testLoggerOIDC(t),
		IdentityConfig: IdentityConfig{Mode: "local"},
	})
	if w := postLogin(s, "alice", "correct-horse"); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if len(sink.events) != 0 {
		t.Errorf("events = %+v, want none in local-only mode", sink.events)
	}
}

func TestMe_LocalSessionReportsLocal(t *testing.T) {
	s, _, _ := besideOIDCServer(t)
	r := httptest.NewRequest(http.MethodGet, "/me", nil)
	r = r.WithContext(contextWithClaims(r.Context(), &auth.Claims{Subject: "local:rescue", PreferredUsername: "local:rescue"}))
	w := httptest.NewRecorder()
	s.HandleMe(w, r)
	var got meResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.AuthMode != "local" {
		t.Errorf("authMode = %q, want local", got.AuthMode)
	}
}

// endSessionOIDC is an OIDC provider with an end-session endpoint.
type endSessionOIDC struct{ mockOIDCProvider }

func (endSessionOIDC) EndSessionEndpoint() string { return "https://idp.example.com/logout" }

func TestLogout_LocalSessionSkipsEndSession(t *testing.T) {
	deps := &Dependencies{
		Auth:          auth.NewMultiProvider(&endSessionOIDC{}, &mockOIDCProvider{}, "https://varroa.example.com"),
		Logger:        testLoggerOIDC(t),
		DashboardURL:  "https://varroa.example.com",
		SecureCookies: true,
	}
	s := NewServer(deps)

	call := func(claims *auth.Claims) string {
		r := httptest.NewRequest(http.MethodPost, "/logout", nil)
		r.AddCookie(&http.Cookie{Name: "varroa_token", Value: "tok"})
		r = r.WithContext(contextWithClaims(r.Context(), claims))
		w := httptest.NewRecorder()
		s.HandleLogout(w, r)
		return w.Body.String()
	}
	if body := call(&auth.Claims{Subject: "idp-user"}); !strings.Contains(body, "idp.example.com") {
		t.Errorf("IdP session body = %s, want an end-session redirect", body)
	}
	if body := call(&auth.Claims{Subject: "local:rescue"}); strings.Contains(body, "idp.example.com") {
		t.Errorf("local session body = %s, want no end-session redirect", body)
	}
}

// localPrefixOIDC answers the callback with an IdP subject that uses the
// reserved local prefix.
type localPrefixOIDC struct{ mockOIDCProvider }

func (localPrefixOIDC) VerifyToken(_ context.Context, _ string) (*auth.Claims, error) {
	return &auth.Claims{Subject: "local:rescue"}, nil
}

func TestHandleOIDCCallback_RefusesLocalPrefix(t *testing.T) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	sink := &recordingActivitySink{}
	s := NewServer(&Dependencies{
		OIDCStateSecret:   secret,
		Logger:            testLoggerOIDC(t),
		Auth:              &localPrefixOIDC{},
		ActivityPublisher: sink,
		SecureCookies:     true,
	})
	nonce, _ := newStateNonce()
	payload, err := encodeStateCookie(secret, nonce, "/", time.Now().Unix()+300)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/callback?code=valid_code&state="+nonce, nil)
	r.AddCookie(&http.Cookie{Name: stateCookieName, Value: payload})
	s.HandleOIDCCallback(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "varroa_token" && c.Value != "" {
			t.Error("callback must not set a session cookie for a refused subject")
		}
	}
	if len(sink.events) != 0 {
		t.Errorf("events = %+v, want none", sink.events)
	}
}

func TestCreateUser_BesideOIDC_RequiresPurpose(t *testing.T) {
	s, _ := publishTestServer(t)
	s.deps.IdentityConfig = IdentityConfig{Mode: "oidc", LocalAccounts: true}

	post := func(body string) int {
		r := withClaims(httptest.NewRequest(http.MethodPost, "/users", strings.NewReader(body)))
		w := httptest.NewRecorder()
		s.HandleUsers(w, r)
		return w.Code
	}
	if code := post(`{"username":"rescue","password":"correct-horse"}`); code != http.StatusBadRequest {
		t.Errorf("no purpose: status %d, want 400", code)
	}
	if code := post(`{"username":"rescue","password":"correct-horse","purpose":"admin"}`); code != http.StatusBadRequest {
		t.Errorf("bad purpose: status %d, want 400", code)
	}
	if code := post(`{"username":"rescue","password":"correct-horse","purpose":"break-glass"}`); code != http.StatusCreated {
		t.Errorf("break-glass: status %d, want 201", code)
	}
}

func TestGroupWrite_BesideOIDC_Allowed(t *testing.T) {
	s, _ := publishTestServer(t)
	s.deps.IdentityConfig = IdentityConfig{Mode: "oidc", LocalAccounts: true}
	r := withClaims(httptest.NewRequest(http.MethodPost, "/groups", strings.NewReader(`{"name":"platform-admins","members":["rescue"]}`)))
	w := httptest.NewRecorder()
	s.handleCreateGroup(w, r)
	if w.Code != http.StatusCreated {
		t.Errorf("status %d, want 201: %s", w.Code, w.Body.String())
	}

	s.deps.IdentityConfig = IdentityConfig{Mode: "oidc"}
	w = httptest.NewRecorder()
	s.handleCreateGroup(w, withClaims(httptest.NewRequest(http.MethodPost, "/groups", strings.NewReader(`{"name":"x"}`))))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("OIDC only: status %d, want 405", w.Code)
	}
}

func TestAdminKeyOwner_BesideOIDC(t *testing.T) {
	s, _, _ := besideOIDCServer(t)
	if got := s.adminKeyOwner("rescue"); got != "local:rescue" {
		t.Errorf("adminKeyOwner(rescue) = %q, want local:rescue", got)
	}
	if got := s.adminKeyOwner("oidc-0123"); got != "oidc-0123" {
		t.Errorf("adminKeyOwner(oidc-0123) = %q, want unchanged", got)
	}
	plain := NewServer(&Dependencies{IdentityConfig: IdentityConfig{Mode: "local"}})
	if got := plain.adminKeyOwner("alice"); got != "alice" {
		t.Errorf("local mode adminKeyOwner(alice) = %q, want alice", got)
	}
}

func TestAdminSetPassword_BesideOIDC_RefusesIdPUser(t *testing.T) {
	s, _, sink := besideOIDCServer(t)
	s.deps.Authorizer = adminAuthorizer()
	r := withClaims(httptest.NewRequest(http.MethodPut, "/users/oidc-0123/password", strings.NewReader(`{"newPassword":"long-enough-1"}`)))
	w := httptest.NewRecorder()
	s.HandleAdminSetPassword(w, r, "oidc-0123")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", w.Code)
	}
	if len(sink.events) != 0 {
		t.Errorf("events = %+v, want none on a refused set", sink.events)
	}
}

// TestAdminSetPassword_PublishesPasswordSet pins that an admin password set
// publishes one user.password_set event and never carries the password.
func TestAdminSetPassword_PublishesPasswordSet(t *testing.T) {
	s, store, sink := besideOIDCServer(t)
	s.deps.Authorizer = adminAuthorizer()
	s.deps.OperatorNamespace = "varroa-system"
	store.add("rescue", "old-pass-123", v1alpha1.UserPurposeBreakGlass)

	r := withClaims(httptest.NewRequest(http.MethodPut, "/users/rescue/password", strings.NewReader(`{"newPassword":"long-enough-1"}`)))
	w := httptest.NewRecorder()
	s.HandleAdminSetPassword(w, r, "rescue")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(sink.events) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(sink.events), sink.events)
	}
	e := sink.events[0]
	if e.Type != "user.password_set" || e.Source != "user" {
		t.Errorf("event = %+v, want user.password_set from user", e)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "long-enough-1") {
		t.Errorf("event carries the password: %s", raw)
	}
}

// TestChangePassword_PublishesPasswordSet pins that a self-service password
// change publishes one user.password_set event naming the caller.
func TestChangePassword_PublishesPasswordSet(t *testing.T) {
	s, store, sink := besideOIDCServer(t)
	s.deps.OperatorNamespace = "varroa-system"
	store.add("rescue", "old-pass-123", v1alpha1.UserPurposeBreakGlass)

	claims := &auth.Claims{Subject: "local:rescue"}
	r := httptest.NewRequest(http.MethodPut, "/me/password", strings.NewReader(`{"oldPassword":"old-pass-123","newPassword":"long-enough-1"}`))
	r = r.WithContext(contextWithClaims(r.Context(), claims))
	w := httptest.NewRecorder()
	s.HandleChangePassword(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(sink.events) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(sink.events), sink.events)
	}
	e := sink.events[0]
	if e.Type != "user.password_set" || e.Source != "user" || e.Actor != ActorFrom(claims) {
		t.Errorf("event = %+v, want user.password_set from user by %q", e, ActorFrom(claims))
	}
}

func TestUpdateUser_BesideOIDC_KeepsPurposeWhenOmitted(t *testing.T) {
	s, _ := publishTestServer(t)
	s.deps.IdentityConfig = IdentityConfig{Mode: "oidc", LocalAccounts: true}
	create := withClaims(httptest.NewRequest(http.MethodPost, "/users",
		strings.NewReader(`{"username":"rescue","password":"correct-horse","purpose":"break-glass"}`)))
	w := httptest.NewRecorder()
	s.HandleUsers(w, create)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status %d: %s", w.Code, w.Body.String())
	}

	upd := withClaims(httptest.NewRequest(http.MethodPut, "/users/rescue", strings.NewReader(`{"email":"r@example.com"}`)))
	w = httptest.NewRecorder()
	s.HandleUpdateUser(w, upd, "rescue")
	if w.Code != http.StatusOK {
		t.Fatalf("update: status %d: %s", w.Code, w.Body.String())
	}
	u, err := crdstore.Get[v1alpha1.User](context.Background(), s.deps.Store, "rescue", s.deps.OperatorNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if u.Spec.Purpose != v1alpha1.UserPurposeBreakGlass || u.Spec.Email != "r@example.com" {
		t.Errorf("user spec = %+v, want purpose kept and email set", u.Spec)
	}
}
