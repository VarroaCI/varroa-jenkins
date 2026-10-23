// Package loop runs one assistant session. It streams a provider call, runs
// the tool calls the model asks for, and parks the session when a write
// needs a person's approval. The session transcript is the only state, so
// any replica can resume a session from its events.
package loop

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/datashare"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
)

// ToolHost lists the tools of the caller and runs one of them. The host
// carries the caller's identity, so the tool runs with the caller's grants.
type ToolHost interface {
	List(ctx context.Context) ([]agent.ToolDef, error)
	Call(ctx context.Context, name string, args json.RawMessage) (agent.ToolResult, error)
}

// WriteDecision says what the loop does with a write tool call.
type WriteDecision int

// The write decisions.
const (
	// WriteApprove parks the session until a person approves the call.
	WriteApprove WriteDecision = iota
	// WriteRunNow runs the call without a person's approval. No caller uses
	// it yet. The tenant agent decides per tool through this hook.
	WriteRunNow
)

// AlwaysApprove parks the session for every write tool call. It is the
// assistant's decision.
func AlwaysApprove(agent.ToolDef, json.RawMessage) WriteDecision { return WriteApprove }

// Trigger says what started a Run. It is logged and does not change the work.
type Trigger int

// The triggers of a Run.
const (
	// TriggerStart is the first run of a session created by the handler.
	TriggerStart Trigger = iota
	// TriggerMessage is a run after a new user message.
	TriggerMessage
	// TriggerResume is a run after every approval of a parked session resolved.
	TriggerResume
)

// String names the trigger for the log.
func (t Trigger) String() string {
	switch t {
	case TriggerMessage:
		return "message"
	case TriggerResume:
		return "resume"
	}
	return "start"
}

// RunConfig is everything one Run needs. The caller takes the lease before
// it calls Run, so Run assumes it holds the lease of the session.
type RunConfig struct {
	Host          ToolHost
	DecideWrite   func(agent.ToolDef, json.RawMessage) WriteDecision
	DaySpend      func(ctx context.Context) (total, limit int64, err error)
	Provider      provider.Provider
	Key           provider.Key
	Model         string
	Store         store.Store
	SessionID     uuid.UUID
	Owner         string
	SessionBudget int64
	// SystemBase is the first sentence of the system text. An empty value uses
	// AssistantBase.
	SystemBase     string
	StepBudget     int
	ApprovalExpiry time.Duration
	Tenants        datashare.TenantResolver
	TenantLevels   func(ctx context.Context) (map[string]agent.DataSharing, error)
	Logger         *slog.Logger
	Now            func() time.Time
}

func (c RunConfig) now() time.Time {
	if c.Now == nil {
		return time.Now().UTC()
	}
	return c.Now().UTC()
}

func (c RunConfig) logger() *slog.Logger {
	if c.Logger == nil {
		return slog.Default()
	}
	return c.Logger
}
