package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Claims holds the identity of a verified credential.
//
// Groups is the resolved group set for every credential type. An IdP
// session fills it from the configured group claims at verify time. A local,
// LDAP, API-key, or operator user credential fills it from its own groups.
type Claims struct {
	Subject           string   `json:"sub"`
	Email             string   `json:"email"`
	Name              string   `json:"name"`
	PreferredUsername string   `json:"preferred_username"`
	Groups            []string `json:"-"`

	// Extra holds the configured user claims outside the four fixed fields.
	// Only an IdP session and a ticket minted from one carry it.
	Extra map[string]string `json:"-"`

	// IssuedAt is the session token issue time. It is zero when the credential has no session token.
	IssuedAt time.Time `json:"-"`
}

// UserValues returns the values of the configured user claims.
// Used by the resolver to build subject keys for kind:User bindings.
func (c *Claims) UserValues(claimNames []string) []string {
	var vals []string
	seen := map[string]bool{}
	for _, name := range claimNames {
		v := c.userClaim(name)
		if v != "" && !seen[v] {
			seen[v] = true
			vals = append(vals, v)
		}
	}
	return vals
}

func (c *Claims) userClaim(name string) string {
	switch name {
	case "sub":
		return c.Subject
	case "email":
		return c.Email
	case "name":
		return c.Name
	case "preferred_username":
		return c.PreferredUsername
	default:
		return c.Extra[name]
	}
}

// ParseClaimNames splits a comma-separated claim name list. It trims each
// name, drops empty names, and removes duplicates. It keeps the order.
func ParseClaimNames(csv string) []string {
	var names []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(csv, ",") {
		name := strings.TrimSpace(raw)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// claimStrings returns the string values of the named claims in raw. A claim
// holds a JSON array of strings or one string. Every other shape is ignored.
// A string is never split. The result keeps first-seen order without duplicates.
func claimStrings(raw map[string]any, names []string) []string {
	var vals []string
	seen := map[string]bool{}
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			vals = append(vals, v)
		}
	}
	for _, name := range names {
		switch v := raw[name].(type) {
		case string:
			add(v)
		case []any:
			for _, elem := range v {
				if s, ok := elem.(string); ok {
					add(s)
				}
			}
		}
	}
	return vals
}

// OIDCProvider is the subset of Validator used by the OIDC auth handlers.
type OIDCProvider interface {
	Provider
	AuthCodeURL(state string) string
	AuthCodeURLOpts(state string, opts ...oauth2.AuthCodeOption) string
	Exchange(ctx context.Context, code string) (*oauth2.Token, error)
	VerifyToken(ctx context.Context, rawToken string) (*Claims, error)
	EndSessionEndpoint() string
}

// compile-time check that *Validator implements OIDCProvider
var _ OIDCProvider = (*Validator)(nil)

// Validator validates OIDC tokens against a provider.
type Validator struct {
	provider           *oidc.Provider
	verifier           *oidc.IDTokenVerifier
	oauth2Cfg          oauth2.Config
	cookieDomain       string // optional domain for varroa_token cookie
	endSessionEndpoint string // cached from OIDC discovery; empty when absent
	userClaims         []string
	groupClaims        []string
}

// NewValidator creates a new OIDC validator by fetching the OIDC discovery
// document from the issuer URL.
func NewValidator(ctx context.Context, issuer, clientID, clientSecret, redirectURL, scopes string) (*Validator, error) {
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc provider %s: %w", issuer, err)
	}

	verifier := provider.Verifier(&oidc.Config{
		ClientID: clientID,
	})

	oauth2Cfg := oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       parseScopes(scopes),
	}

	v := &Validator{
		provider:    provider,
		verifier:    verifier,
		oauth2Cfg:   oauth2Cfg,
		userClaims:  []string{"preferred_username", "sub"},
		groupClaims: []string{"groups"},
	}

	// Fetch optional end_session_endpoint from provider discovery metadata.
	// go-oidc does not expose arbitrary claims, so we parse the raw document.
	var metadata struct {
		EndSessionEndpoint string `json:"end_session_endpoint"`
	}
	if err := provider.Claims(&metadata); err == nil {
		v.endSessionEndpoint = metadata.EndSessionEndpoint
	}

	return v, nil
}

// SetClaimNames sets the configured user and group claim names. An empty
// list keeps the default for that kind: preferred_username, sub for users and
// groups for groups.
func (v *Validator) SetClaimNames(userClaims, groupClaims []string) {
	if len(userClaims) > 0 {
		v.userClaims = userClaims
	}
	if len(groupClaims) > 0 {
		v.groupClaims = groupClaims
	}
}

// AuthCodeURL returns the URL to redirect the user to for authentication.
func (v *Validator) AuthCodeURL(state string) string {
	return v.oauth2Cfg.AuthCodeURL(state)
}

// AuthCodeURLOpts returns the authorization URL with additional OAuth2 options
// such as prompt and max_age.
func (v *Validator) AuthCodeURLOpts(state string, opts ...oauth2.AuthCodeOption) string {
	return v.oauth2Cfg.AuthCodeURL(state, opts...)
}

// EndSessionEndpoint returns the cached end_session_endpoint from OIDC
// discovery, or empty string if unsupported.
func (v *Validator) EndSessionEndpoint() string { return v.endSessionEndpoint }

// parseScopes splits a comma-separated scope string into a slice.
// The "openid" scope is always prepended if not already present.
func parseScopes(raw string) []string {
	if raw == "" {
		return []string{oidc.ScopeOpenID, "profile", "email"}
	}
	scopes := strings.Split(raw, ",")
	for i := range scopes {
		scopes[i] = strings.TrimSpace(scopes[i])
	}
	// Ensure openid is always present.
	hasOpenID := false
	for _, s := range scopes {
		if s == oidc.ScopeOpenID {
			hasOpenID = true
			break
		}
	}
	if !hasOpenID {
		scopes = append([]string{oidc.ScopeOpenID}, scopes...)
	}
	return scopes
}

// Exchange converts an OAuth2 authorization code into an OIDC token.
func (v *Validator) Exchange(ctx context.Context, code string) (*oauth2.Token, error) {
	return v.oauth2Cfg.Exchange(ctx, code)
}

// VerifyToken validates a raw ID token string and returns the claims.
func (v *Validator) VerifyToken(ctx context.Context, rawToken string) (*Claims, error) {
	idToken, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, fmt.Errorf("verify token: %w", err)
	}
	claims, err := v.claimsFrom(idToken)
	if err != nil {
		return nil, fmt.Errorf("parse claims: %w", err)
	}
	return claims, nil
}

// VerifyTokenQuiet is like VerifyToken but logs less detail in errors.
func (v *Validator) VerifyTokenQuiet(ctx context.Context, rawToken string) (*Claims, error) {
	idToken, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, err
	}
	return v.claimsFrom(idToken)
}

// claimsFrom reads the fixed identity fields and the configured claims of a
// verified ID token. Groups always comes from the configured group claims, so
// a groups claim that is not configured never leaks in.
func (v *Validator) claimsFrom(idToken *oidc.IDToken) (*Claims, error) {
	claims := &Claims{}
	if err := idToken.Claims(claims); err != nil {
		return nil, err
	}
	raw := map[string]any{}
	if err := idToken.Claims(&raw); err != nil {
		return nil, err
	}
	claims.Groups = claimStrings(raw, v.groupClaims)
	for _, name := range v.userClaims {
		switch name {
		case "sub", "email", "name", "preferred_username":
			continue
		}
		if s, ok := raw[name].(string); ok && s != "" {
			if claims.Extra == nil {
				claims.Extra = map[string]string{}
			}
			claims.Extra[name] = s
		}
	}
	claims.IssuedAt = idToken.IssuedAt
	return claims, nil
}

// Provider returns the underlying OIDC provider.
func (v *Validator) Provider() *oidc.Provider { return v.provider }

// OAuth2Config returns the OAuth2 configuration.
func (v *Validator) OAuth2Config() oauth2.Config { return v.oauth2Cfg }

// SetCookieDomain sets the domain for the varroa_token cookie.
func (v *Validator) SetCookieDomain(d string) { v.cookieDomain = d }

// --- Provider interface ---

// Mode returns AuthModeOIDC.
func (v *Validator) Mode() AuthMode { return AuthModeOIDC }

// Verify validates a raw token and returns claims (quiet variant). It
// refuses a subject with the local account prefix in every mode, so an IdP
// token never passes as a local session.
func (v *Validator) Verify(ctx context.Context, rawToken string) (*Claims, error) {
	claims, err := v.VerifyTokenQuiet(ctx, rawToken)
	if err != nil {
		return nil, err
	}
	if err := RefuseLocalPrefix(claims); err != nil {
		return nil, err
	}
	if ReservedSubject(claims) {
		return nil, ErrReservedSubject
	}
	return claims, nil
}

// CookieDomain returns the configured cookie domain.
func (v *Validator) CookieDomain() string { return v.cookieDomain }

// Discovery returns nil, nil, false — OIDC mode delegates discovery to the
// external identity provider.
func (v *Validator) Discovery() ([]byte, []byte, bool) { return nil, nil, false }
