package local

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/signing"
)

// Sentinel errors returned by the local auth Provider.
var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrRateLimited        = errors.New("rate limited")
	ErrExpiredToken       = errors.New("token expired")
)

// userStore abstracts the CRD operations needed by the Provider.
// It is satisfied by [controller.ResourceClient].
type userStore interface {
	GetUserCRD(ctx context.Context, name, ns string) (*v1alpha1.User, error)
	ListGroupCRDs(ctx context.Context) ([]*v1alpha1.Group, error)
	PatchUserStatus(ctx context.Context, name, ns string, st *v1alpha1.UserStatus) error
	ApplyUserCRD(ctx context.Context, u *v1alpha1.User) error
}

// Provider is the local authentication provider.
//
// It signs RS256 JWTs directly (no external IdP), validates credentials
// against bcrypt hashes stored in the User CRD, and serves its own
// OpenID discovery and JWKS endpoints.
type Provider struct {
	signer       *signing.Signer
	store        userStore
	ns           string
	issuerURL    string
	clientID     string
	ttl          time.Duration
	cookieDomain string
	limiter      *loginLimiter
}

// New creates a new local Provider.
func New(signer *signing.Signer, store userStore, ns, issuerURL, clientID string, ttl time.Duration, cookieDomain string) *Provider {
	return &Provider{
		signer:       signer,
		store:        store,
		ns:           ns,
		issuerURL:    issuerURL,
		clientID:     clientID,
		ttl:          ttl,
		cookieDomain: cookieDomain,
		limiter:      newLoginLimiter(),
	}
}

// --- auth.Provider interface ---

// Mode returns auth.AuthModeLocal.
func (p *Provider) Mode() auth.AuthMode { return auth.AuthModeLocal }

// CookieDomain returns the configured cookie domain.
func (p *Provider) CookieDomain() string { return p.cookieDomain }

// Discovery returns the OpenID provider metadata and JWKS.
// ok is always true for local mode.
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

// Login authenticates a user by username and password.
//
// On success it returns a signed RS256 id_token and the lifetime in
// seconds. On failure it returns ErrInvalidCredentials (same error for
// unknown user and wrong password — no user enumeration) or
// ErrRateLimited.
func (p *Provider) Login(ctx context.Context, username, password string) (idToken string, expiresIn int, err error) {
	if !p.limiter.Allow(username) {
		return "", 0, ErrRateLimited
	}

	// Fetch the User CRD. A missing user returns the same opaque error as a
	// wrong password to avoid user enumeration (and to keep the bcrypt path
	// off the timing side-channel only when the user genuinely doesn't exist).
	u, err := p.store.GetUserCRD(ctx, username, p.ns)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", 0, ErrInvalidCredentials
		}
		return "", 0, fmt.Errorf("get user %s: %w", username, err)
	}
	if u == nil || u.Status.Credentials == nil || u.Status.Credentials.PasswordHash == "" {
		return "", 0, ErrInvalidCredentials
	}

	// Compare password hash.
	if err := bcrypt.CompareHashAndPassword([]byte(u.Status.Credentials.PasswordHash), []byte(password)); err != nil {
		return "", 0, ErrInvalidCredentials
	}

	// Resolve group memberships.
	groups, err := p.resolveGroups(ctx, username)
	if err != nil {
		return "", 0, fmt.Errorf("resolve groups: %w", err)
	}

	// Build claims and sign.
	now := time.Now()
	claims := map[string]any{
		"iss":                p.issuerURL,
		"sub":                username,
		"email":              u.Spec.Email,
		"name":               u.Spec.DisplayName,
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

	// Best-effort update last login timestamp.
	p.patchLastLogin(ctx, username)

	// Ensure the user carries the managed-by:local label (heals unlabeled
	// pre-existing users on their next login).
	p.ensureLocalLabel(ctx, u)

	return token, int(p.ttl.Seconds()), nil
}

// resolveGroups returns the names of all groups that include username.
func (p *Provider) resolveGroups(ctx context.Context, username string) ([]string, error) {
	all, err := p.store.ListGroupCRDs(ctx)
	if err != nil {
		return nil, err
	}
	var groups []string
	for _, g := range all {
		for _, m := range g.Spec.Members {
			if m == username {
				groups = append(groups, g.Name)
				break
			}
		}
	}
	return groups, nil
}

// patchLastLogin is a best-effort update of the user's last login timestamp.
func (p *Provider) patchLastLogin(ctx context.Context, username string) {
	now := metav1.Now()
	_ = p.store.PatchUserStatus(ctx, username, p.ns, &v1alpha1.UserStatus{
		LastLogin: &now,
	})
}

// ensureLocalLabel stamps managed-by:local on a user that lacks the label
// (healing pre-existing users on their next successful login).
func (p *Provider) ensureLocalLabel(ctx context.Context, u *v1alpha1.User) {
	if u == nil {
		return
	}
	if u.Labels != nil && u.Labels[v1alpha1.LabelManagedBy] == v1alpha1.ManagedByLocal {
		return
	}
	if u.Labels == nil {
		u.Labels = map[string]string{}
	}
	u.Labels[v1alpha1.LabelManagedBy] = v1alpha1.ManagedByLocal
	_ = p.store.ApplyUserCRD(ctx, u)
}

// --- Token verification ---

type localJWTHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

type localClaims struct {
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

// Verify validates a local-auth RS256 JWT.
//
// It checks the signature, then rejects unless iss == issuerURL,
// aud == clientID, and exp > now. This ensures mite tokens
// (iss=varroa-operator) are rejected even though they share the same
// signing key.
func (p *Provider) Verify(ctx context.Context, rawToken string) (*auth.Claims, error) {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid token: expected 3 parts, got %d", len(parts))
	}

	// Decode header to get KID.
	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode header: %w", err)
	}
	var hdr localJWTHeader
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		return nil, fmt.Errorf("unmarshal header: %w", err)
	}
	if hdr.Alg != "RS256" {
		return nil, fmt.Errorf("unsupported algorithm: %s", hdr.Alg)
	}
	if hdr.Kid != p.signer.KID() {
		return nil, fmt.Errorf("unknown kid: %s", hdr.Kid)
	}

	// Verify signature.
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

	// Decode claims.
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode claims: %w", err)
	}
	var lc localClaims
	if err := json.Unmarshal(claimsJSON, &lc); err != nil {
		return nil, fmt.Errorf("unmarshal claims: %w", err)
	}

	// Enforce issuer/audience/expiry.
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

// --- Password helpers (used by API handlers in Phase 6) ---

const bcryptCost = 12

// SetPassword hashes a password and persists it via PatchUserStatus.
func (p *Provider) SetPassword(ctx context.Context, username, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	now := metav1.Now()
	return p.store.PatchUserStatus(ctx, username, p.ns, &v1alpha1.UserStatus{
		Credentials: &v1alpha1.UserCredentials{
			PasswordHash: string(hash),
			LastChanged:  &now,
		},
	})
}

// ChangePassword verifies the old password and sets the new one.
func (p *Provider) ChangePassword(ctx context.Context, username, oldPassword, newPassword string) error {
	u, err := p.store.GetUserCRD(ctx, username, p.ns)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ErrInvalidCredentials
		}
		return fmt.Errorf("get user: %w", err)
	}
	if u == nil || u.Status.Credentials == nil || u.Status.Credentials.PasswordHash == "" {
		return ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.Status.Credentials.PasswordHash), []byte(oldPassword)); err != nil {
		return ErrInvalidCredentials
	}
	return p.SetPassword(ctx, username, newPassword)
}
