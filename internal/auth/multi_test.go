package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"golang.org/x/oauth2"
)

// fakeIssuerProvider answers Verify with fixed claims and records the call.
type fakeIssuerProvider struct {
	claims *Claims
	calls  int
}

func (f *fakeIssuerProvider) Mode() AuthMode { return AuthModeOIDC }
func (f *fakeIssuerProvider) Verify(_ context.Context, _ string) (*Claims, error) {
	f.calls++
	return f.claims, nil
}
func (f *fakeIssuerProvider) CookieDomain() string { return ".example.com" }
func (f *fakeIssuerProvider) Discovery() ([]byte, []byte, bool) {
	return []byte("cfg"), []byte("jwks"), true
}
func (f *fakeIssuerProvider) AuthCodeURL(string) string { return "" }
func (f *fakeIssuerProvider) AuthCodeURLOpts(string, ...oauth2.AuthCodeOption) string {
	return ""
}
func (f *fakeIssuerProvider) Exchange(context.Context, string) (*oauth2.Token, error) {
	return nil, nil
}
func (f *fakeIssuerProvider) VerifyToken(ctx context.Context, raw string) (*Claims, error) {
	return f.Verify(ctx, raw)
}
func (f *fakeIssuerProvider) EndSessionEndpoint() string { return "" }

// unsignedToken builds a token-shaped string with the given iss claim.
func unsignedToken(t *testing.T, iss string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"iss": iss})
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".c2ln"
}

func TestMultiProvider_DispatchesByIssuer(t *testing.T) {
	idp := &fakeIssuerProvider{claims: &Claims{Subject: "alice"}}
	loc := &fakeIssuerProvider{claims: &Claims{Subject: "local:rescue"}}
	m := NewMultiProvider(idp, loc, "https://bff.example.com")

	c, err := m.Verify(context.Background(), unsignedToken(t, "https://bff.example.com"))
	if err != nil || c.Subject != "local:rescue" {
		t.Fatalf("local issuer: claims=%v err=%v", c, err)
	}
	if loc.calls != 1 || idp.calls != 0 {
		t.Fatalf("local issuer calls: local=%d idp=%d", loc.calls, idp.calls)
	}

	c, err = m.Verify(context.Background(), unsignedToken(t, "https://login.example.com"))
	if err != nil || c.Subject != "alice" {
		t.Fatalf("idp issuer: claims=%v err=%v", c, err)
	}
	if idp.calls != 1 {
		t.Fatalf("idp calls = %d, want 1", idp.calls)
	}
}

func TestMultiProvider_RejectsOIDCLocalPrefix(t *testing.T) {
	for _, c := range []*Claims{
		{Subject: "local:rescue"},
		{Subject: "abc", PreferredUsername: "local:rescue"},
	} {
		idp := &fakeIssuerProvider{claims: c}
		m := NewMultiProvider(idp, &fakeIssuerProvider{}, "https://bff.example.com")
		_, err := m.Verify(context.Background(), unsignedToken(t, "https://login.example.com"))
		if !errors.Is(err, ErrLocalPrefixFromIdP) {
			t.Errorf("claims %+v: err = %v, want ErrLocalPrefixFromIdP", c, err)
		}
	}
}

func TestMultiProvider_ModeIsPrimary(t *testing.T) {
	idp := &fakeIssuerProvider{}
	m := NewMultiProvider(idp, &fakeIssuerProvider{}, "https://bff.example.com")
	if m.Mode() != AuthModeOIDC {
		t.Errorf("Mode = %q, want oidc", m.Mode())
	}
	if m.CookieDomain() != ".example.com" {
		t.Errorf("CookieDomain = %q", m.CookieDomain())
	}
	if m.OIDC() != idp {
		t.Error("OIDC() does not return the primary provider")
	}
}

func TestIsLocalSession(t *testing.T) {
	if !IsLocalSession(&Claims{Subject: "local:a"}) {
		t.Error("local:a is a local session")
	}
	if IsLocalSession(&Claims{Subject: "a"}) || IsLocalSession(nil) {
		t.Error("a and nil are not local sessions")
	}
}
