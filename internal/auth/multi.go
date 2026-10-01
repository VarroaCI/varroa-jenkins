package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// LocalSubjectPrefix starts the subject and the preferred username of a
// local account that signs in beside OIDC. The prefix keeps a local account
// and an IdP user of the same name as two separate subjects.
const LocalSubjectPrefix = "local:"

// ErrLocalPrefixFromIdP is the refusal for an IdP token whose subject or
// preferred username starts with LocalSubjectPrefix.
var ErrLocalPrefixFromIdP = errors.New("identity provider subject uses the reserved local: prefix")

// IsLocalSession reports whether claims belong to a local account that
// signed in beside OIDC.
func IsLocalSession(claims *Claims) bool {
	return claims != nil && strings.HasPrefix(claims.Subject, LocalSubjectPrefix)
}

// RefuseLocalPrefix returns ErrLocalPrefixFromIdP when IdP claims use the
// prefix that only a local account beside OIDC may carry.
func RefuseLocalPrefix(claims *Claims) error {
	if claims == nil {
		return nil
	}
	if strings.HasPrefix(claims.Subject, LocalSubjectPrefix) ||
		strings.HasPrefix(claims.PreferredUsername, LocalSubjectPrefix) {
		return ErrLocalPrefixFromIdP
	}
	return nil
}

// MultiProvider accepts sessions from the OIDC issuer and from the local
// account issuer of the BFF. It selects the verifier from the unverified
// iss claim; the selected verifier then checks the signature, the issuer,
// the audience, and the expiry.
type MultiProvider struct {
	oidc        OIDCProvider
	local       Provider
	localIssuer string
}

var _ Provider = (*MultiProvider)(nil)

// NewMultiProvider returns a provider with OIDC as the primary mode and a
// local provider whose tokens carry iss == localIssuer.
func NewMultiProvider(oidc OIDCProvider, local Provider, localIssuer string) *MultiProvider {
	return &MultiProvider{oidc: oidc, local: local, localIssuer: localIssuer}
}

// OIDC returns the primary OIDC provider for the login and callback flow.
func (m *MultiProvider) OIDC() OIDCProvider { return m.oidc }

// Mode returns AuthModeOIDC: OIDC stays the primary sign-in.
func (m *MultiProvider) Mode() AuthMode { return AuthModeOIDC }

// CookieDomain returns the cookie domain of the primary provider.
func (m *MultiProvider) CookieDomain() string { return m.oidc.CookieDomain() }

// Discovery returns the discovery metadata of the local issuer, so a
// Jenkins controller can fetch the keys that sign a local session.
func (m *MultiProvider) Discovery() ([]byte, []byte, bool) { return m.local.Discovery() }

// Verify validates a session token from either issuer. An IdP token whose
// subject uses the local prefix is refused.
func (m *MultiProvider) Verify(ctx context.Context, rawToken string) (*Claims, error) {
	if m.localIssuer != "" && unverifiedIssuer(rawToken) == m.localIssuer {
		return m.local.Verify(ctx, rawToken)
	}
	claims, err := m.oidc.Verify(ctx, rawToken)
	if err != nil {
		return nil, err
	}
	if err := RefuseLocalPrefix(claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// unverifiedIssuer reads the iss claim without a signature check. The
// value only selects a verifier; it never grants trust.
func unverifiedIssuer(rawToken string) string {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Iss string `json:"iss"`
	}
	if json.Unmarshal(payload, &c) != nil {
		return ""
	}
	return c.Iss
}
