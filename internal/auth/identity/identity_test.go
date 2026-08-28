package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/auth"
)

func TestUserResourceName_NilClaims(t *testing.T) {
	if name := UserResourceName(nil, auth.AuthModeOIDC); name != "" {
		t.Errorf("expected empty string for nil claims, got %q", name)
	}
}

func TestUserResourceName_Local(t *testing.T) {
	claims := &auth.Claims{Subject: "alice", Email: "alice@example.com"}
	name := UserResourceName(claims, auth.AuthModeLocal)
	if name != "alice" {
		t.Errorf("expected local name to be subject 'alice', got %q", name)
	}
}

func TestUserResourceName_OIDCDeterministic(t *testing.T) {
	claims := &auth.Claims{Subject: "Cg0g50hZvQK6w5Px3mR8dF2eT7uG9jL4"}
	name1 := UserResourceName(claims, auth.AuthModeOIDC)
	name2 := UserResourceName(claims, auth.AuthModeOIDC)

	if name1 != name2 {
		t.Errorf("expected deterministic output, got %q and %q", name1, name2)
	}
	if !strings.HasPrefix(name1, "oidc-") {
		t.Errorf("expected OIDC name to start with 'oidc-', got %q", name1)
	}
	if len(name1) != 5+32 { // "oidc-" + 32 hex chars
		t.Errorf("expected oidc name length 37, got %d for %q", len(name1), name1)
	}
}

func TestUserResourceName_OIDCHashCorrect(t *testing.T) {
	claims := &auth.Claims{Subject: "test-subject"}
	h := sha256.Sum256([]byte(claims.Subject))
	expected := "oidc-" + hex.EncodeToString(h[:])[:32]

	name := UserResourceName(claims, auth.AuthModeOIDC)
	if name != expected {
		t.Errorf("expected %q, got %q", expected, name)
	}
}

func TestUserResourceName_EmailNeverUsed(t *testing.T) {
	claims := &auth.Claims{
		Subject:           "oid-subj-123",
		Email:             "user@example.com",
		PreferredUsername: "user1",
	}
	// For OIDC mode, the name must be derived from Subject, not Email.
	name := UserResourceName(claims, auth.AuthModeOIDC)
	if strings.Contains(strings.ToLower(name), "example") {
		t.Errorf("email should never appear in the name, got %q", name)
	}
	if strings.Contains(name, "user@example.com") {
		t.Errorf("email should never appear in the name, got %q", name)
	}

	// Verify it's derived from Subject.
	expected := UserResourceName(&auth.Claims{Subject: "oid-subj-123"}, auth.AuthModeOIDC)
	if name != expected {
		t.Errorf("expected name derived from subject, got %q (expected %q)", name, expected)
	}
}

func TestValidateLocalUsername_Valid(t *testing.T) {
	tests := []string{
		"admin",
		"alice",
		"bob-smith",
		"user-1",
		"a",
		"ab",
		"1st-user",
		strings.Repeat("a", 63),
	}
	for _, name := range tests {
		if err := ValidateLocalUsername(name); err != nil {
			t.Errorf("expected valid username %q, got error: %v", name, err)
		}
	}
}

func TestValidateLocalUsername_Invalid(t *testing.T) {
	tests := []struct {
		name string
		msg  string
	}{
		{"", "must not be empty"},
		{"oidc-abc123", "must not start with 'oidc-'"},
		{strings.Repeat("a", 64), "must be at most 63 characters"},
		{"ALICE", "not a valid DNS-1123 label"},
		{"alice_", "not a valid DNS-1123 label"},
		{"alice@example", "not a valid DNS-1123 label"},
		{"-start", "not a valid DNS-1123 label"},
		{"end-", "not a valid DNS-1123 label"},
		{"alice.", "not a valid DNS-1123 label"},
		{"al ice", "not a valid DNS-1123 label"},
	}
	for _, tc := range tests {
		err := ValidateLocalUsername(tc.name)
		if err == nil {
			t.Errorf("expected error for %q (%s), got nil", tc.name, tc.msg)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.msg)) {
			t.Errorf("expected error for %q to mention %q, got: %v", tc.name, tc.msg, err)
		}
	}
}

func TestIsOIDCName(t *testing.T) {
	if IsOIDCName("admin") {
		t.Error("expected 'admin' to not be OIDC name")
	}
	if !IsOIDCName("oidc-abc123def456") {
		t.Error("expected 'oidc-abc123def456' to be OIDC name")
	}
}
