package forge

import (
	"fmt"
	"io"
	"log/slog"
)

// Token is a forge credential. It has no String method, and every
// formatting path renders it as [redacted].
type Token struct {
	v string
}

// NewToken wraps the credential bytes. The caller keeps no other copy.
func NewToken(b []byte) Token {
	return Token{v: string(b)}
}

// Empty reports whether the token holds no value.
func (t Token) Empty() bool { return t.v == "" }

// LogValue renders the token as [redacted] in structured logs.
func (Token) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// Format writes [redacted] for every verb.
func (Token) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, "[redacted]") }

// GoString renders the token as [redacted] for %#v.
func (Token) GoString() string { return "forge.Token([redacted])" }

// MarshalJSON renders the token as the JSON string "[redacted]".
func (Token) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// MarshalText renders the token as [redacted].
func (Token) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }

// reveal returns the raw value. Only the request header code may call it.
func (t Token) reveal() string { return t.v }
