package mcp

import (
	"context"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// SessionFrom returns the agent session id of a tool call. The loop sets it
// through agent.WithSessionContext. A Tenant agent token sets it through
// auth.ContextWithAgentSession. It returns false when neither is set.
func SessionFrom(ctx context.Context) (string, bool) {
	if sc, ok := agent.SessionContextFrom(ctx); ok && sc.SessionID != uuid.Nil {
		return sc.SessionID.String(), true
	}
	return auth.AgentSessionFrom(ctx)
}
