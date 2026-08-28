// Package ldap provides an LDAP/Active-Directory authentication provider.
//
// The provider performs an LDAP bind (simple or search-then-bind) to
// authenticate users, resolves group membership via an LDAP search, and
// issues RS256 JWTs using the shared [signing.Signer] — exactly like the
// local auth provider.  The Jenkins plugin (VarroaSecurityRealm) remains
// unchanged; it validates tokens against the JWKS endpoint served by this
// provider.
//
// Two bind modes are supported:
//
//   - Direct bind: the caller supplies a BindDNTemplate such as
//     "uid=%s,ou=people,dc=example,dc=com".  The username replaces %s and
//     the provider attempts a single bind with the given password.
//
//   - Search-then-bind: a service account first binds to the directory to
//     search for the user DN (useful when the DN cannot be derived from the
//     username alone, e.g. Active Directory).  After the user DN is found
//     the provider performs a second bind with the end-user password.
//
// Group membership is resolved via an LDAP search whose filter receives the
// authenticated user DN.  The CN of each matching group entry is included in
// the JWT "groups" claim.  The Varroa RBAC system matches those claim values
// against VarroaRoleBinding subjects, so the admin must create Group CRDs
// whose names match the LDAP group CNs.
package ldap

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	goldap "github.com/go-ldap/ldap/v3"
	"golang.org/x/time/rate"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/signing"
)

// Sentinel errors returned by the LDAP Provider.
var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrRateLimited        = errors.New("rate limited")
	ErrExpiredToken       = errors.New("token expired")
)

// UserSearchConfig configures search-then-bind user lookup.
type UserSearchConfig struct {
	// BaseDN is the search base, e.g. "ou=people,dc=example,dc=com".
	BaseDN string
	// Filter is an LDAP search filter with a single %s placeholder for the
	// username, e.g. "(uid=%s)" or "(&(objectClass=user)(sAMAccountName=%s))".
	Filter string
	// EmailAttr is the attribute that contains the user's email address.
	// Defaults to "mail" when empty.
	EmailAttr string
	// NameAttr is the attribute that contains the user's display name.
	// Defaults to "cn" when empty.
	NameAttr string
}

// GroupSearchConfig configures LDAP group membership lookup.
type GroupSearchConfig struct {
	// BaseDN is the search base, e.g. "ou=groups,dc=example,dc=com".
	BaseDN string
	// Filter is an LDAP search filter with a single %s placeholder for the
	// authenticated user's DN, e.g. "(member=%s)" or
	// "(memberOf=%s)".
	Filter string
	// NameAttr is the attribute that provides the Varroa group name.
	// Defaults to "cn" when empty.
	NameAttr string
}

// Config holds the LDAP connection and search settings.
type Config struct {
	// URL is the LDAP server URL, e.g. "ldap://ldap.example.com:389" or
	// "ldaps://ldap.example.com:636".
	URL string

	// BindDNTemplate is a printf template for direct (simple) bind:
	// "uid=%s,ou=people,dc=example,dc=com".  The literal "%s" is replaced
	// with the username.  Set to a non-empty string to use direct-bind mode.
	// Mutually exclusive with UserSearch.
	BindDNTemplate string

	// ServiceAccountDN is the DN used for the initial bind in search-then-bind
	// mode.  Leave empty for anonymous search (not recommended).
	ServiceAccountDN string
	// ServiceAccountPassword is the password for ServiceAccountDN.
	ServiceAccountPassword string

	// UserSearch configures search-then-bind mode.  Used when BindDNTemplate
	// is empty.
	UserSearch *UserSearchConfig

	// GroupSearch configures LDAP group membership resolution.  Optional: when
	// nil no group claim is added.
	GroupSearch *GroupSearchConfig

	// StartTLS upgrades a plain ldap:// connection to TLS via STARTTLS before
	// sending any credentials.
	StartTLS bool

	// InsecureSkipVerify disables TLS certificate validation.  Only for
	// testing / development.
	InsecureSkipVerify bool

	// CACert is an optional PEM CA bundle used to verify the LDAP server
	// certificate. When set, TLS verification is enabled and pinned to this CA
	// (preferred over InsecureSkipVerify).
	CACert []byte
}

// dialer is a function that opens an LDAP connection.  The real implementation
// dials the configured server; tests inject a mock.
type dialer func(cfg Config) (ldapConn, error)

// ldapConn is the subset of [goldap.Conn] used by the provider.
type ldapConn interface {
	Bind(username, password string) error
	Search(request *goldap.SearchRequest) (*goldap.SearchResult, error)
	Close() error
}

// Provider is the LDAP authentication provider.
//
// It authenticates users against an LDAP directory, resolves group
// membership, and signs RS256 JWTs — no external IdP required.
type Provider struct {
	signer       *signing.Signer
	cfg          Config
	dial         dialer
	issuerURL    string
	clientID     string
	ttl          time.Duration
	cookieDomain string
	limiter      *loginLimiter
}

// New creates a new LDAP Provider.
func New(signer *signing.Signer, cfg Config, issuerURL, clientID string, ttl time.Duration, cookieDomain string) *Provider {
	if cfg.InsecureSkipVerify && len(cfg.CACert) == 0 {
		slog.Default().Warn("LDAP TLS certificate verification is DISABLED (--ldap-insecure-skip-verify) — this is unsafe; supply --ldap-ca-cert-file instead", "url", cfg.URL)
	}
	return &Provider{
		signer:       signer,
		cfg:          cfg,
		dial:         realDial,
		issuerURL:    issuerURL,
		clientID:     clientID,
		ttl:          ttl,
		cookieDomain: cookieDomain,
		limiter:      newLoginLimiter(),
	}
}

// --- auth.Provider interface ---

// Mode returns auth.AuthModeLDAP.
func (p *Provider) Mode() auth.AuthMode { return auth.AuthModeLDAP }

// CookieDomain returns the configured cookie domain.
func (p *Provider) CookieDomain() string { return p.cookieDomain }

// Discovery returns the OpenID provider metadata and JWKS.
// ok is always true for LDAP mode (the provider issues its own JWTs).
func (p *Provider) Discovery() ([]byte, []byte, bool) {
	oidcCfg := map[string]any{
		"issuer":                                p.issuerURL,
		"jwks_uri":                              p.issuerURL + "/.well-known/jwks.json",
		"response_types_supported":              []string{"id_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
	cfgJSON, _ := json.Marshal(oidcCfg)
	jwks := p.signer.JWKS()
	jwksJSON, _ := json.Marshal(jwks)
	return cfgJSON, jwksJSON, true
}

// --- Login flow ---

// Login authenticates a user against the LDAP directory.
//
// On success it returns a signed RS256 id_token and the TTL in seconds.
// On failure it returns ErrInvalidCredentials (same error for unknown user
// and wrong password — no user enumeration) or ErrRateLimited.
func (p *Provider) Login(_ context.Context, username, password string) (idToken string, expiresIn int, err error) {
	if !p.limiter.Allow(username) {
		return "", 0, ErrRateLimited
	}
	if password == "" {
		// Reject empty passwords — LDAP servers may accept an anonymous bind
		// when the password is empty.
		return "", 0, ErrInvalidCredentials
	}

	conn, err := p.dial(p.cfg)
	if err != nil {
		return "", 0, fmt.Errorf("ldap dial: %w", err)
	}
	defer conn.Close() //nolint:errcheck

	userDN, email, name, err := p.authenticateUser(conn, username, password)
	if err != nil {
		return "", 0, err
	}

	groups, err := p.resolveGroups(conn, userDN)
	if err != nil {
		return "", 0, fmt.Errorf("ldap group resolution: %w", err)
	}

	now := time.Now()
	claims := map[string]any{
		"iss":                p.issuerURL,
		"sub":                username,
		"email":              email,
		"name":               name,
		"preferred_username": username,
		"groups":             groups,
		"iat":                now.Unix(),
		"exp":                now.Add(p.ttl).Unix(),
		"aud":                p.clientID,
	}

	token, err := p.signer.SignJWT(claims)
	if err != nil {
		return "", 0, fmt.Errorf("sign jwt: %w", err)
	}

	return token, int(p.ttl.Seconds()), nil
}

// authenticateUser performs the LDAP bind and returns the user DN, email, and
// display name.
func (p *Provider) authenticateUser(conn ldapConn, username, password string) (dn, email, name string, err error) {
	if p.cfg.BindDNTemplate != "" {
		return p.directBind(conn, username, password)
	}
	return p.searchBind(conn, username, password)
}

// directBind performs a simple bind using the BindDNTemplate.
func (p *Provider) directBind(conn ldapConn, username, password string) (dn, email, name string, err error) {
	dn = fmt.Sprintf(p.cfg.BindDNTemplate, goldap.EscapeDN(username))
	if err := conn.Bind(dn, password); err != nil {
		// Any bind error (wrong DN, wrong password, account disabled) is
		// returned as ErrInvalidCredentials to avoid leaking details.
		return "", "", "", ErrInvalidCredentials
	}

	// Try to read email / display name from the entry.
	email, name = p.lookupUserAttributes(conn, dn)
	return dn, email, name, nil
}

// searchBind performs a service-account search for the user DN and then binds
// as that user.
func (p *Provider) searchBind(conn ldapConn, username, password string) (dn, email, name string, err error) {
	us := p.cfg.UserSearch
	if us == nil {
		// Neither BindDNTemplate nor UserSearch is configured.
		return "", "", "", fmt.Errorf("ldap: no bind mode configured (set BindDNTemplate or UserSearch)")
	}

	// Initial bind with service account (or anonymous).
	if p.cfg.ServiceAccountDN != "" {
		if err := conn.Bind(p.cfg.ServiceAccountDN, p.cfg.ServiceAccountPassword); err != nil {
			return "", "", "", fmt.Errorf("ldap service account bind: %w", err)
		}
	}

	emailAttr := cond(us.EmailAttr, "mail")
	nameAttr := cond(us.NameAttr, "cn")

	filter := fmt.Sprintf(us.Filter, goldap.EscapeFilter(username))
	req := goldap.NewSearchRequest(
		us.BaseDN,
		goldap.ScopeWholeSubtree, goldap.NeverDerefAliases, 1, 0, false,
		filter,
		[]string{"dn", emailAttr, nameAttr},
		nil,
	)
	sr, err := conn.Search(req)
	if err != nil {
		return "", "", "", fmt.Errorf("ldap user search: %w", err)
	}
	if len(sr.Entries) == 0 {
		return "", "", "", ErrInvalidCredentials
	}

	entry := sr.Entries[0]
	dn = entry.DN
	email = entry.GetAttributeValue(emailAttr)
	name = entry.GetAttributeValue(nameAttr)

	// Bind as the found user.
	if err := conn.Bind(dn, password); err != nil {
		return "", "", "", ErrInvalidCredentials
	}

	return dn, email, name, nil
}

// lookupUserAttributes reads email and display name for a directly-bound user.
// This requires a search after binding; failure is non-fatal (returns empty strings).
func (p *Provider) lookupUserAttributes(conn ldapConn, dn string) (email, name string) {
	us := p.cfg.UserSearch
	if us == nil || us.BaseDN == "" {
		return "", ""
	}

	emailAttr := cond(us.EmailAttr, "mail")
	nameAttr := cond(us.NameAttr, "cn")

	req := goldap.NewSearchRequest(
		dn,
		goldap.ScopeBaseObject, goldap.NeverDerefAliases, 1, 0, false,
		"(objectClass=*)",
		[]string{emailAttr, nameAttr},
		nil,
	)
	sr, err := conn.Search(req)
	if err != nil || len(sr.Entries) == 0 {
		// Best-effort: return empty strings on any failure.
		return "", ""
	}
	return sr.Entries[0].GetAttributeValue(emailAttr), sr.Entries[0].GetAttributeValue(nameAttr)
}

// resolveGroups returns the LDAP group names the user belongs to.
// Returns an empty (non-nil) slice when GroupSearch is not configured.
func (p *Provider) resolveGroups(conn ldapConn, userDN string) ([]string, error) {
	gs := p.cfg.GroupSearch
	if gs == nil || gs.BaseDN == "" || gs.Filter == "" {
		return []string{}, nil
	}

	nameAttr := cond(gs.NameAttr, "cn")
	filter := fmt.Sprintf(gs.Filter, goldap.EscapeFilter(userDN))
	req := goldap.NewSearchRequest(
		gs.BaseDN,
		goldap.ScopeWholeSubtree, goldap.NeverDerefAliases, 0, 0, false,
		filter,
		[]string{nameAttr},
		nil,
	)
	sr, err := conn.Search(req)
	if err != nil {
		return nil, fmt.Errorf("ldap group search: %w", err)
	}

	groups := make([]string, 0, len(sr.Entries))
	for _, e := range sr.Entries {
		if v := e.GetAttributeValue(nameAttr); v != "" {
			groups = append(groups, v)
		}
	}
	return groups, nil
}

// --- Token verification ---

type ldapJWTHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

type ldapClaims struct {
	Iss               string   `json:"iss"`
	Sub               string   `json:"sub"`
	Aud               string   `json:"aud"`
	Exp               int64    `json:"exp"`
	Iat               int64    `json:"iat"`
	Email             string   `json:"email"`
	Name              string   `json:"name"`
	PreferredUsername string   `json:"preferred_username"`
	Groups            []string `json:"groups"`
}

// Verify validates an LDAP-issued RS256 JWT.
//
// It checks the signature, then rejects unless iss == issuerURL,
// aud == clientID, and exp > now.
func (p *Provider) Verify(_ context.Context, rawToken string) (*auth.Claims, error) {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid token: expected 3 parts, got %d", len(parts))
	}

	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode header: %w", err)
	}
	var hdr ldapJWTHeader
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		return nil, fmt.Errorf("unmarshal header: %w", err)
	}
	if hdr.Alg != "RS256" {
		return nil, fmt.Errorf("unsupported algorithm: %s", hdr.Alg)
	}
	if hdr.Kid != p.signer.KID() {
		return nil, fmt.Errorf("unknown kid: %s", hdr.Kid)
	}

	signingInput := parts[0] + "." + parts[1]
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}
	pub := p.signer.PublicKey()
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, hashed[:], sig); err != nil {
		return nil, fmt.Errorf("signature verification failed: %w", err)
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode claims: %w", err)
	}
	var lc ldapClaims
	if err := json.Unmarshal(claimsJSON, &lc); err != nil {
		return nil, fmt.Errorf("unmarshal claims: %w", err)
	}

	if lc.Iss != p.issuerURL {
		return nil, fmt.Errorf("unexpected issuer: %s", lc.Iss)
	}
	if lc.Aud != p.clientID {
		return nil, fmt.Errorf("unexpected audience: %s", lc.Aud)
	}
	if time.Now().Unix() > lc.Exp {
		return nil, ErrExpiredToken
	}

	return &auth.Claims{
		Subject:           lc.Sub,
		Email:             lc.Email,
		Name:              lc.Name,
		PreferredUsername: lc.PreferredUsername,
		Groups:            lc.Groups,
	}, nil
}

// --- LDAP connection helpers ---

// realDial opens a real LDAP connection to the server described by cfg.
func realDial(cfg Config) (ldapConn, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse ldap url %q: %w", cfg.URL, err)
	}

	tlsCfg, err := tlsConfig(cfg.InsecureSkipVerify, cfg.CACert, u.Hostname())
	if err != nil {
		return nil, err
	}
	if cfg.InsecureSkipVerify && len(cfg.CACert) == 0 {
		slog.Default().Warn("LDAP TLS certificate verification is DISABLED — credentials may be exposed to a MITM; supply --ldap-ca-cert-file", "url", cfg.URL)
	}

	var conn *goldap.Conn
	switch strings.ToLower(u.Scheme) {
	case "ldaps":
		conn, err = goldap.DialURL(cfg.URL, goldap.DialWithTLSConfig(tlsCfg))
	case "ldap":
		conn, err = goldap.DialURL(cfg.URL)
		if err == nil && cfg.StartTLS {
			err = conn.StartTLS(tlsCfg)
		}
	default:
		return nil, fmt.Errorf("unsupported LDAP scheme %q (use ldap:// or ldaps://)", u.Scheme)
	}
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// --- Rate limiter (per-username, 5 attempts per minute) ---

// Login rate-limiting policy: 5 attempts per minute per username.
const (
	loginBurstSize    = 5
	loginRateInterval = time.Minute
)

type loginLimiter struct {
	mu     sync.Mutex
	limits map[string]*rate.Limiter
	stop   chan struct{}
}

func newLoginLimiter() *loginLimiter {
	l := &loginLimiter{
		limits: make(map[string]*rate.Limiter),
		stop:   make(chan struct{}),
	}
	go l.cleanupLoop()
	return l
}

func (l *loginLimiter) cleanupLoop() {
	ticker := time.NewTicker(loginRateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			l.cleanup()
		case <-l.stop:
			return
		}
	}
}

func (l *loginLimiter) cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for user, lim := range l.limits {
		if lim.Tokens() >= float64(loginBurstSize) {
			delete(l.limits, user)
		}
	}
}

func (l *loginLimiter) Allow(username string) bool {
	l.mu.Lock()
	lim, ok := l.limits[username]
	if !ok {
		lim = rate.NewLimiter(rate.Every(loginRateInterval/loginBurstSize), loginBurstSize)
		l.limits[username] = lim
	}
	l.mu.Unlock()
	return lim.Allow()
}

// --- Helpers ---

// cond returns v when non-empty, otherwise fallback.
func cond(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

// tlsConfig builds a *tls.Config for the given server name.
// tlsConfig builds the TLS config for LDAPS/StartTLS. When a CA bundle is supplied,
// verification is ON and pinned to that private CA. Only when no CA is given AND
// insecureSkipVerify is set is verification disabled (the unsafe escape hatch). With
// neither, it falls back to the system trust store with verification ON (fail-closed).
func tlsConfig(insecureSkipVerify bool, caCert []byte, serverName string) (*tls.Config, error) {
	cfg := &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	switch {
	case len(caCert) > 0:
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("ldap: no valid certificates in CA bundle")
		}
		cfg.RootCAs = pool
	case insecureSkipVerify:
		cfg.InsecureSkipVerify = true //nolint:gosec // opt-in, documented unsafe escape hatch
	default:
		// system trust store; verification ON (fail-closed on unknown CA)
	}
	return cfg, nil
}
