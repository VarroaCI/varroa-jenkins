package local

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/signing"
)

// mockStore implements userStore for tests.
type mockStore struct {
	users  map[string]*v1alpha1.User
	groups map[string]*v1alpha1.Group
	// notFoundErr models the real client, which returns a k8s NotFound
	// error (rather than nil, nil) for a missing user.
	notFoundErr bool
}

func newMockStore() *mockStore {
	return &mockStore{
		users:  map[string]*v1alpha1.User{},
		groups: map[string]*v1alpha1.Group{},
	}
}

func (m *mockStore) GetUserCRD(_ context.Context, name, _ string) (*v1alpha1.User, error) {
	u, ok := m.users[name]
	if !ok {
		if m.notFoundErr {
			return nil, apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "users"}, name)
		}
		return nil, nil
	}
	return u, nil
}

func (m *mockStore) ListGroupCRDs(_ context.Context) ([]*v1alpha1.Group, error) {
	out := make([]*v1alpha1.Group, 0, len(m.groups))
	for _, g := range m.groups {
		out = append(out, g)
	}
	return out, nil
}

func (m *mockStore) PatchUserStatus(_ context.Context, _, _ string, _ *v1alpha1.UserStatus) error {
	return nil
}

func (m *mockStore) ApplyUserCRD(_ context.Context, u *v1alpha1.User) error {
	m.users[u.Name] = u
	return nil
}

func (m *mockStore) addUser(name, password, email, displayName string) {
	hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost) // fast for tests
	m.users[name] = &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.UserSpec{Email: email, DisplayName: displayName},
		Status:     v1alpha1.UserStatus{Credentials: &v1alpha1.UserCredentials{PasswordHash: string(hash)}},
	}
}

func (m *mockStore) addGroup(name string, members []string) {
	m.groups[name] = &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.GroupSpec{DisplayName: name, Members: members},
	}
}

func newTestProvider(t *testing.T) (*Provider, *signing.Signer) {
	t.Helper()
	signer, err := signing.New()
	if err != nil {
		t.Fatalf("signing.New: %v", err)
	}
	store := newMockStore()
	p := &Provider{
		signer:    signer,
		store:     store,
		ns:        "default",
		issuerURL: "http://localhost:8080",
		clientID:  "varroa",
		ttl:       time.Hour,
		limiter:   newLoginLimiter(),
	}
	return p, signer
}

func TestProvider_Mode(t *testing.T) {
	p, _ := newTestProvider(t)
	if p.Mode() != "local" {
		t.Errorf("expected mode=local, got %s", p.Mode())
	}
}

func TestProvider_CookieDomain(t *testing.T) {
	p, _ := newTestProvider(t)
	if p.CookieDomain() != "" {
		t.Errorf("expected empty cookie domain, got %s", p.CookieDomain())
	}
	p.cookieDomain = "example.com"
	if p.CookieDomain() != "example.com" {
		t.Errorf("expected cookie domain example.com, got %s", p.CookieDomain())
	}
}

func TestProvider_Login_UnknownUser(t *testing.T) {
	p, _ := newTestProvider(t)
	_, _, err := p.Login(context.Background(), "nobody", "password")
	if err != ErrInvalidCredentials {
		t.Errorf("expected ErrInvalidCredentials for unknown user, got %v", err)
	}
}

func TestProvider_Login_NotFoundError(t *testing.T) {
	// The real client returns a k8s NotFound error (not nil, nil) for a
	// missing user. Login must map that to ErrInvalidCredentials, not a
	// generic wrapped error, so the handler returns 401 and there is no
	// user enumeration.
	p, _ := newTestProvider(t)
	p.store.(*mockStore).notFoundErr = true
	_, _, err := p.Login(context.Background(), "ghost", "password")
	if err != ErrInvalidCredentials {
		t.Errorf("expected ErrInvalidCredentials for NotFound user, got %v", err)
	}
}

func TestProvider_Login_WrongPassword(t *testing.T) {
	p, _ := newTestProvider(t)
	// Add a user with a known password.
	p.store.(*mockStore).addUser("alice", "correct", "alice@example.com", "Alice")
	_, _, err := p.Login(context.Background(), "alice", "wrong")
	if err != ErrInvalidCredentials {
		t.Errorf("expected ErrInvalidCredentials for wrong password, got %v", err)
	}
	// Same error as unknown user (no user enumeration).
	_, _, err = p.Login(context.Background(), "nobody", "password")
	if err != ErrInvalidCredentials {
		t.Errorf("expected ErrInvalidCredentials for unknown user, got %v", err)
	}
}

func TestProvider_Login_Success(t *testing.T) {
	p, signer := newTestProvider(t)
	p.store.(*mockStore).addUser("alice", "correct", "alice@example.com", "Alice")
	tok, expiresIn, err := p.Login(context.Background(), "alice", "correct")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if tok == "" {
		t.Fatal("expected non-empty token")
	}
	if expiresIn <= 0 {
		t.Errorf("expected positive expires_in, got %d", expiresIn)
	}

	// Verify the token can be validated back.
	claims, err := p.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != "alice" {
		t.Errorf("expected sub=alice, got %s", claims.Subject)
	}
	if claims.Email != "alice@example.com" {
		t.Errorf("expected email=alice@example.com, got %s", claims.Email)
	}
	if signer.KID() == "" {
		t.Error("expected non-empty kid in signer")
	}
}

func TestProvider_Login_WithGroups(t *testing.T) {
	p, _ := newTestProvider(t)
	p.store.(*mockStore).addUser("alice", "correct", "alice@example.com", "Alice")
	p.store.(*mockStore).addGroup("admins", []string{"alice"})
	p.store.(*mockStore).addGroup("developers", []string{"alice", "bob"})

	tok, _, err := p.Login(context.Background(), "alice", "correct")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	claims, err := p.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	groups := claims.Groups
	if len(groups) != 2 {
		t.Errorf("expected 2 groups, got %d: %v", len(groups), groups)
	}
	found := map[string]bool{}
	for _, g := range groups {
		found[g] = true
	}
	if !found["admins"] || !found["developers"] {
		t.Errorf("expected groups [admins developers], got %v", groups)
	}
}

func TestProvider_Login_RateLimit(t *testing.T) {
	p, _ := newTestProvider(t)
	for i := 0; i < 5; i++ {
		_, _, err := p.Login(context.Background(), "alice", "x")
		if err != ErrInvalidCredentials {
			t.Fatalf("call %d: expected ErrInvalidCredentials, got %v", i+1, err)
		}
	}
	_, _, err := p.Login(context.Background(), "alice", "x")
	if err != ErrRateLimited {
		t.Errorf("6th call: expected ErrRateLimited, got %v", err)
	}
}

func TestProvider_Verify_InvalidToken(t *testing.T) {
	p, _ := newTestProvider(t)
	_, err := p.Verify(context.Background(), "not.a.jwt")
	if err == nil {
		t.Error("expected error for invalid token format")
	}
}

func TestProvider_Verify_TamperedSignature(t *testing.T) {
	p, signer := newTestProvider(t)
	claims := map[string]any{
		"iss": "http://localhost:8080",
		"sub": "alice",
		"aud": "varroa",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	tok, err := signer.SignJWT(claims)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}
	parts := strings.Split(tok, ".")
	tamperedTok := parts[0] + "." + parts[1] + "." + fmt.Sprintf("%064x", 0)
	_, err = p.Verify(context.Background(), tamperedTok)
	if err == nil {
		t.Error("expected error for tampered signature")
	}
}

func TestProvider_Verify_WrongIssuer(t *testing.T) {
	p, signer := newTestProvider(t)
	claims := map[string]any{
		"iss": "varroa-operator", // mite token — must be rejected
		"sub": "system:varroa-mite",
		"aud": "varroa",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	tok, err := signer.SignJWT(claims)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}
	_, err = p.Verify(context.Background(), tok)
	if err == nil {
		t.Error("expected rejection for wrong issuer (mite token)")
	}
}

func TestProvider_Verify_WrongAudience(t *testing.T) {
	p, signer := newTestProvider(t)
	claims := map[string]any{
		"iss": "http://localhost:8080",
		"sub": "alice",
		"aud": "wrong-client",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	tok, err := signer.SignJWT(claims)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}
	_, err = p.Verify(context.Background(), tok)
	if err == nil {
		t.Error("expected rejection for wrong audience")
	}
}

func TestProvider_Verify_ExpiredToken(t *testing.T) {
	p, signer := newTestProvider(t)
	claims := map[string]any{
		"iss": "http://localhost:8080",
		"sub": "alice",
		"aud": "varroa",
		"exp": time.Now().Add(-time.Hour).Unix(),
		"iat": time.Now().Add(-2 * time.Hour).Unix(),
	}
	tok, err := signer.SignJWT(claims)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}
	_, err = p.Verify(context.Background(), tok)
	if err == nil {
		t.Error("expected error for expired token")
	}
}

func TestProvider_Discovery(t *testing.T) {
	p, signer := newTestProvider(t)
	openidConfig, jwks, ok := p.Discovery()
	if !ok {
		t.Fatal("expected discovery ok=true")
	}

	var oidcCfg map[string]any
	if err := json.Unmarshal(openidConfig, &oidcCfg); err != nil {
		t.Fatalf("openid-config JSON parse: %v", err)
	}
	if oidcCfg["issuer"] != "http://localhost:8080" {
		t.Errorf("expected issuer, got %v", oidcCfg["issuer"])
	}
	if oidcCfg["jwks_uri"] != "http://localhost:8080/.well-known/jwks.json" {
		t.Errorf("expected jwks_uri, got %v", oidcCfg["jwks_uri"])
	}

	// JWKS has the signer's single key.
	var jwksObj map[string]any
	if err := json.Unmarshal(jwks, &jwksObj); err != nil {
		t.Fatalf("jwks JSON parse: %v", err)
	}
	keys := jwksObj["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	key := keys[0].(map[string]any)
	if key["kid"] != signer.KID() {
		t.Errorf("expected kid=%s, got %v", signer.KID(), key["kid"])
	}
}
