// Package sealedkey seals a user's provider key into a cookie value. The
// BFF replicas share one signing key, so any replica opens a cookie that
// another replica sealed. A sealed value opens only for the subject and
// login session that sealed it.
package sealedkey

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
)

const hkdfInfo = "varroa-assistant-session-key-v1"

// Sealer seals and opens provider keys with one AES-GCM key.
type Sealer struct {
	aead cipher.AEAD
}

// New derives the sealing key from the PEM of the BFF signing key.
func New(pem string) (*Sealer, error) {
	if pem == "" {
		return nil, errors.New("sealedkey: signing key is empty")
	}
	key, err := hkdf.Key(sha256.New, []byte(pem), nil, hkdfInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("sealedkey: derive key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("sealedkey: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("sealedkey: gcm: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

// payload is the sealed plaintext. Sub and Iat bind the value to one login
// session, so a stolen value opens for no other session.
type payload struct {
	Sub      string `json:"sub"`
	Iat      int64  `json:"iat"`
	Provider string `json:"provider"`
	APIKey   []byte `json:"apiKey"`
}

// Seal returns the cookie value that holds apiKey for the provider of one
// login session.
func (s *Sealer) Seal(sub string, iat int64, providerName string, apiKey []byte) (string, error) {
	plain, err := json.Marshal(payload{Sub: sub, Iat: iat, Provider: providerName, APIKey: apiKey})
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := s.aead.Seal(nonce, nonce, plain, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Open returns the provider name and key of a cookie value. It returns false
// when the value does not decode, does not authenticate, or belongs to
// another subject or login session.
func (s *Sealer) Open(value, sub string, iat int64) (string, provider.Key, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", provider.Key{}, false
	}
	n := s.aead.NonceSize()
	if len(raw) < n {
		return "", provider.Key{}, false
	}
	plain, err := s.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", provider.Key{}, false
	}
	var p payload
	if err := json.Unmarshal(plain, &p); err != nil {
		return "", provider.Key{}, false
	}
	if p.Sub != sub || p.Iat != iat {
		return "", provider.Key{}, false
	}
	return p.Provider, provider.NewKey(p.APIKey), true
}
