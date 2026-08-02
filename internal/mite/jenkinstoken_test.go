package mite

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/auth"
)

func TestNewMiteTokenSigner(t *testing.T) {
	s, err := NewMiteTokenSigner()
	if err != nil {
		t.Fatalf("NewMiteTokenSigner: %v", err)
	}
	if s.signer == nil {
		t.Fatal("expected non-nil signer")
	}
	if s.KID() == "" {
		t.Fatal("expected non-empty KID")
	}
	if s.PublicKeyPEM() == "" {
		t.Fatal("expected non-empty public key PEM")
	}
}

func TestNewMiteTokenSignerFromPEM(t *testing.T) {
	// Generate a signer and export its key.
	orig, err := NewMiteTokenSigner()
	if err != nil {
		t.Fatalf("NewMiteTokenSigner: %v", err)
	}
	privPEM, err := orig.PrivateKeyPEM()
	if err != nil {
		t.Fatalf("PrivateKeyPEM: %v", err)
	}
	if privPEM == "" {
		t.Fatal("expected non-empty private key PEM")
	}

	// Load it back.
	loaded, err := NewMiteTokenSignerFromPEM([]byte(privPEM))
	if err != nil {
		t.Fatalf("NewMiteTokenSignerFromPEM: %v", err)
	}

	// KID must be stable (same key → same KID).
	if loaded.KID() != orig.KID() {
		t.Errorf("KID mismatch: orig=%s loaded=%s", orig.KID(), loaded.KID())
	}

	// Public key PEM must match.
	if loaded.PublicKeyPEM() != orig.PublicKeyPEM() {
		t.Error("public key PEM mismatch after round-trip")
	}

	// Tokens signed by both must verify with the same public key.
	tok1, err := orig.GenerateMiteJenkinsToken("test", "ns", time.Hour)
	if err != nil {
		t.Fatalf("GenerateMiteJenkinsToken (orig): %v", err)
	}
	tok2, err := loaded.GenerateMiteJenkinsToken("test", "ns", time.Hour)
	if err != nil {
		t.Fatalf("GenerateMiteJenkinsToken (loaded): %v", err)
	}
	// Both tokens must have the same KID in the header.
	kid1 := tokenKID(t, tok1)
	kid2 := tokenKID(t, tok2)
	if kid1 != orig.KID() || kid2 != orig.KID() {
		t.Errorf("token KID mismatch: want=%s got=%s/%s", orig.KID(), kid1, kid2)
	}

	// Tokens must be verifiable with the original public key (golden format).
	verifyTokenSignature(t, tok1, orig)
	verifyTokenSignature(t, tok2, loaded)
}

func TestGenerateMiteJenkinsToken(t *testing.T) {
	s, err := NewMiteTokenSigner()
	if err != nil {
		t.Fatalf("NewMiteTokenSigner: %v", err)
	}

	tok, err := s.GenerateMiteJenkinsToken("myctl", "myns", 10*time.Minute)
	if err != nil {
		t.Fatalf("GenerateMiteJenkinsToken: %v", err)
	}

	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}

	// Decode header.
	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var hdr struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if hdr.Alg != "RS256" {
		t.Errorf("expected alg=RS256, got %s", hdr.Alg)
	}
	if hdr.Typ != "JWT" {
		t.Errorf("expected typ=JWT, got %s", hdr.Typ)
	}

	// Decode claims.
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims miteClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Iss != "varroa-operator" {
		t.Errorf("expected iss=varroa-operator, got %s", claims.Iss)
	}
	if claims.Sub != "system:varroa-mite" {
		t.Errorf("expected sub=system:varroa-mite, got %s", claims.Sub)
	}
	if claims.Aud != "myns/myctl" {
		t.Errorf("expected aud=myns/myctl, got %s", claims.Aud)
	}
	if claims.Exp == 0 {
		t.Error("expected non-zero exp")
	}
	if claims.Iat == 0 {
		t.Error("expected non-zero iat")
	}

	// Signature must be non-empty.
	if len(parts[2]) == 0 {
		t.Error("expected non-empty signature")
	}

	// Signature must verify (golden format test: after refactor,
	// GenerateMiteJenkinsToken must still produce valid RS256 tokens).
	verifyTokenSignature(t, tok, s)
}

func TestKIDStability(t *testing.T) {
	// Two successive calls must produce different KIDs (fresh keys).
	s1, _ := NewMiteTokenSigner()
	s2, _ := NewMiteTokenSigner()
	if s1.KID() == s2.KID() {
		t.Error("expected different KIDs for different keys")
	}

	// Same key must produce the same KID.
	privPEM, _ := s1.PrivateKeyPEM()
	loaded, _ := NewMiteTokenSignerFromPEM([]byte(privPEM))
	if loaded.KID() != s1.KID() {
		t.Errorf("KID not stable: orig=%s loaded=%s", s1.KID(), loaded.KID())
	}
}

func TestNewMiteTokenSignerFromPEMInvalid(t *testing.T) {
	_, err := NewMiteTokenSignerFromPEM([]byte("not a pem"))
	if err == nil {
		t.Error("expected error for invalid PEM")
	}

	_, err = NewMiteTokenSignerFromPEM([]byte("-----BEGIN PRIVATE KEY-----\ninvalid\n-----END PRIVATE KEY-----"))
	if err == nil {
		t.Error("expected error for invalid key data")
	}
}

func TestGenerateOperatorJenkinsToken(t *testing.T) {
	s, err := NewMiteTokenSigner()
	if err != nil {
		t.Fatalf("NewMiteTokenSigner: %v", err)
	}

	tok, err := s.GenerateOperatorJenkinsToken("myctl", "myns", 10*time.Minute)
	if err != nil {
		t.Fatalf("GenerateOperatorJenkinsToken: %v", err)
	}

	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}

	// Decode header.
	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var hdr struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if hdr.Alg != "RS256" {
		t.Errorf("expected alg=RS256, got %s", hdr.Alg)
	}
	if hdr.Typ != "JWT" {
		t.Errorf("expected typ=JWT, got %s", hdr.Typ)
	}

	// Decode claims.
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims miteClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Iss != "varroa-operator" {
		t.Errorf("expected iss=varroa-operator, got %s", claims.Iss)
	}
	if claims.Sub != "system:varroa-operator" {
		t.Errorf("expected sub=system:varroa-operator, got %s", claims.Sub)
	}
	if claims.Aud != "myns/myctl" {
		t.Errorf("expected aud=myns/myctl, got %s", claims.Aud)
	}
	if claims.Exp == 0 {
		t.Error("expected non-zero exp")
	}
	if claims.Iat == 0 {
		t.Error("expected non-zero iat")
	}

	// Signature must be non-empty.
	if len(parts[2]) == 0 {
		t.Error("expected non-empty signature")
	}

	// Signature must verify (golden format test).
	verifyTokenSignature(t, tok, s)
}

func TestGenerateOperatorJenkinsToken_ExpiresAtTTL(t *testing.T) {
	s, err := NewMiteTokenSigner()
	if err != nil {
		t.Fatalf("NewMiteTokenSigner: %v", err)
	}

	tok, err := s.GenerateOperatorJenkinsToken("myctl", "myns", 5*time.Second)
	if err != nil {
		t.Fatalf("GenerateOperatorJenkinsToken: %v", err)
	}

	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims miteClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}

	// exp should be approximately iat + 5 seconds (within a small tolerance).
	gotTTL := claims.Exp - claims.Iat
	if gotTTL < 4 || gotTTL > 6 {
		t.Errorf("expected exp ≈ iat + 5s, got diff=%d (iat=%d exp=%d)", gotTTL, claims.Iat, claims.Exp)
	}
}

func TestGenerateOperatorJenkinsToken_AudienceEncodesTarget(t *testing.T) {
	s, err := NewMiteTokenSigner()
	if err != nil {
		t.Fatalf("NewMiteTokenSigner: %v", err)
	}

	decodeClaims := func(token string) miteClaims {
		t.Helper()
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			t.Fatalf("expected 3 JWT parts, got %d", len(parts))
		}
		claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatalf("decode claims: %v", err)
		}
		var c miteClaims
		if err := json.Unmarshal(claimsJSON, &c); err != nil {
			t.Fatalf("unmarshal claims: %v", err)
		}
		return c
	}

	tokA, err := s.GenerateOperatorJenkinsToken("ctrl-a", "ns-a", time.Minute)
	if err != nil {
		t.Fatalf("GenerateOperatorJenkinsToken(ctrl-a, ns-a): %v", err)
	}
	tokB, err := s.GenerateOperatorJenkinsToken("ctrl-b", "ns-b", time.Minute)
	if err != nil {
		t.Fatalf("GenerateOperatorJenkinsToken(ctrl-b, ns-b): %v", err)
	}

	cA := decodeClaims(tokA)
	cB := decodeClaims(tokB)

	if cA.Aud != "ns-a/ctrl-a" {
		t.Errorf("expected aud=ns-a/ctrl-a, got %s", cA.Aud)
	}
	if cB.Aud != "ns-b/ctrl-b" {
		t.Errorf("expected aud=ns-b/ctrl-b, got %s", cB.Aud)
	}
	if cA.Aud == cB.Aud {
		t.Error("aud for different targets must differ")
	}
}

func TestGenerateUserJenkinsToken(t *testing.T) {
	s, err := NewMiteTokenSigner()
	if err != nil {
		t.Fatalf("NewMiteTokenSigner: %v", err)
	}
	wantGroups := []string{"platform", "dev"}
	tok, err := s.GenerateUserJenkinsToken("controller", "jenkins", &auth.Claims{
		Subject: "oidc-subject", PreferredUsername: "alice", Email: "alice@example.com",
		Name: "Alice", Groups: wantGroups,
	}, 5*time.Minute)
	if err != nil {
		t.Fatalf("GenerateUserJenkinsToken: %v", err)
	}

	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}
	var claims userClaims
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Iss != "varroa-operator" || claims.Sub != "oidc-subject" || claims.VarroaType != "user" {
		t.Errorf("unexpected identity claims: %+v", claims)
	}
	if claims.PreferredUsername != "alice" || claims.Email != "alice@example.com" || claims.Name != "Alice" {
		t.Errorf("unexpected profile claims: %+v", claims)
	}
	if claims.Aud != "jenkins/controller" || claims.Exp <= claims.Iat || claims.Exp-claims.Iat != 300 {
		t.Errorf("unexpected time/audience claims: %+v", claims)
	}
	if len(claims.Groups) != len(wantGroups) || claims.Groups[0] != wantGroups[0] || claims.Groups[1] != wantGroups[1] {
		t.Errorf("groups not preserved: got %#v want %#v", claims.Groups, wantGroups)
	}
	if tokenKID(t, tok) != s.KID() {
		t.Errorf("unexpected KID: got %q want %q", tokenKID(t, tok), s.KID())
	}
	verifyTokenSignature(t, tok, s)
}

func TestGenerateUserJenkinsTokenEmptySubject(t *testing.T) {
	s, err := NewMiteTokenSigner()
	if err != nil {
		t.Fatalf("NewMiteTokenSigner: %v", err)
	}
	for _, claims := range []*auth.Claims{nil, {}, {Subject: ""}} {
		tok, err := s.GenerateUserJenkinsToken("controller", "jenkins", claims, time.Minute)
		if err == nil || err.Error() != "user token requires a subject" {
			t.Errorf("expected empty-subject error, got token=%q err=%v", tok, err)
		}
		if tok != "" {
			t.Errorf("expected no token, got %q", tok)
		}
	}
}

func TestGenerateUserJenkinsTokenPreservesNilAndEmptyGroups(t *testing.T) {
	s, err := NewMiteTokenSigner()
	if err != nil {
		t.Fatalf("NewMiteTokenSigner: %v", err)
	}
	for _, want := range [][]string{nil, {}} {
		tok, err := s.GenerateUserJenkinsToken("controller", "jenkins", &auth.Claims{Subject: "subject", Groups: want}, time.Minute)
		if err != nil {
			t.Fatalf("GenerateUserJenkinsToken: %v", err)
		}
		parts := strings.Split(tok, ".")
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		var got userClaims
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if (got.Groups == nil) != (want == nil) || len(got.Groups) != len(want) {
			t.Errorf("groups nil/empty shape changed: got %#v want %#v", got.Groups, want)
		}
	}
}

func tokenKID(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}
	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var hdr struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	return hdr.Kid
}

// verifyTokenSignature checks that a JWT was signed with the signer's
// private key (golden test: validates the RS256 format is intact after
// the signing package refactor).
func verifyTokenSignature(t *testing.T, token string, s *MiteTokenSigner) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}

	signingInput := parts[0] + "." + parts[1]
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}

	pubKey := s.signer.PublicKey()
	if err := rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, hashed[:], sig); err != nil {
		t.Fatalf("signature verification failed: %v", err)
	}
}
