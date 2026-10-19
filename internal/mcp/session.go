package mcp

import (
	"context"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

type sessionIDKey struct{}

// SessionFrom returns the agent session id of a tool call. The loop sets it
// through agent.WithSessionContext. A caller may also set it with the
// unexported key, which tests use. It returns false when neither is set.
func SessionFrom(ctx context.Context) (string, bool) {
	if sc, ok := agent.SessionContextFrom(ctx); ok && sc.SessionID != uuid.Nil {
		return sc.SessionID.String(), true
	}
	if id, ok := ctx.Value(sessionIDKey{}).(string); ok && id != "" {
		return id, true
	}
	return "", false
}
