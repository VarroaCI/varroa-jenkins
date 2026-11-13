package bus

import (
	"fmt"
	"log/slog"
)

// AssistantKeyRequest is the payload of operator.<cluster>.assistantkeys.set
// and .clear. APIKey carries a provider key, so the type redacts it in every
// log and format path. Only the key Secret may hold the value.
type AssistantKeyRequest struct {
	Provider string `json:"provider"`
	APIKey   []byte `json:"apiKey,omitempty"`
}

// AssistantKeyResponse is the reply of the assistant key subjects. It never
// holds a key. Code is empty on success.
type AssistantKeyResponse struct {
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// redactedKey replaces a key in every log and format.
const redactedKey = "[redacted]"

// LogValue implements slog.LogValuer. It never writes the key.
func (r AssistantKeyRequest) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("provider", r.Provider),
		slog.String("apiKey", redactedKey),
	)
}

// String implements fmt.Stringer. It never writes the key.
func (r AssistantKeyRequest) String() string {
	return fmt.Sprintf("AssistantKeyRequest{provider:%s apiKey:%s}", r.Provider, redactedKey)
}

// GoString implements fmt.GoStringer. It never writes the key.
func (r AssistantKeyRequest) GoString() string {
	return r.String()
}
