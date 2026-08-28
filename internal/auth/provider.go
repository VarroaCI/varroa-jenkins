package auth

import "context"

// AuthMode is the configured authentication mode.
//
//nolint:revive // AuthMode is the canonical public name; the auth.AuthMode stutter is acceptable.
type AuthMode string

// Supported authentication modes.
const (
	AuthModeOIDC  AuthMode = "oidc"
	AuthModeLocal AuthMode = "local"
	AuthModeLDAP  AuthMode = "ldap"
)

// Provider is the authentication provider interface.
//
// The OIDC Validator and local-auth Provider both implement this,
// allowing the middleware and API handlers to be mode-agnostic.
type Provider interface {
	// Mode returns the configured auth mode.
	Mode() AuthMode

	// Verify validates a raw token string and returns claims.
	// Errors are quiet — suitable for per-request middleware use.
	Verify(ctx context.Context, rawToken string) (*Claims, error)

	// CookieDomain returns the domain for the varroa_token cookie.
	CookieDomain() string

	// Discovery returns OIDC discovery metadata and JWKS.
	// ok is false in OIDC mode (the external IdP handles discovery).
	Discovery() (openidConfig []byte, jwks []byte, ok bool)
}

// ScheduleVerifier validates BroodSchedule-triggered JWT tokens.
// It is implemented by (*schedule.Verifier) but defined here to avoid a
// circular dependency (schedule imports auth for Claims).
type ScheduleVerifier interface {
	Verify(ctx context.Context, token string) (*Claims, bool, error)
}
