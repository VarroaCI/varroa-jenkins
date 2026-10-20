// Package store holds the agent session store. The Postgres store writes the
// four agent tables through the varroa_client role. The in-memory store keeps
// the same contract for the loop tests. Both read time from Options.Now, so a
// test controls every timestamp.
package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// MinSchemaVersion is the lowest record-store migration that this store
// reads. Migration 00038 adds the agent tables.
const MinSchemaVersion = 38

// Lease timing. A lease holder renews its lease every LeaseRenewEvery. A lease
// that is not renewed for LeaseTTL is expired. A waiting session with no
// event for LostResumeAfter is lost.
const (
	LeaseTTL        = 90 * time.Second
	LeaseRenewEvery = 30 * time.Second
	LostResumeAfter = 5 * time.Minute
)

// Sentinel errors. Callers match them with errors.Is.
var (
	ErrSchemaTooOld = errors.New("agent store: schema is too old")
	ErrNotFound     = errors.New("agent store: not found")
	ErrConflict     = errors.New("agent store: conflict")
	ErrExpired      = errors.New("agent store: approval expired")
	ErrInvalid      = errors.New("agent store: invalid input")
)

// checkSchemaVersion refuses a record-store schema below MinSchemaVersion.
func checkSchemaVersion(v int64) error {
	if v < MinSchemaVersion {
		return fmt.Errorf("%w: applied version %d is below %d", ErrSchemaTooOld, v, MinSchemaVersion)
	}
	return nil
}

// Options tunes a store. A nil Now reads the wall clock in UTC. A zero
// MaxConns keeps the pool default.
type Options struct {
	Now      func() time.Time
	MaxConns int32
}

func (o Options) now() time.Time {
	if o.Now == nil {
		return time.Now().UTC()
	}
	return o.Now().UTC()
}

// Guard is an optional check that a write runs under. The zero Guard checks
// nothing. States, when set, must contain the session's state. LeaseOwner,
// when set, must equal the session's lease owner.
type Guard struct {
	States     []agent.SessionState
	LeaseOwner string
}

func (g Guard) check(state agent.SessionState, owner string) error {
	if len(g.States) > 0 && !containsState(g.States, state) {
		return fmt.Errorf("%w: session state %s", ErrConflict, state)
	}
	if g.LeaseOwner != "" && owner != g.LeaseOwner {
		return fmt.Errorf("%w: lease owner changed", ErrConflict)
	}
	return nil
}

func containsState(list []agent.SessionState, s agent.SessionState) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// EndInput ends a session. From lists the states that the end accepts, and
// LeaseOwner, when set, names the owner that must still hold the lease.
type EndInput struct {
	From       []agent.SessionState
	LeaseOwner string
	State      agent.SessionState
	Events     []agent.EventInput
}

// SessionQuery filters and pages sessions. A zero Limit means 50. The
// maximum is 200.
type SessionQuery struct {
	Principal string
	Kind      agent.SessionKind
	Prompt    string
	Team      string
	State     agent.SessionState
	Limit     int
	Cursor    string
}

// SessionPage is one page of sessions. NextCursor is empty on the last page.
type SessionPage struct {
	Items      []agent.Session
	NextCursor string
}

// ApprovalQuery filters and pages approvals. A zero Limit means 50.
type ApprovalQuery struct {
	Principal  string
	SessionID  uuid.UUID
	State      agent.ApprovalState
	TargetKind agent.TargetKind
	TargetRef  string
	Limit      int
	Cursor     string
}

// ApprovalPage is one page of approvals.
type ApprovalPage struct {
	Items      []agent.Approval
	NextCursor string
}

// Resolution resolves one pending approval. Owner is the principal of the
// session, and a different principal sees ErrNotFound.
type Resolution struct {
	State  agent.ApprovalState
	By     string
	Reason string
	Owner  string
	Events []agent.EventInput
}

// ResolveResult is the outcome of one resolution. PendingLeft counts the
// pending approvals of the session after this resolution.
type ResolveResult struct {
	Approval    agent.Approval
	Session     agent.Session
	PendingLeft int
}

// SettingsPatch changes one or more settings fields. A nil field is left
// unchanged. Global fields are Paused, DefaultProvider, DefaultModel,
// TenantSet, and TenantRemove. User fields are Enabled, Provider, Model,
// and DataSharing.
type SettingsPatch struct {
	Paused          *bool
	DefaultProvider *string
	DefaultModel    *string
	TenantSet       *agent.TenantLevel
	TenantRemove    *string

	Enabled     *bool
	Provider    *string
	Model       *string
	DataSharing *agent.DataSharing

	// Check, when set, runs on the merged settings under the scope row lock,
	// before the write. An error from Check writes nothing, and PutSettings
	// returns that error. Check must not call the store: the memory store runs
	// Check under its one mutex, so a call back into the store deadlocks.
	Check func(after agent.Settings) error
}

// applyPatch applies p to st as the write would store it.
func applyPatch(st *agent.Settings, scope string, p SettingsPatch, by string, now time.Time) {
	st.Stored = true
	st.UpdatedAt = now
	st.UpdatedBy = by
	if scope == agent.ScopeGlobal {
		if p.Paused != nil {
			st.Paused = *p.Paused
			st.PausedBy, st.PausedAt = "", nil
			if st.Paused {
				st.PausedBy = by
				st.PausedAt = &now
			}
		}
		if p.DefaultProvider != nil {
			st.DefaultProvider = *p.DefaultProvider
		}
		if p.DefaultModel != nil {
			st.DefaultModel = *p.DefaultModel
		}
		if p.TenantSet != nil {
			if st.TenantDataSharing == nil {
				st.TenantDataSharing = map[string]agent.DataSharing{}
			}
			st.TenantDataSharing[p.TenantSet.Tenant] = p.TenantSet.Level
		}
		if p.TenantRemove != nil {
			delete(st.TenantDataSharing, *p.TenantRemove)
		}
		return
	}
	if p.Enabled != nil {
		st.Enabled = *p.Enabled
	}
	if p.Provider != nil {
		st.Provider = *p.Provider
	}
	if p.Model != nil {
		st.Model = *p.Model
	}
	if p.DataSharing != nil {
		st.DataSharing = *p.DataSharing
	}
}

// Store is the agent session store. Each write runs in one transaction,
// under the session row lock, and appends its events in the same transaction.
type Store interface {
	CreateSession(ctx context.Context, s agent.Session, events []agent.EventInput) (agent.Session, []agent.Event, error)
	GetSession(ctx context.Context, id uuid.UUID) (agent.Session, error)
	GetApproval(ctx context.Context, id uuid.UUID) (agent.Approval, error)
	ListSessions(ctx context.Context, q SessionQuery) (SessionPage, error)
	DaySpend(ctx context.Context, principal string) (int64, error)
	CountRunning(ctx context.Context, principal string) (int, error)
	TakeLease(ctx context.Context, id uuid.UUID, owner string, from []agent.SessionState) (agent.Session, error)
	RenewLease(ctx context.Context, id uuid.UUID, owner string) error
	ClearLease(ctx context.Context, id uuid.UUID, owner string, events []agent.EventInput) ([]agent.Event, error)
	RecordStep(ctx context.Context, id uuid.UUID, owner string, prompt, completion int64) (agent.Session, error)
	EndSession(ctx context.Context, id uuid.UUID, in EndInput) ([]agent.Event, error)
	AppendEvent(ctx context.Context, id uuid.UUID, g Guard, events ...agent.EventInput) ([]agent.Event, error)
	Events(ctx context.Context, id uuid.UUID, after, limit int) ([]agent.Event, error)
	CreateApprovals(ctx context.Context, sessionID uuid.UUID, owner string, approvals []agent.Approval, events []agent.EventInput) ([]agent.Approval, []agent.Event, error)
	ResolveApproval(ctx context.Context, id uuid.UUID, r Resolution) (ResolveResult, error)
	ListApprovals(ctx context.Context, q ApprovalQuery) (ApprovalPage, error)
	ExpireDue(ctx context.Context) (int, error)
	SweepLeases(ctx context.Context) (int, error)
	Sweep(ctx context.Context) (expired, lost int, err error)
	GetSettings(ctx context.Context, scope string) (agent.Settings, error)
	PutSettings(ctx context.Context, scope string, p SettingsPatch, by string) (agent.Settings, error)
	// SetInstructionsSHA records the revision a Team agent session read and
	// appends one event, in one write. The lease owner must hold a running session.
	SetInstructionsSHA(ctx context.Context, id uuid.UUID, owner, sha string, ev agent.EventInput) error
	// PutTeamAgentUnattended sets one TeamAgent's unattended grant, or removes it
	// when grant is nil. It writes the global scope only.
	PutTeamAgentUnattended(ctx context.Context, key string, grant *agent.UnattendedGrant, by string) error
}

// sweepBoth is the shared body of Sweep for both stores.
func sweepBoth(ctx context.Context, s Store) (int, int, error) {
	expired, err := s.ExpireDue(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("expire due approvals: %w", err)
	}
	lost, err := s.SweepLeases(ctx)
	if err != nil {
		return expired, 0, fmt.Errorf("sweep leases: %w", err)
	}
	return expired, lost, nil
}

// Validation helpers shared by both stores.

func validateSession(s agent.Session) error {
	if !containsKind(agent.SessionKinds(), s.Kind) {
		return fmt.Errorf("%w: kind %q", ErrInvalid, s.Kind)
	}
	if s.Principal == "" {
		return fmt.Errorf("%w: principal is empty", ErrInvalid)
	}
	if s.Cluster == "" {
		return fmt.Errorf("%w: cluster is empty", ErrInvalid)
	}
	if !containsKey(agent.KeySources(), s.KeySource) {
		return fmt.Errorf("%w: key source %q", ErrInvalid, s.KeySource)
	}
	if !s.DataSharing.Valid() {
		return fmt.Errorf("%w: data sharing %q", ErrInvalid, s.DataSharing)
	}
	if !containsState(agent.SessionStates(), s.State) {
		return fmt.Errorf("%w: state %q", ErrInvalid, s.State)
	}
	if s.State == agent.StateSkipped {
		if s.SkipReason == "" || !containsSkip(agent.SkipReasons(), s.SkipReason) {
			return fmt.Errorf("%w: a skipped session needs a skip reason", ErrInvalid)
		}
	} else if s.SkipReason != "" {
		return fmt.Errorf("%w: only a skipped session has a skip reason", ErrInvalid)
	}
	return nil
}

func containsKind(list []agent.SessionKind, k agent.SessionKind) bool {
	for _, v := range list {
		if v == k {
			return true
		}
	}
	return false
}

func containsKey(list []agent.KeySource, k agent.KeySource) bool {
	for _, v := range list {
		if v == k {
			return true
		}
	}
	return false
}

func containsSkip(list []agent.SkipReason, r agent.SkipReason) bool {
	for _, v := range list {
		if v == r {
			return true
		}
	}
	return false
}

func containsEventType(t agent.EventType) bool {
	for _, v := range agent.EventTypes() {
		if v == t {
			return true
		}
	}
	return false
}

// normalizeEvents checks the event types and payloads, and gives an empty
// payload the value {}.
func normalizeEvents(in []agent.EventInput) ([]agent.EventInput, error) {
	out := make([]agent.EventInput, 0, len(in))
	for _, e := range in {
		if !containsEventType(e.Type) {
			return nil, fmt.Errorf("%w: event type %q", ErrInvalid, e.Type)
		}
		payload := e.Payload
		if len(payload) == 0 {
			payload = []byte("{}")
		}
		if !jsonValid(payload) {
			return nil, fmt.Errorf("%w: event payload is not JSON", ErrInvalid)
		}
		out = append(out, agent.EventInput{Type: e.Type, Payload: payload})
	}
	return out, nil
}

// clampLimit returns the page size. Zero or less means 50, and the maximum is 200.
func clampLimit(n, def, ceiling int) int {
	if n <= 0 {
		return def
	}
	if n > ceiling {
		return ceiling
	}
	return n
}

// encodeCursor packs a sort time and an id into an opaque cursor.
func encodeCursor(t time.Time, id uuid.UUID) string {
	raw := t.UTC().Format(time.RFC3339Nano) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor reads a cursor made by encodeCursor. An empty cursor gives the
// zero values and no error.
func decodeCursor(c string) (time.Time, uuid.UUID, bool, error) {
	if c == "" {
		return time.Time{}, uuid.Nil, false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, uuid.Nil, false, fmt.Errorf("%w: cursor", ErrInvalid)
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.Nil, false, fmt.Errorf("%w: cursor", ErrInvalid)
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.Nil, false, fmt.Errorf("%w: cursor", ErrInvalid)
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, false, fmt.Errorf("%w: cursor", ErrInvalid)
	}
	return t, id, true, nil
}

// startOfDay returns midnight UTC of the day of t.
func startOfDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// scopeValid reports whether a settings scope is global or user:<subject>.
func scopeValid(scope string) bool {
	if scope == agent.ScopeGlobal {
		return true
	}
	return strings.HasPrefix(scope, agent.UserScopePrefix) && len(scope) > len(agent.UserScopePrefix)
}

// validatePatch checks that a patch names fields of one scope only, and that
// a tenant is not both set and removed.
func validatePatch(scope string, p SettingsPatch) error {
	global := scope == agent.ScopeGlobal
	userSet := p.Enabled != nil || p.Provider != nil || p.Model != nil || p.DataSharing != nil
	globalSet := p.Paused != nil || p.DefaultProvider != nil || p.DefaultModel != nil ||
		p.TenantSet != nil || p.TenantRemove != nil
	if global && userSet {
		return fmt.Errorf("%w: a global scope takes global fields only", ErrInvalid)
	}
	if !global && globalSet {
		return fmt.Errorf("%w: a user scope takes user fields only", ErrInvalid)
	}
	if p.DataSharing != nil && !p.DataSharing.Valid() {
		return fmt.Errorf("%w: data sharing %q", ErrInvalid, *p.DataSharing)
	}
	if p.TenantSet != nil && !p.TenantSet.Level.Valid() {
		return fmt.Errorf("%w: tenant level %q", ErrInvalid, p.TenantSet.Level)
	}
	if p.TenantSet != nil && p.TenantRemove != nil && p.TenantSet.Tenant == *p.TenantRemove {
		return fmt.Errorf("%w: a tenant is both set and removed", ErrInvalid)
	}
	return nil
}

// validateApproval checks the fields a new approval must carry: an id, an expiry,
// and a target kind from the registry.
func validateApproval(a agent.Approval) error {
	if a.ID == uuid.Nil || a.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: approval needs an id and an expiry", ErrInvalid)
	}
	if !containsTargetKind(a.TargetKind) {
		return fmt.Errorf("%w: target kind %q", ErrInvalid, a.TargetKind)
	}
	return nil
}

func containsTargetKind(k agent.TargetKind) bool {
	for _, v := range agent.TargetKinds() {
		if v == k {
			return true
		}
	}
	return false
}

// resolutionValid checks the state a resolution sets.
func resolutionValid(s agent.ApprovalState) error {
	if s != agent.ApprovalApproved && s != agent.ApprovalDenied {
		return fmt.Errorf("%w: resolution state %q", ErrInvalid, s)
	}
	return nil
}

// leaseExpiredMessage and resumeLostMessage are the texts of the two
// lease-loss events.
const (
	leaseExpiredMessage = "the lease expired before the loop run ended"
	resumeLostMessage   = "the session lost its resume after the last approval. An approved tool may have run."
)

// doneEnd returns the done payload that ends a session without a stop reason.
func doneEnd(state agent.SessionState) agent.EventInput {
	ev, _ := agent.NewEvent(agent.EventDone, agent.DonePayload{State: state})
	return ev
}

// lostEvents returns the error and done events that end a lost session.
func lostEvents(msg string) []agent.EventInput {
	errEv, _ := agent.NewEvent(agent.EventError, agent.ErrorPayload{Code: agent.CodeLeaseLost, Message: msg})
	return []agent.EventInput{errEv, doneEnd(agent.StateFailed)}
}

// jsonValid reports whether b holds one JSON value.
func jsonValid(b []byte) bool {
	return json.Valid(b)
}
