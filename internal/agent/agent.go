package agent

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// SessionState is the state of one agent session.
type SessionState string

// The session states. A terminal state never changes again.
const (
	StateRunning         SessionState = "running"
	StateWaitingApproval SessionState = "waiting_approval"
	StateCompleted       SessionState = "completed"
	StateFailed          SessionState = "failed"
	StateCancelled       SessionState = "cancelled"
	StateSkipped         SessionState = "skipped"
	StateExpired         SessionState = "expired"
)

// SessionKind is the kind of one agent session.
type SessionKind string

// The session kinds.
const (
	KindAssistant   SessionKind = "assistant"
	KindTenantAgent SessionKind = "tenant_agent"
)

// EventType is the type of one session event.
type EventType string

// The event types of a session transcript.
const (
	EventUser              EventType = "user"
	EventAssistant         EventType = "assistant"
	EventToolCall          EventType = "tool_call"
	EventToolResult        EventType = "tool_result"
	EventApprovalRequested EventType = "approval_requested"
	EventApprovalResolved  EventType = "approval_resolved"
	EventError             EventType = "error"
	EventDone              EventType = "done"
)

// ApprovalState is the state of one approval request.
type ApprovalState string

// The approval states.
const (
	ApprovalPending  ApprovalState = "pending"
	ApprovalApproved ApprovalState = "approved"
	ApprovalDenied   ApprovalState = "denied"
	ApprovalExpired  ApprovalState = "expired"
)

// SkipReason says why a session was skipped at creation.
type SkipReason string

// The skip reasons.
const (
	SkipBudgetDay           SkipReason = "budget_day"
	SkipKillSwitch          SkipReason = "kill_switch"
	SkipDataSharingNone     SkipReason = "data_sharing_none"
	SkipProviderUnavailable SkipReason = "provider_unavailable"
)

// ErrorCode is the code of an error event.
type ErrorCode string

// The error codes of an error event.
const (
	CodeBudgetSession     ErrorCode = "budget_session"
	CodeBudgetDay         ErrorCode = "budget_day"
	CodeSteps             ErrorCode = "steps"
	CodeKillSwitch        ErrorCode = "kill_switch"
	CodeProviderError     ErrorCode = "provider_error"
	CodeSessionKeyMissing ErrorCode = "session_key_missing"
	CodeLeaseLost         ErrorCode = "lease_lost"
)

// KeySource says where a session's provider key came from.
type KeySource string

// The key sources.
const (
	KeySourceChart KeySource = "chart"
	KeySourceUser  KeySource = "user"
)

// TargetKind is the kind of object a write tool call acts on.
type TargetKind string

// The target kinds.
const (
	TargetRun        TargetKind = "run"
	TargetJob        TargetKind = "job"
	TargetController TargetKind = "controller"
	TargetNone       TargetKind = "none"
)

// DataSharing is the level of data a provider may receive.
type DataSharing string

// The data-sharing levels, from the least to the most.
const (
	DataNone     DataSharing = "none"
	DataMetadata DataSharing = "metadata"
	DataLogs     DataSharing = "logs"
)

// ProviderKind is the wire protocol of a model provider.
type ProviderKind string

// The provider kinds. The anthropic value is the name of the Messages API
// protocol, not an identity claim.
const (
	ProviderAnthropic        ProviderKind = "anthropic"
	ProviderOpenAICompatible ProviderKind = "openai-compatible"
)

// ProviderTrust says whether a provider runs inside the cluster boundary.
type ProviderTrust string

// The provider trust levels.
const (
	TrustInternal ProviderTrust = "internal"
	TrustExternal ProviderTrust = "external"
)

// ToolClass says whether a tool only reads or may change state.
type ToolClass string

// The tool classes.
const (
	ClassRead  ToolClass = "read"
	ClassWrite ToolClass = "write"
)

// Untyped constants shared by the store, the loop, and the handlers.
const (
	TriggerManual = "manual"

	ActivitySourceAssistant = "assistant"
	// ActivitySourceTenantAgent stamps activity written by a Tenant agent session.
	ActivitySourceTenantAgent = "tenant_agent"

	// MaxRunningSessions caps the sessions one principal runs at once. The
	// create path counts running rows against it; a race between two creates
	// can exceed it by one, which the create path accepts.
	MaxRunningSessions = 3

	ScopeGlobal       = "global"
	UserScopePrefix   = "user:"
	TicketScopePrefix = "assistant:"

	NoTenantToken = "_none"

	CtxRoute      = "route"
	CtxTenant     = "tenant"
	CtxJob        = "job"
	CtxRun        = "run"
	CtxController = "controller"
	CtxCell       = "cell"

	CtxRepo   = "repo"
	CtxPrompt = "prompt"
)

// UserScope returns the settings scope of one user subject.
func UserScope(subject string) string { return UserScopePrefix + subject }

// TicketScope returns the scope of one assistant session ticket.
func TicketScope(sessionID string) string { return TicketScopePrefix + sessionID }

// Terminal reports whether the state ends a session for good.
func (s SessionState) Terminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateCancelled, StateSkipped, StateExpired:
		return true
	}
	return false
}

// Valid reports whether the level is one of the three data-sharing levels.
func (d DataSharing) Valid() bool {
	switch d {
	case DataNone, DataMetadata, DataLogs:
		return true
	}
	return false
}

// SessionStates lists every session state in registry order.
func SessionStates() []SessionState {
	return []SessionState{StateRunning, StateWaitingApproval, StateCompleted, StateFailed,
		StateCancelled, StateSkipped, StateExpired}
}

// SessionKinds lists every session kind in registry order.
func SessionKinds() []SessionKind { return []SessionKind{KindAssistant, KindTenantAgent} }

// EventTypes lists every event type in registry order.
func EventTypes() []EventType {
	return []EventType{EventUser, EventAssistant, EventToolCall, EventToolResult,
		EventApprovalRequested, EventApprovalResolved, EventError, EventDone}
}

// ApprovalStates lists every approval state in registry order.
func ApprovalStates() []ApprovalState {
	return []ApprovalState{ApprovalPending, ApprovalApproved, ApprovalDenied, ApprovalExpired}
}

// SkipReasons lists every skip reason in registry order.
func SkipReasons() []SkipReason {
	return []SkipReason{SkipBudgetDay, SkipKillSwitch, SkipDataSharingNone, SkipProviderUnavailable}
}

// ErrorCodes lists every error code in registry order.
func ErrorCodes() []ErrorCode {
	return []ErrorCode{CodeBudgetSession, CodeBudgetDay, CodeSteps, CodeKillSwitch,
		CodeProviderError, CodeSessionKeyMissing, CodeLeaseLost}
}

// KeySources lists every key source in registry order.
func KeySources() []KeySource { return []KeySource{KeySourceChart, KeySourceUser} }

// TargetKinds lists every target kind in registry order.
func TargetKinds() []TargetKind {
	return []TargetKind{TargetRun, TargetJob, TargetController, TargetNone}
}

// DataLevels lists the data-sharing levels from none to logs.
func DataLevels() []DataSharing { return []DataSharing{DataNone, DataMetadata, DataLogs} }

// ProviderKinds lists every provider kind in registry order.
func ProviderKinds() []ProviderKind {
	return []ProviderKind{ProviderAnthropic, ProviderOpenAICompatible}
}

// ProviderTrusts lists every provider trust level in registry order.
func ProviderTrusts() []ProviderTrust { return []ProviderTrust{TrustInternal, TrustExternal} }

// ToolClasses lists every tool class in registry order.
func ToolClasses() []ToolClass { return []ToolClass{ClassRead, ClassWrite} }

// ClientContextKeys lists the context keys a client may set.
func ClientContextKeys() []string {
	return []string{CtxRoute, CtxTenant, CtxJob, CtxRun, CtxController, CtxCell}
}

// ServerContextKeys lists the context keys only the server may set.
func ServerContextKeys() []string { return []string{CtxRepo, CtxPrompt} }

// ToolDef describes one tool that the loop may offer a model.
type ToolDef struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Class       ToolClass
	Hidden      bool
}

// ToolResult is the outcome of one tool call.
type ToolResult struct {
	Content string
	IsError bool
}

// Session is one agent session row.
type Session struct {
	ID                    uuid.UUID
	Kind                  SessionKind
	Principal             string
	Tenant                string
	Cluster               string
	Trigger               string
	Context               map[string]any
	Provider              string
	Model                 string
	KeySource             KeySource
	DataSharing           DataSharing
	InstructionsSHA       *string
	TenantAgentGeneration *int64
	State                 SessionState
	SkipReason            SkipReason
	PromptTokens          int64
	CompletionTokens      int64
	Steps                 int
	LeaseOwner            string
	LeaseExpiresAt        *time.Time
	StartedAt             time.Time
	EndedAt               *time.Time
	// FirstMessage is the first user text in its list form (TrimFirstMessage).
	// The store reads it from the events. No column holds it, and a write ignores it.
	FirstMessage string
}

// Tokens returns the prompt and completion tokens of the session together.
func (s Session) Tokens() int64 { return s.PromptTokens + s.CompletionTokens }

// EventInput is one event to append. The store assigns its sequence number.
type EventInput struct {
	Type    EventType
	Payload json.RawMessage
}

// Event is one stored session event.
type Event struct {
	SessionID uuid.UUID
	Seq       int
	Type      EventType
	Payload   json.RawMessage
	CreatedAt time.Time
}

// Approval is one approval request for a write tool call.
type Approval struct {
	ID          uuid.UUID
	SessionID   uuid.UUID
	CallID      string
	Tool        string
	Arguments   json.RawMessage
	TargetKind  TargetKind
	TargetRef   string
	State       ApprovalState
	RequestedAt time.Time
	ExpiresAt   time.Time
	ResolvedBy  string
	ResolvedAt  *time.Time
	Reason      string
}

// TenantLevel is one tenant's data-sharing level.
type TenantLevel struct {
	Tenant string
	Level  DataSharing
}

// Settings is the settings of one scope: the global row or one user row.
// The global fields are set on the global scope and the user fields on a
// user scope.
type Settings struct {
	Scope string
	// Stored is false when the scope has no row yet and the values are the
	// defaults.
	Stored bool

	Paused            bool
	PausedBy          string
	PausedAt          *time.Time
	DefaultProvider   string
	DefaultModel      string
	TenantDataSharing map[string]DataSharing

	Enabled     bool
	Provider    string
	Model       string
	DataSharing DataSharing

	// TenantAgentUnattended maps a TenantAgent's namespace/name to its grant.
	// Only the global scope carries it.
	TenantAgentUnattended map[string]UnattendedGrant

	UpdatedAt time.Time
	UpdatedBy string
}

// UnattendedGrant names the write tools that one TenantAgent may run without an
// approval. UID pins the grant to one object, so a recreated object with the
// same name gets no grant.
type UnattendedGrant struct {
	UID       string    `json:"uid"`
	Tools     []string  `json:"tools"`
	GrantedBy string    `json:"granted_by"`
	GrantedAt time.Time `json:"granted_at"`
}

// NewID returns a new UUIDv7 for a session, approval, or ticket.
func NewID() (uuid.UUID, error) { return uuid.NewV7() }
