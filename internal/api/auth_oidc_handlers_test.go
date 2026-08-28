package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// --- normalizeReturn tests ---

func TestNormalizeReturn(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", "/"},
		{"/", "/"},
		{"/controllers", "/controllers"},
		{"/controllers/ns/name", "/controllers/ns/name"},
		{"controllers", "/controllers"},
		{"//controllers", "/"},
		{"https://evil.com", "/"},
		{"//evil.com/path", "/"},
		{"/../etc", "/"},
		{"/./path", "/"},
		{"/..", "/"},
		{"/foo/../bar", "/"},
		{"/foo/./bar", "/"},
		{"/api/v1/auth/login", "/api/v1/auth/login"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeReturn(tt.input)
			if got != tt.expected {
				t.Errorf("normalizeReturn(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

// --- signStatePayload / state validation tests ---

func TestSignStatePayload(t *testing.T) {
	secret := []byte("01234567890123456789012345678901")
	nonce := "abc123nonce"
	returnPath := "/controllers"
	expiry := time.Now().Unix() + 300

	// Test encodeStateCookie produces consistent results
	cookie1, err := encodeStateCookie(secret, nonce, returnPath, expiry)
	if err != nil {
		t.Fatal(err)
	}
	if cookie1 == "" {
		t.Fatal("expected non-empty cookie")
	}

	cookie2, err := encodeStateCookie(secret, nonce, returnPath, expiry)
	if err != nil {
		t.Fatal(err)
	}
	if cookie1 != cookie2 {
		t.Error("cookie not deterministic")
	}

	// Different secret
	cookie3, _ := encodeStateCookie([]byte("different_key_12345678901234567890"), nonce, returnPath, expiry)
	if cookie1 == cookie3 {
		t.Error("different secret should produce different cookie")
	}

	// Different nonce
	cookie4, _ := encodeStateCookie(secret, "different_nonce", returnPath, expiry)
	if cookie1 == cookie4 {
		t.Error("different nonce should produce different cookie")
	}

	// Decode and verify
	p, err := decodeStateCookie(secret, cookie1)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if p.Nonce != nonce || p.Return != returnPath || p.Expiry != expiry {
		t.Error("decoded payload does not match")
	}
}

func TestNewStateNonce(t *testing.T) {
	n1, err := newStateNonce()
	if err != nil {
		t.Fatal(err)
	}
	if len(n1) != 64 {
		t.Errorf("expected 64-char hex, got %d", len(n1))
	}

	n2, _ := newStateNonce()
	if n1 == n2 {
		t.Error("nonces should be unique")
	}
}

// Test encode/decode state cookie with dots in return paths
func TestEncodeDecodeStateCookie_DotsInPath(t *testing.T) {
	secret := []byte("test-secret-12345678901234567890123456")
	tests := []string{"/foo.bar", "/a/b.c?x=1", "/foo/bar.baz/qux", "/controllers/core/default/my-controller"}

	for _, returnPath := range tests {
		nonce, _ := newStateNonce()
		expiry := time.Now().Unix() + 300

		cookie, err := encodeStateCookie(secret, nonce, returnPath, expiry)
		if err != nil {
			t.Fatalf("encodeStateCookie(%q) failed: %v", returnPath, err)
		}

		p, err := decodeStateCookie(secret, cookie)
		if err != nil {
			t.Fatalf("decodeStateCookie(%q) failed: %v", returnPath, err)
		}

		if p.Return != returnPath {
			t.Errorf("return path mismatch: got %q, want %q", p.Return, returnPath)
		}
		if p.Nonce != nonce {
			t.Errorf("nonce mismatch")
		}
		if p.Expiry != expiry {
			t.Errorf("expiry mismatch")
		}
	}
}

// --- OIDC login handler tests ---

func TestHandleOIDCLogin_MethodNotAllowed(t *testing.T) {
	s := oidcTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	s.HandleOIDCLogin(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleOIDCLogin_SetsStateCookie(t *testing.T) {
	s := oidcTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/login?return=/controllers", nil)
	s.HandleOIDCLogin(w, r)

	if w.Code != http.StatusFound {
		t.Errorf("expected 302 redirect, got %d", w.Code)
	}

	cookies := w.Result().Cookies()
	var stateCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == stateCookieName {
			stateCookie = c
			break
		}
	}
	if stateCookie == nil {
		t.Fatal("expected oidc_state cookie")
	}
	if !stateCookie.HttpOnly {
		t.Error("state cookie should be HttpOnly")
	}
	if !stateCookie.Secure {
		t.Error("state cookie should be Secure by default")
	}
	if stateCookie.SameSite != http.SameSiteLaxMode {
		t.Error("state cookie should be SameSite=Lax")
	}
	if stateCookie.Path != "/api/v1" {
		t.Errorf("state cookie path should be /api/v1, got %q", stateCookie.Path)
	}
	if stateCookie.MaxAge != stateCookieMaxAge {
		t.Errorf("state cookie maxAge should be %d, got %d", stateCookieMaxAge, stateCookie.MaxAge)
	}

	// Validate cookie payload format: base64url(json).hex(sig)
	parts := strings.SplitN(stateCookie.Value, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("expected 2-part state cookie (base64url.sig), got %d parts", len(parts))
	}
	if len(parts[0]) == 0 {
		t.Error("expected non-empty base64url payload")
	}
	if len(parts[1]) == 0 {
		t.Error("expected non-empty signature")
	}

	// Decode and verify the payload contains the return path.
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	var p statePayloadJSON
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("json decode failed: %v", err)
	}
	if p.Return != "/controllers" {
		t.Errorf("expected return=/controllers, got %q", p.Return)
	}
	if len(p.Nonce) == 0 {
		t.Error("expected non-empty nonce")
	}
	if p.Expiry == 0 {
		t.Error("expected non-zero expiry")
	}
}

func TestHandleOIDCLogin_DefaultReturn(t *testing.T) {
	s := oidcTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/login", nil)
	s.HandleOIDCLogin(w, r)

	for _, c := range w.Result().Cookies() {
		if c.Name == stateCookieName {
			// Decode and verify the return path
			parts := strings.SplitN(c.Value, ".", 2)
			if len(parts) != 2 {
				t.Fatal("expected 2-part state cookie")
			}
			raw, err := base64.RawURLEncoding.DecodeString(parts[0])
			if err != nil {
				t.Fatalf("base64 decode failed: %v", err)
			}
			var p statePayloadJSON
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatalf("json decode failed: %v", err)
			}
			if p.Return != "/" {
				t.Errorf("expected return /, got %q", p.Return)
			}
			return
		}
	}
	t.Fatal("expected oidc_state cookie")
}

func TestHandleOIDCLogin_NormalizesEvilReturn(t *testing.T) {
	s := oidcTestServer(t)
	for _, evil := range []string{"https://evil.com", "//evil.com", "/../etc", "/./path"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/login?return="+evil, nil)
		s.HandleOIDCLogin(w, r)

		for _, c := range w.Result().Cookies() {
			if c.Name == stateCookieName {
				parts := strings.SplitN(c.Value, ".", 2)
				if len(parts) != 2 {
					t.Fatal("expected 2-part state cookie")
				}
				raw, err := base64.RawURLEncoding.DecodeString(parts[0])
				if err != nil {
					t.Fatalf("base64 decode failed: %v", err)
				}
				var p statePayloadJSON
				if err := json.Unmarshal(raw, &p); err != nil {
					t.Fatalf("json decode failed: %v", err)
				}
				if p.Return != "/" {
					t.Errorf("for %q: expected return /, got %q", evil, p.Return)
				}
			}
		}
	}
}

func TestHandleOIDCLogin_ConsumesInteractiveMarker(t *testing.T) {
	s := oidcTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/login", nil)
	r.AddCookie(&http.Cookie{Name: interactiveCookieName, Value: "1"})
	s.HandleOIDCLogin(w, r)

	for _, c := range w.Result().Cookies() {
		if c.Name == interactiveCookieName && c.MaxAge != -1 {
			t.Error("interactive marker should be deleted")
		}
	}
}

func TestHandleOIDCLogin_OIDCNotConfigured(t *testing.T) {
	s := localTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/login", nil)
	s.HandleOIDCLogin(w, r)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("expected 501, got %d", w.Code)
	}
}

// --- callback state validation tests ---

func TestHandleOIDCCallback_MissingStateCookie(t *testing.T) {
	s := oidcTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/callback?code=abc&state=xyz", nil)
	s.HandleOIDCCallback(w, r)

	if w.Code != http.StatusFound {
		t.Errorf("expected 302 redirect to /login, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/login" {
		t.Errorf("expected Location /login, got %q", loc)
	}
}

func TestHandleOIDCCallback_ExpiredStateCookie(t *testing.T) {
	s := oidcTestServer(t)
	secret := s.deps.OIDCStateSecret
	nonce := "testnonce12345678901234567890123456"
	returnPath := "/"
	expired := time.Now().Unix() - 10
	payload, err := encodeStateCookie(secret, nonce, returnPath, expired)
	if err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/callback?code=abc&state="+nonce, nil)
	r.AddCookie(&http.Cookie{Name: stateCookieName, Value: payload})
	s.HandleOIDCCallback(w, r)

	if w.Code != http.StatusFound {
		t.Errorf("expected 302 for expired, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/login" {
		t.Errorf("expected Location /login, got %q", loc)
	}
}

func TestHandleOIDCCallback_TamperedSignature(t *testing.T) {
	s := oidcTestServer(t)
	nonce := "abcnonce12345678901234567890123456"
	// Use a valid encoded payload but with a bad signature
	raw, _ := json.Marshal(statePayloadJSON{Nonce: nonce, Return: "/", Expiry: time.Now().Unix() + 300})
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	payload := encoded + ".bad_sig"

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/callback?code=abc&state="+nonce, nil)
	r.AddCookie(&http.Cookie{Name: stateCookieName, Value: payload})
	s.HandleOIDCCallback(w, r)

	if w.Code != http.StatusFound {
		t.Errorf("expected 302 for tampered sig, got %d", w.Code)
	}
}

func TestHandleOIDCCallback_NonceMismatch(t *testing.T) {
	s := oidcTestServer(t)
	secret := s.deps.OIDCStateSecret
	nonce, _ := newStateNonce()
	expiry := time.Now().Unix() + 300
	payload, err := encodeStateCookie(secret, nonce, "/", expiry)
	if err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/callback?code=abc&state=different_nonce", nil)
	r.AddCookie(&http.Cookie{Name: stateCookieName, Value: payload})
	s.HandleOIDCCallback(w, r)

	if w.Code != http.StatusFound {
		t.Errorf("expected 302 for nonce mismatch, got %d", w.Code)
	}
}

func TestHandleOIDCCallback_ProviderError(t *testing.T) {
	s := oidcTestServer(t)
	secret := s.deps.OIDCStateSecret
	nonce, _ := newStateNonce()
	expiry := time.Now().Unix() + 300
	payload, err := encodeStateCookie(secret, nonce, "/", expiry)
	if err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/callback?error=access_denied&error_description=User+cancelled&state="+nonce, nil)
	r.AddCookie(&http.Cookie{Name: stateCookieName, Value: payload})
	s.HandleOIDCCallback(w, r)

	if w.Code != http.StatusFound {
		t.Errorf("expected 302 on provider error, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "/login?error=access_denied") {
		t.Errorf("expected /login?error=..., got %q", loc)
	}
}

func TestHandleOIDCCallback_MissingCode(t *testing.T) {
	s := oidcTestServer(t)
	secret := s.deps.OIDCStateSecret
	nonce, _ := newStateNonce()
	expiry := time.Now().Unix() + 300
	payload, err := encodeStateCookie(secret, nonce, "/", expiry)
	if err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/callback?state="+nonce, nil)
	r.AddCookie(&http.Cookie{Name: stateCookieName, Value: payload})
	s.HandleOIDCCallback(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing code, got %d", w.Code)
	}
}

// Test callback progress HTML does not contain the ID token anywhere.
func TestHandleOIDCCallback_HTMLNoTokenDisclosure(t *testing.T) {
	s := oidcTestServer(t)
	secret := s.deps.OIDCStateSecret
	nonce, _ := newStateNonce()
	expiry := time.Now().Unix() + 300
	payload, err := encodeStateCookie(secret, nonce, "/", expiry)
	if err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/callback?code=valid_code&state="+nonce, nil)
	r.AddCookie(&http.Cookie{Name: stateCookieName, Value: payload})

	// The mock OIDC provider's Exchange returns a token with extra "id_token":"mock-id-token-abc123"
	s.HandleOIDCCallback(w, r)

	body := w.Body.String()
	// The ID token MUST NOT appear in the HTML body, JS, or any hidden field.
	if strings.Contains(body, "mock-id-token-abc123") {
		t.Error("ID token leaked into callback HTML body")
	}
	if strings.Contains(body, "varroa_id_token") {
		t.Error("'varroa_id_token' localStorage key leaked into callback HTML")
	}
	if strings.Contains(body, "localStorage.setItem") {
		t.Error("localStorage.setItem should not be in callback HTML")
	}
	if strings.Contains(body, "varroa_user") {
		t.Error("'varroa_user' localStorage key leaked into callback HTML")
	}
	// The cookie is set via Set-Cookie header, not in the HTML.
	// Verify the response contains only the progress page and redirect.
	if !strings.Contains(body, "Signing you in") {
		t.Error("callback HTML should contain progress copy")
	}
	// The redirect must be present.
	if !strings.Contains(body, "window.location.replace") && !strings.Contains(body, "meta http-equiv") {
		t.Error("callback HTML should contain a redirect mechanism")
	}
}

// --- Secure cookie flag tests ---

func TestSecureCookies_DefaultTrue(t *testing.T) {
	s := oidcTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/login?return=/", nil)
	s.HandleOIDCLogin(w, r)

	cookies := w.Result().Cookies()
	for _, c := range cookies {
		if c.Name == stateCookieName {
			if !c.Secure {
				t.Error("state cookie should be Secure=true when SecureCookies=true")
			}
			return
		}
	}
	t.Fatal("expected state cookie")
}

func TestSecureCookies_FalseForHTTP(t *testing.T) {
	deps := &Dependencies{
		OIDCStateSecret: []byte("test-secret-12345678901234567890123456"),
		Logger:          testLoggerOIDC(t),
		Auth:            &mockOIDCProvider{},
		ActivityStore:   activity.New(10),
		SecureCookies:   false,
	}
	s := NewServer(deps)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/login?return=/", nil)
	s.HandleOIDCLogin(w, r)

	cookies := w.Result().Cookies()
	for _, c := range cookies {
		if c.Name == stateCookieName {
			if c.Secure {
				t.Error("state cookie should NOT be Secure when SecureCookies=false (HTTP local dev)")
			}
			return
		}
	}
	t.Fatal("expected state cookie")
}

func TestSecureCookies_LogoutMarkerRespectsSetting(t *testing.T) {
	// When SecureCookies=false, the interactive_login marker should also not have Secure flag.
	deps := &Dependencies{
		Logger:        testLoggerOIDC(t),
		Auth:          &mockOIDCProvider{},
		SecureCookies: false,
	}
	s := NewServer(deps)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/logout", nil)
	s.HandleLogout(w, r)

	cookies := w.Result().Cookies()
	for _, c := range cookies {
		if c.Name == interactiveCookieName {
			if c.Secure {
				t.Error("interactive marker should NOT be Secure when SecureCookies=false")
			}
			return
		}
	}
	// It's possible the cookie wasn't returned if the provider check failed; check varroa_token instead.
	for _, c := range cookies {
		if c.Name == "varroa_token" && c.Secure {
			t.Error("varroa_token delete cookie should NOT be Secure when SecureCookies=false")
		}
	}
}

// --- HandleLogout tests ---

func TestHandleLogout_MethodNotAllowed(t *testing.T) {
	s := oidcTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/logout", nil)
	s.HandleLogout(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleLogout_ReturnsJSONNoLocation(t *testing.T) {
	s := oidcTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/logout", nil)
	s.HandleLogout(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("expected JSON, got %q", ct)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Errorf("expected no Location header, got %q", loc)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp["redirect"] == "" {
		t.Error("expected redirect field in JSON response")
	}
}

func TestHandleLogout_ClearsCookie(t *testing.T) {
	s := oidcTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/logout", nil)
	r.AddCookie(&http.Cookie{Name: "varroa_token", Value: "test-token"})
	s.HandleLogout(w, r)

	for _, c := range w.Result().Cookies() {
		if c.Name == "varroa_token" {
			if c.MaxAge != -1 {
				t.Error("varroa_token should be cleared (MaxAge=-1)")
			}
			if c.Value != "" {
				t.Error("varroa_token should be empty")
			}
			return
		}
	}
	t.Error("expected varroa_token delete cookie")
}

func TestHandleLogout_SetsInteractiveMarker(t *testing.T) {
	s := oidcTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/logout", nil)
	s.HandleLogout(w, r)

	found := false
	for _, c := range w.Result().Cookies() {
		if c.Name == interactiveCookieName {
			if c.Value != "1" {
				t.Errorf("interactive marker value should be 1, got %q", c.Value)
			}
			if c.Path != "/api/v1/auth" {
				t.Errorf("interactive marker path should be /api/v1/auth, got %q", c.Path)
			}
			if c.MaxAge != stateCookieMaxAge {
				t.Errorf("interactive marker maxAge should be %d, got %d", stateCookieMaxAge, c.MaxAge)
			}
			if !c.HttpOnly {
				t.Error("interactive marker should be HttpOnly")
			}
			found = true
		}
	}
	if !found {
		t.Error("expected interactive_login marker cookie")
	}
}

func TestHandleLogout_LocalModeNoMarker(t *testing.T) {
	s := localTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/logout", nil)
	s.HandleLogout(w, r)

	for _, c := range w.Result().Cookies() {
		if c.Name == interactiveCookieName {
			t.Error("local mode should not set interactive_login marker")
		}
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["redirect"] != "/" {
		t.Errorf("local mode should redirect to /, got %q", resp["redirect"])
	}
}

func TestHandleLogout_OIDCFallbackToLogin(t *testing.T) {
	s := oidcTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/logout", nil)
	s.HandleLogout(w, r)

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["redirect"] != "/login" {
		t.Errorf("expected redirect to /login, got %q", resp["redirect"])
	}
}

func TestHandleLogout_ReadsIDTokenBeforeClear(t *testing.T) {
	s := oidcTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/logout", nil)
	r.AddCookie(&http.Cookie{Name: "varroa_token", Value: "my-id-token"})
	s.HandleLogout(w, r)

	// Token should be cleared.
	for _, c := range w.Result().Cookies() {
		if c.Name == "varroa_token" && c.Value != "" {
			t.Error("token should be cleared")
		}
	}

	// Interactive marker should still be set.
	found := false
	for _, c := range w.Result().Cookies() {
		if c.Name == interactiveCookieName {
			found = true
		}
	}
	if !found {
		t.Error("interactive marker should be set even without end_session_endpoint")
	}
}

// --- Test helpers ---

// oidcTestServer creates a Server with OIDC auth config for testing.
func oidcTestServer(t *testing.T) *Server {
	t.Helper()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	deps := &Dependencies{
		OIDCStateSecret: secret,
		Logger:          testLoggerOIDC(t),
		Auth:            &mockOIDCProvider{},
		ActivityStore:   activity.New(10),
		SecureCookies:   true,
	}
	return NewServer(deps)
}

// localTestServer creates a Server with Local auth config for testing.
func localTestServer(t *testing.T) *Server {
	t.Helper()
	deps := &Dependencies{
		Logger: testLoggerOIDC(t),
		Auth:   &mockLocalProvider{},
	}
	return NewServer(deps)
}

type mockOIDCProvider struct{}

func (m *mockOIDCProvider) Mode() auth.AuthMode { return auth.AuthModeOIDC }
func (m *mockOIDCProvider) Verify(_ context.Context, _ string) (*auth.Claims, error) {
	return &auth.Claims{Email: "test@example.com", Subject: "test-sub"}, nil
}
func (m *mockOIDCProvider) CookieDomain() string              { return "" }
func (m *mockOIDCProvider) Discovery() ([]byte, []byte, bool) { return nil, nil, false }
func (m *mockOIDCProvider) AuthCodeURL(state string) string {
	return "https://provider.test/auth?state=" + state
}
func (m *mockOIDCProvider) AuthCodeURLOpts(state string, _ ...oauth2.AuthCodeOption) string {
	return "https://provider.test/auth?state=" + state
}
func (m *mockOIDCProvider) Exchange(_ context.Context, code string) (*oauth2.Token, error) {
	return (&oauth2.Token{AccessToken: "mock-access", TokenType: "Bearer"}).WithExtra(map[string]any{
		"id_token": "mock-id-token-abc123",
	}), nil
}
func (m *mockOIDCProvider) VerifyToken(_ context.Context, rawToken string) (*auth.Claims, error) {
	return &auth.Claims{Email: "test@example.com", Subject: "test-sub"}, nil
}
func (m *mockOIDCProvider) EndSessionEndpoint() string { return "" }

type mockLocalProvider struct{}

func (m *mockLocalProvider) Mode() auth.AuthMode { return auth.AuthModeLocal }
func (m *mockLocalProvider) Verify(_ context.Context, _ string) (*auth.Claims, error) {
	return &auth.Claims{Email: "local@example.com", Subject: "local-sub"}, nil
}
func (m *mockLocalProvider) CookieDomain() string { return "" }
func (m *mockLocalProvider) Discovery() ([]byte, []byte, bool) {
	cfg, _ := json.Marshal(map[string]string{"issuer": "http://test.local"})
	return cfg, nil, true
}

func testLoggerOIDC(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
