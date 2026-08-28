package signing

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.priv == nil {
		t.Fatal("expected non-nil private key")
	}
	if s.kid == "" {
		t.Fatal("expected non-empty KID")
	}
	if s.PublicKeyPEM() == "" {
		t.Fatal("expected non-empty public key PEM")
	}

	privPEM, err := s.PrivateKeyPEM()
	if err != nil {
		t.Fatalf("PrivateKeyPEM: %v", err)
	}
	if privPEM == "" {
		t.Fatal("expected non-empty private key PEM")
	}
}

func TestKID_Equals(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.KID() != s.kid {
		t.Errorf("KID() returned %q, want %q", s.KID(), s.kid)
	}
}

func TestNewFromPEM_StableKID(t *testing.T) {
	orig, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	privPEM, err := orig.PrivateKeyPEM()
	if err != nil {
		t.Fatalf("PrivateKeyPEM: %v", err)
	}

	loaded, err := NewFromPEM([]byte(privPEM))
	if err != nil {
		t.Fatalf("NewFromPEM: %v", err)
	}

	if loaded.KID() != orig.KID() {
		t.Errorf("KID mismatch: orig=%s loaded=%s", orig.KID(), loaded.KID())
	}
	if loaded.PublicKeyPEM() != orig.PublicKeyPEM() {
		t.Error("public key PEM mismatch after round-trip")
	}
}

func TestNewFromPEM_Invalid(t *testing.T) {
	_, err := NewFromPEM([]byte("not a pem"))
	if err == nil {
		t.Error("expected error for invalid PEM")
	}

	_, err = NewFromPEM([]byte("-----BEGIN PRIVATE KEY-----\ninvalid\n-----END PRIVATE KEY-----"))
	if err == nil {
		t.Error("expected error for invalid key data")
	}
}

func TestSignJWT_RoundTrip(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	claims := map[string]any{
		"iss": "test-issuer",
		"sub": "test-subject",
		"aud": "test-audience",
		"exp": 9999999999,
		"iat": 1111111111,
	}

	tok, err := s.SignJWT(claims)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}

	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}

	// Verify header.
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
	if hdr.Kid != s.KID() {
		t.Errorf("expected kid=%s, got %s", s.KID(), hdr.Kid)
	}

	// Verify claims.
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(claimsJSON, &decoded); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if decoded["iss"] != "test-issuer" {
		t.Errorf("expected iss=test-issuer, got %v", decoded["iss"])
	}
	if decoded["sub"] != "test-subject" {
		t.Errorf("expected sub=test-subject, got %v", decoded["sub"])
	}

	// Verify signature.
	signingInput := parts[0] + "." + parts[1]
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if err := rsa.VerifyPKCS1v15(&s.priv.PublicKey, crypto.SHA256, hashed[:], sig); err != nil {
		t.Fatalf("signature verification failed: %v", err)
	}
}

func TestSignJWT_DifferentKID(t *testing.T) {
	s1, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s2, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s1.KID() == s2.KID() {
		t.Error("expected different KIDs for different keys")
	}
}

func TestJWKS_Shape(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	jwks := s.JWKS()
	if len(jwks.Keys) != 1 {
		t.Fatalf("expected 1 key in JWKS, got %d", len(jwks.Keys))
	}
	k := jwks.Keys[0]
	if k.Kty != "RSA" {
		t.Errorf("expected kty=RSA, got %s", k.Kty)
	}
	if k.Use != "sig" {
		t.Errorf("expected use=sig, got %s", k.Use)
	}
	if k.Alg != "RS256" {
		t.Errorf("expected alg=RS256, got %s", k.Alg)
	}
	if k.Kid != s.KID() {
		t.Errorf("expected kid=%s, got %s", s.KID(), k.Kid)
	}
	if k.N == "" {
		t.Error("expected non-empty n (modulus)")
	}
	if k.E == "" {
		t.Error("expected non-empty e (exponent)")
	}
}
