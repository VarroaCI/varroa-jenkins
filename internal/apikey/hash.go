package apikey

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// Hash returns the base64url-encoded SHA-256 hash of the secret.
func Hash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// Verify compares a secret against a stored Hash using constant-time comparison.
func Verify(secret, hash string) bool {
	computed := Hash(secret)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(hash)) == 1
}
