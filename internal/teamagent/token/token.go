// Package token mints the credential of a Team agent session. The token is a
// compact HS256 JWT. The value never prints: it has no String method, and
// each formatting path writes the placeholder.
package token

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/internal/teamagent/accept"
)

// Redacted is the text that stands for the token in every log, format, and
// encoding.
const Redacted = "[redacted]"

// minKeyBytes is the smallest HMAC key the file may hold.
const minKeyBytes = 32

// Value is a minted Team agent token. Its zero value holds no token.
type Value struct {
	v   string
	exp time.Time
}

// LogValue writes the placeholder to a structured log.
func (Value) LogValue() slog.Value { return slog.StringValue(Redacted) }

// Format writes the placeholder for every verb.
func (Value) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, Redacted) }

// GoString writes the placeholder for %#v.
func (Value) GoString() string { return "token.Value(" + Redacted + ")" }

// MarshalJSON writes the placeholder as a JSON string.
func (Value) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }

// MarshalText writes the placeholder as text.
func (Value) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// SetHeader sets the Authorization header to the bearer token. It is the
// only way the value leaves the package.
func (v Value) SetHeader(h http.Header) { h.Set("Authorization", "Bearer "+v.v) }

// ExpiresAt returns the expiry time of the token.
func (v Value) ExpiresAt() time.Time { return v.exp }

// ReadKey reads the HMAC key from a file. The file must hold at least 32
// bytes. The error never names the key bytes.
func ReadKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < minKeyBytes {
		return nil, fmt.Errorf("team agent key %s holds %d bytes, want at least %d", path, len(b), minKeyBytes)
	}
	return b, nil
}

// header is the fixed JOSE header of every Team agent token.
type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// Claims is the payload of a Team agent token.
type Claims struct {
	Aud string `json:"aud"`
	Iss string `json:"iss"`
	Sub string `json:"sub"`
	UID string `json:"uid"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	SID string `json:"sid"`
	JTI string `json:"jti"`
}

// Mint returns a token for one session of one agent. The token expires
// after accept.TokenLifetime.
func Mint(key []byte, principal string, uid types.UID, sid uuid.UUID, now time.Time) (Value, error) {
	if len(key) < minKeyBytes {
		return Value{}, errors.New("team agent key is too short")
	}
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return Value{}, err
	}
	h, err := json.Marshal(header{Alg: "HS256", Typ: "JWT", Kid: accept.TokenKeyID})
	if err != nil {
		return Value{}, err
	}
	p, err := json.Marshal(Claims{
		Aud: accept.TokenAudience,
		Iss: accept.TokenIssuer,
		Sub: principal,
		UID: string(uid),
		Iat: now.Unix(),
		Exp: now.Add(accept.TokenLifetime).Unix(),
		SID: sid.String(),
		JTI: hex.EncodeToString(jti),
	})
	if err != nil {
		return Value{}, err
	}
	signing := enc(h) + "." + enc(p)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signing))
	token := signing + "." + enc(mac.Sum(nil))
	return Value{v: token, exp: now.Add(accept.TokenLifetime)}, nil
}

// enc encodes bytes as unpadded base64url, the JWT segment form.
func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
