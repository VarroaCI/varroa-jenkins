package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/signing"
)

// TestValidatorVerify_RefusesLocalPrefix checks that an IdP token whose
// subject uses the local account prefix fails in every mode, also when no
// MultiProvider wraps the validator.
func TestValidatorVerify_RefusesLocalPrefix(t *testing.T) {
	signer, err := signing.New()
	if err != nil {
		t.Fatal(err)
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                                srv.URL,
				"authorization_endpoint":                srv.URL + "/auth",
				"token_endpoint":                        srv.URL + "/token",
				"jwks_uri":                              srv.URL + "/jwks",
				"id_token_signing_alg_values_supported": []string{"RS256"},
			})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(signer.JWKS())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	v, err := NewValidator(context.Background(), srv.URL, "varroa", "secret", srv.URL+"/cb", "openid")
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	sign := func(sub string) string {
		tok, err := signer.SignJWT(map[string]any{
			"iss": srv.URL, "aud": "varroa", "sub": sub, "preferred_username": sub,
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	if c, err := v.Verify(context.Background(), sign("alice")); err != nil || c.Subject != "alice" {
		t.Fatalf("plain subject: claims=%v err=%v", c, err)
	}
	if _, err := v.Verify(context.Background(), sign("local:rescue")); !errors.Is(err, ErrLocalPrefixFromIdP) {
		t.Errorf("local: subject: err = %v, want ErrLocalPrefixFromIdP", err)
	}
}
