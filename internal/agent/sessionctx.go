package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"github.com/google/uuid"
)

// SessionContext is the session identity and context that a tool call
// carries in its Go context. The loop sets it before each call.
type SessionContext struct {
	SessionID uuid.UUID
	Values    map[string]any
}

// String returns the value of key when it is a string, and "" otherwise.
func (c SessionContext) String(key string) string {
	s, _ := c.Values[key].(string)
	return s
}

type sessionContextKey struct{}

// WithSessionContext returns a context that carries sc.
func WithSessionContext(ctx context.Context, sc SessionContext) context.Context {
	return context.WithValue(ctx, sessionContextKey{}, sc)
}

// SessionContextFrom returns the session context of ctx, and false when
// ctx carries none.
func SessionContextFrom(ctx context.Context) (SessionContext, bool) {
	sc, ok := ctx.Value(sessionContextKey{}).(SessionContext)
	return sc, ok
}

// UnknownContextKeyError reports a context key that a client may not set.
type UnknownContextKeyError struct {
	Key string
}

// Error names the refused key.
func (e *UnknownContextKeyError) Error() string {
	return "unknown context key " + e.Key
}

// ParseClientContext decodes the context object of a client request. An
// empty raw value gives an empty map. Every key must be one of
// ClientContextKeys, and every value must be a JSON string. The server-only
// keys are refused with *UnknownContextKeyError like any other unknown key.
func ParseClientContext(raw json.RawMessage) (map[string]any, error) {
	out := map[string]any{}
	if len(raw) == 0 {
		return out, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("context must be a JSON object: %w", err)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	allowed := ClientContextKeys()
	for _, k := range keys {
		if !slices.Contains(allowed, k) {
			return nil, &UnknownContextKeyError{Key: k}
		}
		var s string
		if err := json.Unmarshal(fields[k], &s); err != nil {
			return nil, fmt.Errorf("context key %q must be a string", k)
		}
		out[k] = s
	}
	return out, nil
}
