package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/datashare"
	"github.com/varroaci/varroa-jenkins/internal/agent/loop"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/sealedkey"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/pgdial"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// assistantMaxBody caps the size of one assistant request body. The message
// limit is 8000 characters, and the rest of a request is small.
const assistantMaxBody = 64 << 10

// Assistant error codes. Each answers a 4xx or 5xx body with a code field.
const (
	assistantErrDisabled      = "assistant_disabled"
	assistantErrKeyUnavail    = "assistant_key_unavailable"
	assistantErrOff           = "assistant_off"
	assistantErrBusy          = "assistant_busy"
	assistantErrPromptNoStart = "prompt_not_startable"
	assistantErrKillSwitch    = "kill_switch"
	// assistantErrApprovalExpired answers a decision on an approval whose
	// expiry passed. The decision is refused and the session is not resumed.
	assistantErrApprovalExpired = "approval_expired"
	// assistantErrTenantAgent answers a message to a Tenant agent session. Its
	// turns come from the runner, so a person cannot add one.
	assistantErrTenantAgent = "tenant_agent_session"
)

// AssistantRuntime holds the parts of the assistant that the BFF wires at
// start. A nil *AssistantRuntime on Dependencies turns every assistant route
// off.
type AssistantRuntime struct {
	Config   provider.Config
	Registry *provider.Registry
	// Store is the session store. The BFF wraps it with newNotifyingStore.
	Store store.Store
	// HostFor returns the tool host that acts as the caller.
	HostFor func(*auth.Claims) loop.ToolHost
	// HostForOrigin returns the tool host that acts as the caller and stamps
	// origin on its activity. A Tenant agent approval uses it with the
	// tenant_agent origin.
	HostForOrigin func(*auth.Claims, string) loop.ToolHost
	// Sealer opens and seals user keys. Nil turns PUT /me/assistant/key off.
	Sealer  *sealedkey.Sealer
	Owner   string
	Cluster string
	// Publish sends one bus message. Nil turns notifications off.
	Publish func(subject string, data []byte) error
	Logger  *slog.Logger
	Now     func() time.Time

	wg sync.WaitGroup
}

func (rt *AssistantRuntime) now() time.Time {
	if rt.Now == nil {
		return time.Now().UTC()
	}
	return rt.Now().UTC()
}

func (rt *AssistantRuntime) logger() *slog.Logger {
	if rt.Logger == nil {
		return slog.Default()
	}
	return rt.Logger
}

// Wait blocks until every started run returns. Tests use it.
func (rt *AssistantRuntime) Wait() { rt.wg.Wait() }

// RunConfig is the only builder of a loop.RunConfig. The approval and
// message handlers use it too, so every run reads the same settings.
func (rt *AssistantRuntime) RunConfig(claims *auth.Claims, s agent.Session, key provider.Key, tenants datashare.TenantResolver) loop.RunConfig {
	return loop.RunConfig{
		Host:        rt.HostFor(claims),
		DecideWrite: loop.AlwaysApprove,
		DaySpend: func(ctx context.Context) (int64, int64, error) {
			total, err := rt.Store.DaySpend(ctx, s.Principal)
			return total, rt.Config.Budgets.UserDay, err
		},
		Provider:       rt.providerFor(s.Provider),
		Key:            key,
		Model:          s.Model,
		Store:          rt.Store,
		SessionID:      s.ID,
		Owner:          rt.Owner,
		SessionBudget:  rt.Config.Budgets.Session,
		StepBudget:     rt.Config.Budgets.Steps,
		ApprovalExpiry: rt.Config.ApprovalExpiry,
		Tenants:        tenants,
		TenantLevels: func(ctx context.Context) (map[string]agent.DataSharing, error) {
			g, err := rt.Store.GetSettings(ctx, agent.ScopeGlobal)
			if err != nil {
				return nil, err
			}
			return g.TenantDataSharing, nil
		},
		Logger: rt.logger(),
		Now:    rt.now,
	}
}

// providerFor returns the adapter of a provider name, or nil. A nil adapter
// makes the provider call fail, which ends the session as a provider error.
func (rt *AssistantRuntime) providerFor(name string) provider.Provider {
	if rt.Registry == nil {
		return nil
	}
	e, ok := rt.Registry.Get(name)
	if !ok {
		return nil
	}
	return e.Provider
}

// startRun runs the loop in its own goroutine. The run uses a background
// context, so a finished request does not stop it.
func (rt *AssistantRuntime) startRun(cfg loop.RunConfig, trig loop.Trigger) {
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		if err := loop.Run(context.Background(), cfg, trig); err != nil {
			rt.logger().Error("assistant run failed", "session", cfg.SessionID.String(), "error", err.Error())
		}
	}()
}

// assistantTenantLookup maps a run or a tenant to its tenant for the caller.
// A lookup that finds nothing returns the empty tenant.
type assistantTenantLookup struct {
	s      *Server
	claims *auth.Claims
}

func (l assistantTenantLookup) RunTenant(ctx context.Context, runID string) (string, error) {
	if l.s.deps.RecordStore == nil {
		return "", nil
	}
	run, _, err := l.s.deps.RecordStore.GetRun(ctx, runID)
	if err != nil {
		return "", nil //nolint:nilerr // a run the lookup cannot find has no tenant to share data with
	}
	return run.Tenant, nil
}

func (l assistantTenantLookup) TenantTenant(ctx context.Context, tenant string) (string, error) {
	refs, serr := l.s.resolveTenantTenants(ctx, l.claims, tenant)
	if serr != nil || len(refs) == 0 {
		return "", nil //nolint:nilerr // an unresolved tenant has no tenant to share data with
	}
	return refs[0].Tenant, nil
}

// assistantOrDisabled returns the runtime, or writes the disabled answer.
func (s *Server) assistantOrDisabled(w http.ResponseWriter) (*AssistantRuntime, bool) {
	if s.deps.Assistant == nil {
		writeAssistantError(s, w, http.StatusServiceUnavailable, assistantErrDisabled, "the assistant is not enabled")
		return nil, false
	}
	return s.deps.Assistant, true
}

// writeAssistantError writes an error body with a code field.
func writeAssistantError(s *Server, w http.ResponseWriter, status int, code, msg string) {
	s.writeJSONErrorStatus(w, status, msg, map[string]any{"code": code})
}

// assistantStoreError answers a store error. A missing row is 404, an expired
// approval is 409 approval_expired, a state conflict is 409 assistant_busy,
// an old schema is 503 assistant_disabled, a database that does not answer is
// 503 record_store_unreachable, and anything else is 500. The schema case
// comes first, so an old schema never reads as an outage.
func (s *Server) assistantStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.writeJSONError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrExpired):
		writeAssistantError(s, w, http.StatusConflict, assistantErrApprovalExpired, "the approval expired")
	case errors.Is(err, store.ErrConflict):
		writeAssistantError(s, w, http.StatusConflict, assistantErrBusy, "the session changed; reload and try again")
	case errors.Is(err, recordstore.ErrSchemaTooOld), errors.Is(err, store.ErrSchemaTooOld):
		writeAssistantError(s, w, http.StatusServiceUnavailable, assistantErrDisabled, "the record store schema is too old for the assistant")
	case errors.Is(err, store.ErrUnavailable), errors.Is(err, recordstore.ErrUnavailable), pgdial.IsUnreachable(err):
		writeAssistantError(s, w, http.StatusServiceUnavailable, codeRecordStoreUnreachable, "record store unreachable")
	default:
		s.deps.Logger.Error("assistant store", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "assistant store failed")
	}
}

// assistantSessionJSON is the session shape of the API.
type assistantSessionJSON struct {
	ID               string         `json:"id"`
	Kind             string         `json:"kind"`
	Principal        string         `json:"principal"`
	Tenant           string         `json:"tenant"`
	Cluster          string         `json:"cluster"`
	Trigger          string         `json:"trigger"`
	Context          map[string]any `json:"context"`
	FirstMessage     string         `json:"firstMessage"`
	Provider         string         `json:"provider"`
	Model            string         `json:"model"`
	KeySource        string         `json:"keySource"`
	DataSharing      string         `json:"dataSharing"`
	State            string         `json:"state"`
	SkipReason       *string        `json:"skipReason"`
	PromptTokens     int64          `json:"promptTokens"`
	CompletionTokens int64          `json:"completionTokens"`
	Steps            int            `json:"steps"`
	StartedAt        time.Time      `json:"startedAt"`
	EndedAt          *time.Time     `json:"endedAt"`
}

func toAssistantSessionJSON(s agent.Session) assistantSessionJSON {
	out := assistantSessionJSON{
		ID: s.ID.String(), Kind: string(s.Kind), Principal: s.Principal, Tenant: s.Tenant,
		Cluster: s.Cluster, Trigger: s.Trigger, Context: s.Context, FirstMessage: s.FirstMessage,
		Provider: s.Provider, Model: s.Model, KeySource: string(s.KeySource), DataSharing: string(s.DataSharing),
		State: string(s.State), PromptTokens: s.PromptTokens, CompletionTokens: s.CompletionTokens,
		Steps: s.Steps, StartedAt: s.StartedAt, EndedAt: s.EndedAt,
	}
	if out.Context == nil {
		out.Context = map[string]any{}
	}
	if s.SkipReason != "" {
		r := string(s.SkipReason)
		out.SkipReason = &r
	}
	return out
}

// assistantApprovalJSON is the approval shape of the API.
type assistantApprovalJSON struct {
	ID          string          `json:"id"`
	SessionID   string          `json:"sessionId"`
	CallID      string          `json:"callId"`
	Tool        string          `json:"tool"`
	Arguments   json.RawMessage `json:"arguments"`
	TargetKind  string          `json:"targetKind"`
	TargetRef   string          `json:"targetRef"`
	State       string          `json:"state"`
	RequestedAt time.Time       `json:"requestedAt"`
	ExpiresAt   time.Time       `json:"expiresAt"`
	ResolvedBy  string          `json:"resolvedBy"`
	ResolvedAt  *time.Time      `json:"resolvedAt"`
	Reason      string          `json:"reason"`
	Result      *string         `json:"result"`
	// ResultWithheld is true when the caller may view the session but not its
	// tool result content. Result is then nil.
	ResultWithheld bool `json:"resultWithheld"`
}

func toAssistantApprovalJSON(a agent.Approval, result *string) assistantApprovalJSON {
	args := a.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return assistantApprovalJSON{
		ID: a.ID.String(), SessionID: a.SessionID.String(), CallID: a.CallID, Tool: a.Tool,
		Arguments: args, TargetKind: string(a.TargetKind), TargetRef: a.TargetRef,
		State: string(a.State), RequestedAt: a.RequestedAt, ExpiresAt: a.ExpiresAt,
		ResolvedBy: a.ResolvedBy, ResolvedAt: a.ResolvedAt, Reason: a.Reason, Result: result,
	}
}

// sessionUUID parses a path id. It answers false on a bad id.
func sessionUUID(raw string) (uuid.UUID, bool) {
	id, err := uuid.Parse(raw)
	return id, err == nil
}
