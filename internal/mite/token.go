package mite

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TokenSigner creates and validates single-use bootstrap tokens using
// HMAC-SHA256. Tokens are validated cryptographically; single-use enforcement
// (replay rejection) is layered on top via the server's ConsumedTokenStore,
// keyed by the token's jti.
type TokenSigner struct {
	key []byte
}

// TokenClaims are the fields carried by a v2 bootstrap token.
type TokenClaims struct {
	Version    string
	JTI        string
	Controller string
	Namespace  string
	Expiry     int64
}

// NewTokenSigner creates a TokenSigner backed by a secret key.
func NewTokenSigner(key []byte) *TokenSigner {
	return &TokenSigner{key: key}
}

// GenerateToken creates a signed v2 bootstrap token for the given controller.
// The payload is "v2"\x00jti\x00controller\x00namespace\x00expiry followed by
// an HMAC-SHA256 signature. A fresh 128-bit random jti is minted here so single-
// use enforcement can key on it; the operator call site keeps its (ctrl, ns, ttl)
// signature.
func (ts *TokenSigner) GenerateToken(controllerName, namespace string, ttl time.Duration) (string, error) {
	jtiBytes := make([]byte, 16)
	if _, err := rand.Read(jtiBytes); err != nil {
		return "", fmt.Errorf("generate jti: %w", err)
	}
	jti := base64.RawURLEncoding.EncodeToString(jtiBytes)
	expiry := time.Now().Add(ttl).Unix()
	payload := "v2" + "\x00" + jti + "\x00" + controllerName + "\x00" + namespace + "\x00" + strconv.FormatInt(expiry, 10)

	mac := hmac.New(sha256.New, ts.key)
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)

	raw := append([]byte(payload), sig...)
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// ValidateToken checks a bootstrap token's signature, version, expiry, and
// identity, returning the parsed claims. The signature is verified (constant-time)
// before any field is trusted; any failure returns a nil claims and non-nil error.
func (ts *TokenSigner) ValidateToken(token, controllerName, namespace string) (*TokenClaims, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("decode token: %w", err)
	}
	if len(raw) < sha256.Size {
		return nil, fmt.Errorf("token too short")
	}

	payload := raw[:len(raw)-sha256.Size]
	sig := raw[len(raw)-sha256.Size:]

	mac := hmac.New(sha256.New, ts.key)
	mac.Write(payload)
	expected := mac.Sum(nil)
	if !hmac.Equal(sig, expected) {
		return nil, fmt.Errorf("invalid signature")
	}

	claims, err := parsePayload(payload)
	if err != nil {
		return nil, err
	}
	if claims.Version != "v2" {
		return nil, fmt.Errorf("unsupported token version %q", claims.Version)
	}
	if time.Now().Unix() > claims.Expiry {
		return nil, fmt.Errorf("token expired")
	}
	if claims.Controller != controllerName || claims.Namespace != namespace {
		return nil, fmt.Errorf("controller/namespace mismatch")
	}
	return claims, nil
}

// IsCurrentTokenFormat reports whether token is a structurally-valid, unexpired
// v2 token. It does NOT verify the signature — it exists only for the operator's
// re-mint decision (whether a cached token needs rotating), never for auth.
func IsCurrentTokenFormat(token string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) < sha256.Size {
		return false
	}
	claims, err := parsePayload(raw[:len(raw)-sha256.Size])
	if err != nil {
		return false
	}
	return claims.Version == "v2" && time.Now().Unix() <= claims.Expiry
}

// ParseTokenIdentity extracts the controller name and namespace from a token
// without validating the signature (used for the pre-validation identity check
// in Register).
func ParseTokenIdentity(token string) (controllerName, namespace string, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", "", fmt.Errorf("decode token: %w", err)
	}
	if len(raw) < sha256.Size {
		return "", "", fmt.Errorf("token too short")
	}
	claims, err := parsePayload(raw[:len(raw)-sha256.Size])
	if err != nil {
		return "", "", err
	}
	return claims.Controller, claims.Namespace, nil
}

// parsePayload splits a v2 payload "v2"\x00jti\x00controller\x00namespace\x00expiry.
func parsePayload(payload []byte) (*TokenClaims, error) {
	parts := strings.SplitN(string(payload), "\x00", 5)
	if len(parts) != 5 {
		return nil, fmt.Errorf("malformed payload")
	}
	return &TokenClaims{
		Version:    parts[0],
		JTI:        parts[1],
		Controller: parts[2],
		Namespace:  parts[3],
		Expiry:     mustParseInt64(parts[4]),
	}, nil
}

func mustParseInt64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
