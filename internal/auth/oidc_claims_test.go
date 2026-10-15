package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/signing"
)

// newTestIssuer starts a fake OIDC issuer and returns a validator for it.
// The returned sign function signs an ID token with the given extra claims.
func newTestIssuer(t *testing.T) (*Validator, func(map[string]any) string) {
	t.Helper()
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
	t.Cleanup(srv.Close)

	v, err := NewValidator(context.Background(), srv.URL, "varroa", "secret", srv.URL+"/cb", "openid")
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	sign := func(extra map[string]any) string {
		claims := map[string]any{
			"iss": srv.URL, "aud": "varroa", "sub": "alice",
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		}
		for k, val := range extra {
			claims[k] = val
		}
		tok, err := signer.SignJWT(claims)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	return v, sign
}

func verifyGroups(t *testing.T, v *Validator, tok string) []string {
	t.Helper()
	c, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return c.Groups
}

func TestValidatorVerify_GroupClaimRoles(t *testing.T) {
	v, sign := newTestIssuer(t)
	v.SetClaimNames(nil, []string{"roles"})
	tok := sign(map[string]any{"roles": []string{"a"}, "groups": []string{"x"}})
	if got := verifyGroups(t, v, tok); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("Groups = %v, want [a] (groups claim must not leak in)", got)
	}
}

func TestValidatorVerify_GroupClaimColonName(t *testing.T) {
	v, sign := newTestIssuer(t)
	v.SetClaimNames(nil, []string{"cognito:groups"})
	tok := sign(map[string]any{"cognito:groups": []string{"admins"}})
	if got := verifyGroups(t, v, tok); !reflect.DeepEqual(got, []string{"admins"}) {
		t.Errorf("Groups = %v, want [admins]", got)
	}
}

func TestValidatorVerify_GroupClaimSingleString(t *testing.T) {
	v, sign := newTestIssuer(t)
	v.SetClaimNames(nil, []string{"role"})
	tok := sign(map[string]any{"role": "ops"})
	if got := verifyGroups(t, v, tok); !reflect.DeepEqual(got, []string{"ops"}) {
		t.Errorf("Groups = %v, want [ops]", got)
	}
}

func TestValidatorVerify_GroupClaimStringIsNotSplit(t *testing.T) {
	v, sign := newTestIssuer(t)
	v.SetClaimNames(nil, []string{"role"})
	tok := sign(map[string]any{"role": "a, b c"})
	if got := verifyGroups(t, v, tok); !reflect.DeepEqual(got, []string{"a, b c"}) {
		t.Errorf("Groups = %v, want one value %q", got, "a, b c")
	}
}

func TestValidatorVerify_GroupClaimMultipleNamesDedup(t *testing.T) {
	v, sign := newTestIssuer(t)
	v.SetClaimNames(nil, []string{"groups", "roles"})
	tok := sign(map[string]any{"groups": []string{"a"}, "roles": []string{"a", "b"}})
	if got := verifyGroups(t, v, tok); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("Groups = %v, want [a b]", got)
	}
}

func TestValidatorVerify_GroupClaimIgnoresOtherShapes(t *testing.T) {
	v, sign := newTestIssuer(t)
	v.SetClaimNames(nil, []string{"roles"})
	tok := sign(map[string]any{"roles": []any{1, "x", ""}})
	if got := verifyGroups(t, v, tok); !reflect.DeepEqual(got, []string{"x"}) {
		t.Errorf("Groups = %v, want [x]", got)
	}

	v2, sign2 := newTestIssuer(t)
	v2.SetClaimNames(nil, []string{"roles"})
	tok2 := sign2(map[string]any{"roles": map[string]any{"admin": true}})
	if got := verifyGroups(t, v2, tok2); len(got) != 0 {
		t.Errorf("Groups = %v, want none for an object claim", got)
	}
}

func TestValidatorVerify_NestedClaimPathIsLiteral(t *testing.T) {
	v, sign := newTestIssuer(t)
	v.SetClaimNames(nil, []string{"realm_access.roles"})
	tok := sign(map[string]any{"realm_access": map[string]any{"roles": []string{"a"}}})
	if got := verifyGroups(t, v, tok); len(got) != 0 {
		t.Errorf("Groups = %v, want none: nested paths are not read", got)
	}
}

func TestValidatorVerify_DefaultGroupsClaim(t *testing.T) {
	v, sign := newTestIssuer(t)
	tok := sign(map[string]any{"groups": []string{"g"}})
	if got := verifyGroups(t, v, tok); !reflect.DeepEqual(got, []string{"g"}) {
		t.Errorf("Groups = %v, want [g]", got)
	}
}

func TestValidatorVerify_CustomUserClaim(t *testing.T) {
	v, sign := newTestIssuer(t)
	v.SetClaimNames([]string{"upn", "sub"}, nil)
	c, err := v.Verify(context.Background(), sign(map[string]any{"upn": "alice@corp"}))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := c.UserValues([]string{"upn", "sub"}); !reflect.DeepEqual(got, []string{"alice@corp", "alice"}) {
		t.Errorf("UserValues = %v, want [alice@corp alice]", got)
	}
}

func TestParseClaimNames(t *testing.T) {
	got := ParseClaimNames(" groups , roles,,groups ")
	if !reflect.DeepEqual(got, []string{"groups", "roles"}) {
		t.Errorf("ParseClaimNames = %v, want [groups roles]", got)
	}
	if got := ParseClaimNames(""); len(got) != 0 {
		t.Errorf("ParseClaimNames(\"\") = %v, want none", got)
	}
}
