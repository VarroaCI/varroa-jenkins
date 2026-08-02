package ldap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	goldap "github.com/go-ldap/ldap/v3"

	"github.com/varroaci/varroa-jenkins/internal/signing"
)

// mockConn is a fake LDAP connection for tests.
type mockConn struct {
	// binds maps "dn:password" → error (nil means success).
	binds         map[string]error
	searchResults map[string][]*goldap.Entry
}

func newMockConn() *mockConn {
	return &mockConn{
		binds:         make(map[string]error),
		searchResults: make(map[string][]*goldap.Entry),
	}
}

func (m *mockConn) Bind(dn, password string) error {
	key := dn + ":" + password
	if err, ok := m.binds[key]; ok {
		return err
	}
	return goldap.NewError(goldap.LDAPResultInvalidCredentials, errors.New("invalid credentials"))
}

func (m *mockConn) Search(req *goldap.SearchRequest) (*goldap.SearchResult, error) {
	if entries, ok := m.searchResults[req.Filter]; ok {
		return &goldap.SearchResult{Entries: entries}, nil
	}
	return &goldap.SearchResult{}, nil
}

func (m *mockConn) Close() error { return nil }

// makeEntry is a test helper for goldap.NewEntry.
func makeEntry(dn string, attrs map[string][]string) *goldap.Entry {
	return goldap.NewEntry(dn, attrs)
}

// newTestProvider builds a Provider with an injected mock dialer.
func newTestProvider(t *testing.T, cfg Config, conn *mockConn) *Provider {
	t.Helper()
	signer, err := signing.New()
	if err != nil {
		t.Fatalf("signing.New: %v", err)
	}
	p := New(signer, cfg, "http://varroa.test", "varroa", 8*time.Hour, "")
	p.dial = func(_ Config) (ldapConn, error) { return conn, nil }
	return p
}

// jwtPayload decodes the claims section of a JWT and returns the parsed map.
func jwtPayload(t *testing.T, tok string) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	return out
}

// --- Direct bind tests ---

func TestLogin_DirectBind_Success(t *testing.T) {
	conn := newMockConn()
	conn.binds["uid=alice,ou=people,dc=test:secret"] = nil // nil = success

	cfg := Config{BindDNTemplate: "uid=%s,ou=people,dc=test"}
	p := newTestProvider(t, cfg, conn)

	tok, exp, err := p.Login(context.Background(), "alice", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if tok == "" {
		t.Fatal("expected non-empty token")
	}
	if exp <= 0 {
		t.Fatalf("expected positive expiresIn, got %d", exp)
	}

	claims := jwtPayload(t, tok)
	if claims["sub"] != "alice" {
		t.Errorf("sub claim = %v, want alice", claims["sub"])
	}
}

func TestLogin_DirectBind_WrongPassword(t *testing.T) {
	conn := newMockConn()

	cfg := Config{BindDNTemplate: "uid=%s,ou=people,dc=test"}
	p := newTestProvider(t, cfg, conn)

	_, _, err := p.Login(context.Background(), "alice", "wrong")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestLogin_DirectBind_DNSpecialChars(t *testing.T) {
	conn := newMockConn()
	// Username with a comma — EscapeDN should escape the comma as \, so the
	// resulting DN becomes "uid=ali\\,ce,ou=people,dc=test".
	escapedDN := `uid=ali\,ce,ou=people,dc=test`
	conn.binds[escapedDN+":secret"] = nil

	cfg := Config{BindDNTemplate: "uid=%s,ou=people,dc=test"}
	p := newTestProvider(t, cfg, conn)

	tok, _, err := p.Login(context.Background(), "ali,ce", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	claims := jwtPayload(t, tok)
	if claims["sub"] != "ali,ce" {
		t.Errorf("sub claim = %v, want ali,ce", claims["sub"])
	}
}

func TestLogin_EmptyPassword_Rejected(t *testing.T) {
	conn := newMockConn()
	// Even if LDAP would accept an empty-password bind, we reject it first.
	conn.binds["uid=alice,ou=people,dc=test:"] = nil

	cfg := Config{BindDNTemplate: "uid=%s,ou=people,dc=test"}
	p := newTestProvider(t, cfg, conn)

	_, _, err := p.Login(context.Background(), "alice", "")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials for empty password, got %v", err)
	}
}

// --- Search-then-bind tests ---

func TestLogin_SearchBind_Success(t *testing.T) {
	conn := newMockConn()

	userDN := "uid=bob,ou=people,dc=test"
	conn.binds["cn=svc,dc=test:svcpass"] = nil
	conn.binds[userDN+":bobsecret"] = nil
	conn.searchResults["(uid=bob)"] = []*goldap.Entry{
		makeEntry(userDN, map[string][]string{
			"mail": {"bob@test.com"},
			"cn":   {"Bob Smith"},
		}),
	}

	cfg := Config{
		ServiceAccountDN:       "cn=svc,dc=test",
		ServiceAccountPassword: "svcpass",
		UserSearch: &UserSearchConfig{
			BaseDN:    "ou=people,dc=test",
			Filter:    "(uid=%s)",
			EmailAttr: "mail",
			NameAttr:  "cn",
		},
	}
	p := newTestProvider(t, cfg, conn)

	tok, _, err := p.Login(context.Background(), "bob", "bobsecret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	claims := jwtPayload(t, tok)
	if claims["email"] != "bob@test.com" {
		t.Errorf("email = %v, want bob@test.com", claims["email"])
	}
	if claims["name"] != "Bob Smith" {
		t.Errorf("name = %v, want Bob Smith", claims["name"])
	}
}

func TestLogin_SearchBind_UserNotFound(t *testing.T) {
	conn := newMockConn()
	conn.binds["cn=svc,dc=test:svcpass"] = nil

	cfg := Config{
		ServiceAccountDN:       "cn=svc,dc=test",
		ServiceAccountPassword: "svcpass",
		UserSearch: &UserSearchConfig{
			BaseDN: "ou=people,dc=test",
			Filter: "(uid=%s)",
		},
	}
	p := newTestProvider(t, cfg, conn)

	_, _, err := p.Login(context.Background(), "nobody", "pass")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestLogin_SearchBind_WrongPassword(t *testing.T) {
	conn := newMockConn()
	conn.binds["cn=svc,dc=test:svcpass"] = nil
	conn.searchResults["(uid=carol)"] = []*goldap.Entry{
		makeEntry("uid=carol,ou=people,dc=test", nil),
	}
	// User bind NOT registered → returns invalid credentials.

	cfg := Config{
		ServiceAccountDN:       "cn=svc,dc=test",
		ServiceAccountPassword: "svcpass",
		UserSearch: &UserSearchConfig{
			BaseDN: "ou=people,dc=test",
			Filter: "(uid=%s)",
		},
	}
	p := newTestProvider(t, cfg, conn)

	_, _, err := p.Login(context.Background(), "carol", "wrong")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

// --- Group resolution tests ---

func TestLogin_Groups(t *testing.T) {
	conn := newMockConn()
	userDN := "uid=dave,ou=people,dc=test"
	conn.binds[userDN+":davepass"] = nil

	escapedDN := goldap.EscapeFilter(userDN)
	groupFilter := "(member=" + escapedDN + ")"
	conn.searchResults[groupFilter] = []*goldap.Entry{
		makeEntry("cn=devs,ou=groups,dc=test", map[string][]string{"cn": {"devs"}}),
		makeEntry("cn=varroa-admins,ou=groups,dc=test", map[string][]string{"cn": {"varroa-admins"}}),
	}

	cfg := Config{
		BindDNTemplate: "uid=%s,ou=people,dc=test",
		GroupSearch: &GroupSearchConfig{
			BaseDN:   "ou=groups,dc=test",
			Filter:   "(member=%s)",
			NameAttr: "cn",
		},
	}
	p := newTestProvider(t, cfg, conn)

	tok, _, err := p.Login(context.Background(), "dave", "davepass")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	claims := jwtPayload(t, tok)
	groups, _ := claims["groups"].([]any)
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %v", groups)
	}
}

func TestLogin_NoGroupSearch_EmptyGroups(t *testing.T) {
	conn := newMockConn()
	conn.binds["uid=eve,ou=people,dc=test:evepass"] = nil

	cfg := Config{BindDNTemplate: "uid=%s,ou=people,dc=test"}
	p := newTestProvider(t, cfg, conn)

	tok, _, err := p.Login(context.Background(), "eve", "evepass")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	claims := jwtPayload(t, tok)
	groups, _ := claims["groups"].([]any)
	if len(groups) != 0 {
		t.Fatalf("expected 0 groups when GroupSearch is nil, got %v", groups)
	}
}

// --- Token verification tests ---

func TestVerify_RoundTrip(t *testing.T) {
	conn := newMockConn()
	conn.binds["uid=frank,ou=people,dc=test:frankpass"] = nil

	cfg := Config{BindDNTemplate: "uid=%s,ou=people,dc=test"}
	p := newTestProvider(t, cfg, conn)

	tok, _, err := p.Login(context.Background(), "frank", "frankpass")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	got, err := p.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Subject != "frank" {
		t.Errorf("Subject = %q, want frank", got.Subject)
	}
}

func TestVerify_ExpiredToken(t *testing.T) {
	signer, _ := signing.New()
	p := New(signer, Config{BindDNTemplate: "uid=%s,dc=test"}, "http://varroa.test", "varroa", -1*time.Hour, "")
	conn := newMockConn()
	conn.binds["uid=grace,dc=test:pass"] = nil
	p.dial = func(_ Config) (ldapConn, error) { return conn, nil }

	tok, _, err := p.Login(context.Background(), "grace", "pass")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	_, err = p.Verify(context.Background(), tok)
	if !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("expected ErrExpiredToken, got %v", err)
	}
}

func TestVerify_InvalidTokenFormat(t *testing.T) {
	signer, _ := signing.New()
	p := New(signer, Config{}, "http://varroa.test", "varroa", 8*time.Hour, "")

	_, err := p.Verify(context.Background(), "not.a.valid.jwt.at.all")
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func TestVerify_WrongIssuer(t *testing.T) {
	conn := newMockConn()
	conn.binds["uid=henry,dc=test:pass"] = nil

	cfg := Config{BindDNTemplate: "uid=%s,dc=test"}
	p := newTestProvider(t, cfg, conn)

	tok, _, err := p.Login(context.Background(), "henry", "pass")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Verify with a different issuer URL — should fail.
	p2 := New(p.signer, cfg, "http://other.issuer", "varroa", 8*time.Hour, "")
	_, err = p2.Verify(context.Background(), tok)
	if err == nil || !strings.Contains(err.Error(), "unexpected issuer") {
		t.Fatalf("expected 'unexpected issuer' error, got %v", err)
	}
}

// --- auth.Provider interface tests ---

func TestProviderMode(t *testing.T) {
	signer, _ := signing.New()
	p := New(signer, Config{}, "http://v.test", "c", 8*time.Hour, "")
	if p.Mode() != "ldap" {
		t.Errorf("Mode() = %q, want ldap", p.Mode())
	}
}

func TestProviderCookieDomain(t *testing.T) {
	signer, _ := signing.New()
	p := New(signer, Config{}, "http://v.test", "c", 8*time.Hour, ".example.com")
	if p.CookieDomain() != ".example.com" {
		t.Errorf("CookieDomain() = %q, want .example.com", p.CookieDomain())
	}
}

func TestProviderDiscovery(t *testing.T) {
	signer, _ := signing.New()
	p := New(signer, Config{}, "http://v.test", "c", 8*time.Hour, "")
	cfgJSON, jwksJSON, ok := p.Discovery()
	if !ok {
		t.Fatal("Discovery() ok = false")
	}
	if len(cfgJSON) == 0 || len(jwksJSON) == 0 {
		t.Fatal("Discovery() returned empty JSON")
	}
	var oidcMeta map[string]any
	if err := json.Unmarshal(cfgJSON, &oidcMeta); err != nil {
		t.Fatalf("unmarshal openid-config: %v", err)
	}
	if oidcMeta["issuer"] != "http://v.test" {
		t.Errorf("issuer = %v, want http://v.test", oidcMeta["issuer"])
	}
}

func TestRateLimit(t *testing.T) {
	conn := newMockConn()
	cfg := Config{BindDNTemplate: "uid=%s,dc=test"}
	p := newTestProvider(t, cfg, conn)

	// Exhaust the 5-attempt burst.
	for i := 0; i < 5; i++ {
		_, _, _ = p.Login(context.Background(), "ivan", "x")
	}
	_, _, err := p.Login(context.Background(), "ivan", "x")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited after burst, got %v", err)
	}
}

func TestLogin_NoBindMode_Error(t *testing.T) {
	conn := newMockConn()
	cfg := Config{} // no BindDNTemplate, no UserSearch
	p := newTestProvider(t, cfg, conn)

	_, _, err := p.Login(context.Background(), "user", "pass")
	if err == nil || !strings.Contains(err.Error(), "no bind mode configured") {
		t.Fatalf("expected 'no bind mode configured' error, got %v", err)
	}
}
