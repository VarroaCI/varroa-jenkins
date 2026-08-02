package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// UserResourceName derives the deterministic Kubernetes object name for a User
// CRD from the authenticated claims. In local mode the name is the username
// (which equals the subject). In OIDC mode the name is "oidc-" followed by
// the first 32 hex characters of sha256(subject).
//
// Email is never used as the object name — the result is always stable across
// email changes. Returns empty string when claims is nil.
func UserResourceName(claims *auth.Claims, mode auth.AuthMode) string {
	if claims == nil {
		return ""
	}
	if mode == auth.AuthModeLocal {
		return claims.Subject
	}
	// OIDC: deterministic hash of the stable subject claim.
	h := sha256.Sum256([]byte(claims.Subject))
	return "oidc-" + hex.EncodeToString(h[:])[:32]
}

// dns1123LabelRe matches valid RFC 1123 DNS labels: 1-63 chars, lowercase
// alphanumeric or '-', must start and end with alphanumeric.
var dns1123LabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidateLocalUsername checks that a local-mode username is a valid DNS-1123
// label. Returns nil if valid, otherwise a descriptive error.
func ValidateLocalUsername(name string) error {
	if name == "" {
		return fmt.Errorf("username must not be empty")
	}
	if strings.HasPrefix(name, "oidc-") {
		return fmt.Errorf("username must not start with 'oidc-'")
	}
	if len(name) > 63 {
		return fmt.Errorf("username must be at most 63 characters, got %d", len(name))
	}
	if !dns1123LabelRe.MatchString(name) {
		return fmt.Errorf("username %q is not a valid DNS-1123 label (lowercase alphanumeric and '-')", name)
	}
	return nil
}

// IsOIDCName reports whether name is an OIDC-style hashed CRD name
// (prefix "oidc-"). Used as a heuristic, not a strict validation.
func IsOIDCName(name string) bool {
	return strings.HasPrefix(name, "oidc-")
}
