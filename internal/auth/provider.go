package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

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

// TenantAgentVerifier validates a Tenant agent session token. It is implemented
// by (*tenantagent.Verifier). It reports matched=false for a token that is not
// a Tenant agent token, so the caller can try the next verifier.
type TenantAgentVerifier interface {
	Verify(r *http.Request, token string) (claims *Claims, sessionID string, matched bool, err error)
}

// ReservedSubjectPrefix starts the subject of every Tenant agent. Only the
// Tenant agent verifier may produce a subject with this prefix.
const ReservedSubjectPrefix = "agent:"

// ErrReservedSubject refuses an identity that carries the reserved agent
// prefix on a path other than the Tenant agent verifier.
var ErrReservedSubject = errors.New("identity uses the reserved agent prefix")

// ReservedSubject reports whether any identity value of the claims starts
// with the reserved prefix. A human login must not carry one, because a
// role binding on an agent subject would then match the human.
func ReservedSubject(c *Claims) bool {
	if c == nil {
		return false
	}
	for _, v := range []string{c.Subject, c.PreferredUsername, c.Email} {
		if strings.HasPrefix(v, ReservedSubjectPrefix) {
			return true
		}
	}
	return false
}

// agentSessionKey carries the session id of a Tenant agent request.
type agentSessionKey struct{}

// ContextWithAgentSession returns a context that names the Tenant agent
// session of the request.
func ContextWithAgentSession(ctx context.Context, sid string) context.Context {
	return context.WithValue(ctx, agentSessionKey{}, sid)
}

// AgentSessionFrom returns the Tenant agent session id of the request, when
// one is set.
func AgentSessionFrom(ctx context.Context) (string, bool) {
	sid, ok := ctx.Value(agentSessionKey{}).(string)
	return sid, ok && sid != ""
}
