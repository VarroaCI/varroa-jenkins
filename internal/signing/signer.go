// Package signing provides shared RSA JWT signing and JWKS primitives.
//
// Both the mite operator-JWT signer (internal/mite) and the local-auth
// provider (internal/auth/local) consume the same Signer, ensuring the
// KID is identical across all tokens issued by a given keypair.
package signing

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
)

// Signer holds an RSA-2048 private key and its stable KID.
type Signer struct {
	priv *rsa.PrivateKey
	kid  string
}

// jwtHeader is the standard JWT header for RS256-signed tokens.
type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// JWKS is a RFC 7517 JSON Web Key Set containing a single key.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// JWK is a single JSON Web Key (RSA public key).
type JWK struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// New creates a Signer with a freshly generated RSA-2048 keypair.
// The KID is derived from the SHA-256 fingerprint of the public key.
func New() (*Signer, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate rsa key: %w", err)
	}
	kid := keyFingerprint(&priv.PublicKey)
	return &Signer{priv: priv, kid: kid}, nil
}

// NewFromPEM loads an existing RSA private key from PEM bytes.
// It tries PKCS#8 first, then falls back to PKCS#1.
// The KID is derived from the public key fingerprint so it is stable
// across restarts when the key is persisted.
func NewFromPEM(privPEM []byte) (*Signer, error) {
	block, _ := pem.Decode(privPEM)
	if block == nil {
		return nil, fmt.Errorf("decode private key pem: no PEM block found")
	}
	pk, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// Try PKCS1 as fallback.
		pk, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse private key: %w", err)
		}
	}
	rsaPriv, ok := pk.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key is not an RSA private key, got %T", pk)
	}
	kid := keyFingerprint(&rsaPriv.PublicKey)
	return &Signer{priv: rsaPriv, kid: kid}, nil
}

// PrivateKeyPEM exports the RSA private key as PKCS#8 PEM.
func (s *Signer) PrivateKeyPEM() (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(s.priv)
	if err != nil {
		return "", fmt.Errorf("marshal private key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: der,
	})), nil
}

// PublicKeyPEM returns the RSA public key in SubjectPublicKeyInfo PEM format.
func (s *Signer) PublicKeyPEM() string {
	der, err := x509.MarshalPKIXPublicKey(&s.priv.PublicKey)
	if err != nil {
		return ""
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: der,
	}))
}

// KID returns the key ID for this signer.
func (s *Signer) KID() string { return s.kid }

// PublicKey returns the RSA public key for verification.
func (s *Signer) PublicKey() *rsa.PublicKey { return &s.priv.PublicKey }

// SignJWT signs an arbitrary set of claims as a compact RS256 JWS.
//
// The JWT header is {alg:RS256, typ:JWT, kid:<kid>}. claims is
// JSON-marshalled into the payload. The signing algorithm is SHA-256
// over the signing input with PKCS1v15 signature (matching the
// existing GenerateMiteJenkinsToken behaviour).
func (s *Signer) SignJWT(claims any) (string, error) {
	header := jwtHeader{Alg: "RS256", Typ: "JWT", Kid: s.kid}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("marshal header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal claims: %w", err)
	}

	hdrB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	clB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)

	signingInput := hdrB64 + "." + clB64
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.priv, crypto.SHA256, hashed[:])
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)

	return signingInput + "." + sigB64, nil
}

// JWKS returns the single-key JWKS for this signer's public key.
func (s *Signer) JWKS() JWKS {
	pub := &s.priv.PublicKey
	return JWKS{
		Keys: []JWK{{
			Kty: "RSA",
			Use: "sig",
			Alg: "RS256",
			Kid: s.kid,
			N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}},
	}
}

// keyFingerprint returns a hex-encoded SHA-256 fingerprint of the
// public key for use as a stable KID. The fingerprint is the first
// 8 bytes of SHA-256(PKIX DER).
func keyFingerprint(pub *rsa.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		// Fallback: random KID (should never happen for valid keys).
		b := make([]byte, 8)
		rand.Read(b)
		return fmt.Sprintf("%x", b)
	}
	h := sha256.Sum256(der)
	return fmt.Sprintf("%x", h[:8])
}
