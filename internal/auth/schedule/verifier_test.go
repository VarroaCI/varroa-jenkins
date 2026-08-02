package schedule

import (
	"context"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/signing"
)

// TestVerifier_AudMismatch verifies that a token with a non-matching aud (but
// otherwise valid, signed by the same key) returns matched==false so the
// caller falls through to the next verifier. This simulates a local.Provider-
// issued session JWT sharing the same kid.
func TestVerifier_AudMismatch(t *testing.T) {
	signer := newTestSigner(t)
	v := NewVerifier(signer)

	// Mint a token with a different aud (simulating a local auth token).
	token := mintTestToken(t, signer, map[string]interface{}{
		"aud": "some-other-client",
		"sub": "user@test.com",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	claims, matched, err := v.Verify(context.Background(), token)
	if matched {
		t.Errorf("expected matched=false for aud mismatch, got matched=true")
	}
	if err != nil {
		t.Errorf("expected nil error for aud mismatch, got: %v", err)
	}
	if claims != nil {
		t.Errorf("expected nil claims for aud mismatch, got: %+v", claims)
	}
}

// TestVerifier_AudMismatch_LocalSessionJWT simulates a local.Provider-issued
// session JWT with the shared kid but aud=varroa (not varroa-schedule). Must
// NOT be misrouted as a schedule token.
func TestVerifier_AudMismatch_LocalSessionJWT(t *testing.T) {
	signer := newTestSigner(t)
	v := NewVerifier(signer)

	token := mintTestToken(t, signer, map[string]interface{}{
		"aud":                "varroa",
		"sub":                "alice",
		"exp":                time.Now().Add(time.Hour).Unix(),
		"preferred_username": "alice",
	})

	claims, matched, err := v.Verify(context.Background(), token)
	if matched {
		t.Errorf("expected matched=false for local session JWT, got matched=true")
	}
	if err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
	if claims != nil {
		t.Errorf("expected nil claims, got: %+v", claims)
	}
}

// TestVerifier_BadSignature verifies that a token with aud==Audience but an
// invalid signature returns matched==true and a non-nil error (hard 401).
func TestVerifier_BadSignature(t *testing.T) {
	signer := newTestSigner(t)
	v := NewVerifier(signer)

	// Use a different signer to sign (wrong key).
	otherSigner := newTestSigner(t)
	token := mintTestToken(t, otherSigner, map[string]interface{}{
		"aud": Audience,
		"sub": "schedule:default/my-schedule",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	claims, matched, err := v.Verify(context.Background(), token)
	if !matched {
		t.Errorf("expected matched=true for aud match with bad sig, got matched=false")
	}
	if err == nil {
		t.Errorf("expected error for bad signature, got nil")
	}
	if claims != nil {
		t.Errorf("expected nil claims for failed verify, got: %+v", claims)
	}
}

// TestVerifier_ExpiredToken verifies that an expired token with aud==Audience
// returns matched==true and a non-nil error.
func TestVerifier_ExpiredToken(t *testing.T) {
	signer := newTestSigner(t)
	v := NewVerifier(signer)

	token := mintTestToken(t, signer, map[string]interface{}{
		"aud": Audience,
		"sub": "schedule:default/my-schedule",
		"exp": time.Now().Add(-time.Hour).Unix(),
	})

	claims, matched, err := v.Verify(context.Background(), token)
	if !matched {
		t.Errorf("expected matched=true for aud match with expired, got matched=false")
	}
	if err == nil {
		t.Errorf("expected error for expired token, got nil")
	}
	if claims != nil {
		t.Errorf("expected nil claims for expired token, got: %+v", claims)
	}
}

// TestVerifier_ValidToken verifies that a valid token with aud==Audience
// returns matched==true with all four Claims fields populated identically.
func TestVerifier_ValidToken(t *testing.T) {
	signer := newTestSigner(t)
	v := NewVerifier(signer)

	token := mintTestToken(t, signer, map[string]interface{}{
		"aud": Audience,
		"sub": "schedule:default/my-schedule",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	claims, matched, err := v.Verify(context.Background(), token)
	if !matched {
		t.Errorf("expected matched=true for valid token, got matched=false")
	}
	if err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
	if claims == nil {
		t.Fatal("expected non-nil claims, got nil")
	}
	expected := "schedule:default/my-schedule"
	if claims.Subject != expected {
		t.Errorf("Subject = %q, want %q", claims.Subject, expected)
	}
	if claims.Email != expected {
		t.Errorf("Email = %q, want %q", claims.Email, expected)
	}
	if claims.Name != expected {
		t.Errorf("Name = %q, want %q", claims.Name, expected)
	}
	if claims.PreferredUsername != expected {
		t.Errorf("PreferredUsername = %q, want %q", claims.PreferredUsername, expected)
	}
}

// TestVerifier_NotAJWT verifies that a non-JWT string returns matched==false.
func TestVerifier_NotAJWT(t *testing.T) {
	signer := newTestSigner(t)
	v := NewVerifier(signer)

	claims, matched, err := v.Verify(context.Background(), "not-a-jwt")
	if matched {
		t.Errorf("expected matched=false for non-JWT, got matched=true")
	}
	if err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
	if claims != nil {
		t.Errorf("expected nil claims, got: %+v", claims)
	}
}

// --- helpers ---

func newTestSigner(t *testing.T) *signing.Signer {
	t.Helper()
	s, err := signing.New()
	if err != nil {
		t.Fatalf("signing.New: %v", err)
	}
	return s
}

func mintTestToken(t *testing.T, s *signing.Signer, claimsMap map[string]interface{}) string {
	t.Helper()
	token, err := s.SignJWT(claimsMap)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}
	return token
}
