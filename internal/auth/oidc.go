package auth

import (
	"context"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Claims holds the OIDC token claims we care about.
type Claims struct {
	Subject           string   `json:"sub"`
	Email             string   `json:"email"`
	Name              string   `json:"name"`
	PreferredUsername string   `json:"preferred_username"`
	Groups            []string `json:"groups"`
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

// GroupValues returns the values of the configured group claims.
// Used by the resolver to build subject keys for kind:Group bindings.
func (c *Claims) GroupValues(claimNames []string) []string {
	var vals []string
	seen := map[string]bool{}
	for _, name := range claimNames {
		for _, v := range c.groupClaim(name) {
			if v != "" && !seen[v] {
				seen[v] = true
				vals = append(vals, v)
			}
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
		return ""
	}
}

func (c *Claims) groupClaim(name string) []string {
	switch name {
	case "groups":
		return c.Groups
	default:
		return nil
	}
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
		provider:  provider,
		verifier:  verifier,
		oauth2Cfg: oauth2Cfg,
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

	claims := &Claims{}
	if err := idToken.Claims(claims); err != nil {
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
	claims := &Claims{}
	if err := idToken.Claims(claims); err != nil {
		return nil, err
	}
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

// Verify validates a raw token and returns claims (quiet variant).
func (v *Validator) Verify(ctx context.Context, rawToken string) (*Claims, error) {
	return v.VerifyTokenQuiet(ctx, rawToken)
}

// CookieDomain returns the configured cookie domain.
func (v *Validator) CookieDomain() string { return v.cookieDomain }

// Discovery returns nil, nil, false — OIDC mode delegates discovery to the
// external identity provider.
func (v *Validator) Discovery() ([]byte, []byte, bool) { return nil, nil, false }
