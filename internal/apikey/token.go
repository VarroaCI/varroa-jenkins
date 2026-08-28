package apikey

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const (
	prefixLen    = 8
	secretLen    = 32
	prefixEncLen = 13 // base32.RawHexEncoding.EncodedLen(8)
	secretEncLen = 43 // base64.RawURLEncoding.EncodedLen(32)
	tokenPrefix  = "vk_"
)

var errMalformed = errors.New("malformed API key")

// Generate creates a fresh API key: random prefix (8B base32) and secret (32B
// base64url), returning the prefix, secret, and the full vk_<prefix>.<secret> token.
func Generate() (prefix, secret, token string, err error) {
	rawPrefix := make([]byte, prefixLen)
	if _, err := rand.Read(rawPrefix); err != nil {
		return "", "", "", fmt.Errorf("generate prefix: %w", err)
	}
	prefix = strings.ToLower(base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(rawPrefix))

	rawSecret := make([]byte, secretLen)
	if _, err := rand.Read(rawSecret); err != nil {
		return "", "", "", fmt.Errorf("generate secret: %w", err)
	}
	secret = base64.RawURLEncoding.EncodeToString(rawSecret)

	token = tokenPrefix + prefix + "." + secret
	return prefix, secret, token, nil
}

// Parse extracts the prefix and secret from a vk_<prefix>.<secret> token.
func Parse(raw string) (prefix, secret string, err error) {
	if !strings.HasPrefix(raw, tokenPrefix) {
		return "", "", errMalformed
	}
	body := raw[len(tokenPrefix):]
	dot := strings.IndexByte(body, '.')
	if dot <= 0 || dot != prefixEncLen || len(body) != prefixEncLen+1+secretEncLen {
		return "", "", errMalformed
	}
	prefix = body[:dot]
	secret = body[dot+1:]

	if len(prefix) != prefixEncLen || len(secret) != secretEncLen {
		return "", "", errMalformed
	}
	for _, c := range prefix {
		if !isBase32Hex(c) {
			return "", "", errMalformed
		}
	}
	for _, c := range secret {
		if !isBase64URL(c) {
			return "", "", errMalformed
		}
	}
	return prefix, secret, nil
}

func isBase32Hex(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'v') || (c >= 'A' && c <= 'V')
}

func isBase64URL(c rune) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
}
