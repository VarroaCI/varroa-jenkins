package provider

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// redacted is the only text that a Key prints or encodes.
const redacted = "[redacted]"

// Key is a provider credential. Every formatter, logger, and encoder prints
// redacted. The bytes leave the Key only through SetHeader.
type Key struct {
	b []byte
}

// NewKey returns a Key that holds a copy of b.
func NewKey(b []byte) Key {
	return Key{b: append([]byte(nil), b...)}
}

// Empty reports whether the key holds no bytes.
func (k Key) Empty() bool { return len(k.b) == 0 }

// Format prints the redacted text for every verb.
func (k Key) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, redacted)
}

// GoString prints the redacted text for %#v.
func (k Key) GoString() string { return redacted }

// String returns the redacted text.
func (k Key) String() string { return redacted }

// LogValue returns the redacted text for slog.
func (k Key) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON encodes the redacted text.
func (k Key) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText encodes the redacted text.
func (k Key) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// SetHeader sets header name to prefix followed by the key bytes. It does
// nothing for an empty key. It is the only reader of the key bytes.
func (k Key) SetHeader(h http.Header, name, prefix string) {
	if k.Empty() {
		return
	}
	h.Set(name, prefix+string(k.b))
}
