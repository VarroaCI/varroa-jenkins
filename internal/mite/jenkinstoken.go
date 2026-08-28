package mite

import (
	"fmt"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/signing"
)

// MiteTokenSigner creates RS256 JWT tokens for the mite sidecar to
// authenticate with Jenkins. The operator holds the private key; the
// Jenkins plugin verifies with the public key. Name is intentional
// to distinguish from the HMAC TokenSigner.
//
// MiteTokenSigner wraps [signing.Signer] so it shares the same KID
// as the local-auth provider when both use the same keypair.
//
//nolint:revive
type MiteTokenSigner struct {
	signer *signing.Signer
}

type miteClaims struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"`
	Aud string `json:"aud"`
	Exp int64  `json:"exp"`
	Iat int64  `json:"iat"`
}

type userClaims struct {
	Iss               string   `json:"iss"`
	Sub               string   `json:"sub"`
	VarroaType        string   `json:"varroa_typ"`
	PreferredUsername string   `json:"preferred_username,omitempty"`
	Email             string   `json:"email,omitempty"`
	Name              string   `json:"name,omitempty"`
	Groups            []string `json:"groups"`
	Aud               string   `json:"aud"`
	Exp               int64    `json:"exp"`
	Iat               int64    `json:"iat"`
}

// NewMiteTokenSigner creates a MiteTokenSigner with a fresh RSA-2048 keypair.
// The KID is derived from the SHA-256 fingerprint of the public key so that
// the same key always produces the same KID across restarts.
func NewMiteTokenSigner() (*MiteTokenSigner, error) {
	s, err := signing.New()
	if err != nil {
		return nil, err
	}
	return &MiteTokenSigner{signer: s}, nil
}

// NewMiteTokenSignerFromPEM loads an existing RSA private key from PEM bytes.
// The KID is derived from the public key fingerprint so it is stable across
// operator restarts when the key is persisted.
func NewMiteTokenSignerFromPEM(privPEM []byte) (*MiteTokenSigner, error) {
	s, err := signing.NewFromPEM(privPEM)
	if err != nil {
		return nil, err
	}
	return &MiteTokenSigner{signer: s}, nil
}

// PrivateKeyPEM exports the RSA private key as PKCS#8 PEM for storage
// in a Kubernetes Secret.
func (s *MiteTokenSigner) PrivateKeyPEM() (string, error) {
	return s.signer.PrivateKeyPEM()
}

// PublicKeyPEM returns the RSA public key in SubjectPublicKeyInfo PEM format.
func (s *MiteTokenSigner) PublicKeyPEM() string {
	return s.signer.PublicKeyPEM()
}

// KID returns the key ID for this signer.
func (s *MiteTokenSigner) KID() string {
	return s.signer.KID()
}

// Signer returns the underlying shared signing.Signer so callers
// can issue their own tokens with the same key material (same KID).
func (s *MiteTokenSigner) Signer() *signing.Signer {
	return s.signer
}

// GenerateMiteJenkinsToken creates a signed RS256 JWT:
//
//	header:  {"alg":"RS256","typ":"JWT","kid":<kid>}
//	claims:  {"iss":"varroa-operator","sub":"system:varroa-mite",
//	          "aud":"<namespace>/<name>","exp":<now+ttl>,"iat":<now>}
func (s *MiteTokenSigner) GenerateMiteJenkinsToken(name, ns string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := miteClaims{
		Iss: "varroa-operator",
		Sub: "system:varroa-mite",
		Aud: ns + "/" + name,
		Exp: now.Add(ttl).Unix(),
		Iat: now.Unix(),
	}

	tok, err := s.signer.SignJWT(claims)
	if err != nil {
		return "", fmt.Errorf("sign mite jenkins token: %w", err)
	}
	return tok, nil
}

// GenerateOperatorJenkinsToken creates a signed RS256 JWT for the operator's
// own direct-to-Jenkins identity (distinct from the mite identity):
//
//	header:  {"alg":"RS256","typ":"JWT","kid":<kid>}
//	claims:  {"iss":"varroa-operator","sub":"system:varroa-operator",
//	          "aud":"<namespace>/<name>","exp":<now+ttl>,"iat":<now>}
func (s *MiteTokenSigner) GenerateOperatorJenkinsToken(name, ns string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := miteClaims{
		Iss: "varroa-operator",
		Sub: "system:varroa-operator",
		Aud: ns + "/" + name,
		Exp: now.Add(ttl).Unix(),
		Iat: now.Unix(),
	}

	tok, err := s.signer.SignJWT(claims)
	if err != nil {
		return "", fmt.Errorf("sign operator jenkins token: %w", err)
	}
	return tok, nil
}

// GenerateUserJenkinsToken creates a signed RS256 JWT for a caller's Jenkins identity.
func (s *MiteTokenSigner) GenerateUserJenkinsToken(name, ns string, c *auth.Claims, ttl time.Duration) (string, error) {
	if c == nil || c.Subject == "" {
		return "", fmt.Errorf("user token requires a subject")
	}

	now := time.Now()
	claims := userClaims{
		Iss:               "varroa-operator",
		Sub:               c.Subject,
		VarroaType:        "user",
		PreferredUsername: c.PreferredUsername,
		Email:             c.Email,
		Name:              c.Name,
		Groups:            c.Groups,
		Aud:               ns + "/" + name,
		Exp:               now.Add(ttl).Unix(),
		Iat:               now.Unix(),
	}

	tok, err := s.signer.SignJWT(claims)
	if err != nil {
		return "", fmt.Errorf("sign user jenkins token: %w", err)
	}
	return tok, nil
}
