package controller

import (
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/mite"
)

// TestTokenNeedsRemint_OldSignerRotated is the #302 regression on the operator
// side: a bootstrap token that is structurally valid and unexpired but was
// signed under a PREVIOUS HMAC key (as after a control-plane reinstall
// regenerated the internal CA) must be re-minted. The old format-only gate
// (IsCurrentTokenFormat) skips the signature check and would leave it in place,
// so the gateway rejects every reconnect with "invalid or expired bootstrap
// token".
func TestTokenNeedsRemint_OldSignerRotated(t *testing.T) {
	oldSigner := mite.NewTokenSigner([]byte("old-ca-hmac-key"))
	newSigner := mite.NewTokenSigner([]byte("new-ca-hmac-key"))

	tok, err := oldSigner.GenerateToken("ctrl", "ns", 15*time.Minute)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	// Sanity: the old-CA token is still current-format and unexpired, so the
	// format-only gate would NOT rotate it — the bug.
	if !mite.IsCurrentTokenFormat(tok) {
		t.Fatal("precondition: old-signer token should be current-format and unexpired")
	}

	if !tokenNeedsRemint(newSigner, tok, "ctrl", "ns") {
		t.Error("expected re-mint for a token signed under the old signer (fails current-signer validation)")
	}
}

// TestTokenNeedsRemint_CurrentSignerKept verifies a token minted by the current
// signer is left untouched, so a healthy reconnecting mite is not invalidated.
func TestTokenNeedsRemint_CurrentSignerKept(t *testing.T) {
	signer := mite.NewTokenSigner([]byte("current-ca-hmac-key"))
	tok, err := signer.GenerateToken("ctrl", "ns", 15*time.Minute)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	if tokenNeedsRemint(signer, tok, "ctrl", "ns") {
		t.Error("expected no re-mint for a token signed by the current signer")
	}
}

// TestTokenNeedsRemint_MissingOrGarbage covers the pre-existing triggers:
// an empty token and a non-v2/garbage token both force a re-mint.
func TestTokenNeedsRemint_MissingOrGarbage(t *testing.T) {
	signer := mite.NewTokenSigner([]byte("k"))

	if !tokenNeedsRemint(signer, "", "ctrl", "ns") {
		t.Error("expected re-mint for an empty token")
	}
	if !tokenNeedsRemint(signer, "not-a-real-token", "ctrl", "ns") {
		t.Error("expected re-mint for a malformed/non-v2 token")
	}
}

// TestTokenNeedsRemint_Expired: an expired token must be rotated — this is the
// slow-init case where plugins-init outlives the 15-minute TTL and the mite's
// first-ever register presents a dead token.
func TestTokenNeedsRemint_Expired(t *testing.T) {
	signer := mite.NewTokenSigner([]byte("k"))
	tok, err := signer.GenerateToken("ctrl", "ns", -time.Minute)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if !tokenNeedsRemint(signer, tok, "ctrl", "ns") {
		t.Error("expected re-mint for an expired token")
	}
}
