package store

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// Memory is an in-memory Store with the same contract as Postgres. One mutex
// guards every method, so each write is atomic.
type Memory struct {
	mu        sync.Mutex
	o         Options
	sessions  map[uuid.UUID]*agent.Session
	events    map[uuid.UUID][]agent.Event
	approvals map[uuid.UUID]*agent.Approval
	settings  map[string]*agent.Settings
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty in-memory store with the global settings row.
func NewMemory(o Options) *Memory {
	m := &Memory{
		o:         o,
		sessions:  map[uuid.UUID]*agent.Session{},
		events:    map[uuid.UUID][]agent.Event{},
		approvals: map[uuid.UUID]*agent.Approval{},
		settings:  map[string]*agent.Settings{},
	}
	m.settings[agent.ScopeGlobal] = &agent.Settings{Scope: agent.ScopeGlobal, Stored: true, TenantDataSharing: map[string]agent.DataSharing{}}
	return m
}

func (m *Memory) now() time.Time { return m.o.now() }

// appendLocked adds events to a session. The caller holds m.mu.
func (m *Memory) appendLocked(id uuid.UUID, in []agent.EventInput, now time.Time) []agent.Event {
	out := make([]agent.Event, 0, len(in))
	for _, e := range in {
		seq := len(m.events[id]) + 1
		ev := agent.Event{SessionID: id, Seq: seq, Type: e.Type, Payload: slices.Clone(e.Payload), CreatedAt: now}
		m.events[id] = append(m.events[id], ev)
		out = append(out, ev)
	}
	return out
}

// sessionLocked returns the stored session or ErrNotFound.
func (m *Memory) sessionLocked(id uuid.UUID) (*agent.Session, error) {
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	return s, nil
}

// guardLocked applies a Guard to a stored session.
func guardLocked(s *agent.Session, g Guard) error {
	return g.check(s.State, s.LeaseOwner)
}

func copySession(s *agent.Session) agent.Session {
	out := *s
	out.Context = cloneContext(s.Context)
	return out
}

// cloneContext copies a session context map, so no caller shares it with the store.
func cloneContext(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyApproval(a *agent.Approval) agent.Approval {
	out := *a
	out.Arguments = slices.Clone(a.Arguments)
	return out
}

func copyEvents(in []agent.Event) []agent.Event {
	out := slices.Clone(in)
	for i := range out {
		out[i].Payload = slices.Clone(out[i].Payload)
	}
	return out
}

// CreateSession stores a new session and its events.
func (m *Memory) CreateSession(_ context.Context, s agent.Session, events []agent.EventInput) (agent.Session, []agent.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	s, err := fillSession(s, now)
	if err != nil {
		return agent.Session{}, nil, err
	}
	evs, err := normalizeEvents(events)
	if err != nil {
		return agent.Session{}, nil, err
	}
	if _, exists := m.sessions[s.ID]; exists {
		return agent.Session{}, nil, fmt.Errorf("%w: session %s exists", ErrConflict, s.ID)
	}
	s.Context = cloneContext(s.Context)
	m.sessions[s.ID] = &s
	out := m.appendLocked(s.ID, evs, now)
	return copySession(&s), copyEvents(out), nil
}

// fillSession sets the defaults of a new session row and checks it.
func fillSession(s agent.Session, now time.Time) (agent.Session, error) {
	if s.ID == uuid.Nil {
		id, err := agent.NewID()
		if err != nil {
			return s, fmt.Errorf("new session id: %w", err)
		}
		s.ID = id
	}
	if s.StartedAt.IsZero() {
		s.StartedAt = now
	}
	s.StartedAt = s.StartedAt.UTC()
	if s.Trigger == "" {
		s.Trigger = agent.TriggerManual
	}
	if s.Context == nil {
		s.Context = map[string]any{}
	}
	if s.State == agent.StateSkipped {
		ended := s.StartedAt
		s.EndedAt = &ended
	}
	if s.LeaseOwner != "" {
		exp := now.Add(LeaseTTL)
		s.LeaseExpiresAt = &exp
	}
	if err := validateSession(s); err != nil {
		return s, err
	}
	return s, nil
}

// GetSession returns one session.
func (m *Memory) GetSession(_ context.Context, id uuid.UUID) (agent.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.sessionLocked(id)
	if err != nil {
		return agent.Session{}, err
	}
	return copySession(s), nil
}

// GetApproval returns one approval.
func (m *Memory) GetApproval(_ context.Context, id uuid.UUID) (agent.Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok {
		return agent.Approval{}, fmt.Errorf("%w: approval %s", ErrNotFound, id)
	}
	return copyApproval(a), nil
}

// ListSessions returns one page of sessions, newest first.
func (m *Memory) ListSessions(_ context.Context, q SessionQuery) (SessionPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cT, cID, hasCursor, err := decodeCursor(q.Cursor)
	if err != nil {
		return SessionPage{}, err
	}
	limit := clampLimit(q.Limit, 50, 200)
	var rows []agent.Session
	for _, s := range m.sessions {
		if q.Principal != "" && s.Principal != q.Principal {
			continue
		}
		if q.Kind != "" && s.Kind != q.Kind {
			continue
		}
		if q.Prompt != "" && s.Context[agent.CtxPrompt] != q.Prompt {
			continue
		}
		if q.Team != "" && s.Team != q.Team {
			continue
		}
		if q.State != "" && s.State != q.State {
			continue
		}
		if hasCursor && !sessionBefore(*s, cT, cID) {
			continue
		}
		rows = append(rows, copySession(s))
	}
	sort.Slice(rows, func(i, j int) bool {
		return sessionBefore(rows[j], rows[i].StartedAt, rows[i].ID)
	})
	page := SessionPage{}
	if len(rows) > limit {
		page.Items = rows[:limit]
		last := page.Items[limit-1]
		page.NextCursor = encodeCursor(last.StartedAt, last.ID)
	} else {
		page.Items = rows
	}
	return page, nil
}

// sessionBefore reports whether s sorts strictly before (t, id) in the
// started_at DESC, id DESC order. "Before" here means newer.
func sessionBefore(s agent.Session, t time.Time, id uuid.UUID) bool {
	if !s.StartedAt.Equal(t) {
		return s.StartedAt.Before(t)
	}
	return s.ID.String() < id.String()
}

// DaySpend returns the tokens of the principal's sessions that started on the
// current UTC day.
func (m *Memory) DaySpend(_ context.Context, principal string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	start := startOfDay(m.now())
	end := start.Add(24 * time.Hour)
	var total int64
	for _, s := range m.sessions {
		if s.Principal == principal && !s.StartedAt.Before(start) && s.StartedAt.Before(end) {
			total += s.Tokens()
		}
	}
	return total, nil
}

// CountRunning returns the number of running sessions of the principal.
func (m *Memory) CountRunning(_ context.Context, principal string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.sessions {
		if s.Principal == principal && s.State == agent.StateRunning {
			n++
		}
	}
	return n, nil
}

// TakeLease starts or resumes a session under owner. The session must be in
// one of from, and it must be unleased or already held by owner.
func (m *Memory) TakeLease(_ context.Context, id uuid.UUID, owner string, from []agent.SessionState) (agent.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(from) == 0 {
		return agent.Session{}, fmt.Errorf("%w: take lease needs a from state", ErrInvalid)
	}
	s, err := m.sessionLocked(id)
	if err != nil {
		return agent.Session{}, err
	}
	if !containsState(from, s.State) || (s.LeaseOwner != "" && s.LeaseOwner != owner) {
		return agent.Session{}, fmt.Errorf("%w: lease not available", ErrConflict)
	}
	exp := m.now().Add(LeaseTTL)
	s.State = agent.StateRunning
	s.LeaseOwner = owner
	s.LeaseExpiresAt = &exp
	s.EndedAt = nil
	return copySession(s), nil
}

// RenewLease extends the lease of a running session held by owner.
func (m *Memory) RenewLease(_ context.Context, id uuid.UUID, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.sessionLocked(id)
	if err != nil {
		return err
	}
	now := m.now()
	if s.State != agent.StateRunning || s.LeaseOwner != owner || s.LeaseExpiresAt == nil || !s.LeaseExpiresAt.After(now) {
		return fmt.Errorf("%w: lease not held", ErrConflict)
	}
	exp := now.Add(LeaseTTL)
	s.LeaseExpiresAt = &exp
	return nil
}

// ClearLease parks a running session that owner holds. It clears the lease,
// moves the session to waiting_approval, and appends events.
func (m *Memory) ClearLease(_ context.Context, id uuid.UUID, owner string, events []agent.EventInput) ([]agent.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	evs, err := normalizeEvents(events)
	if err != nil {
		return nil, err
	}
	s, err := m.sessionLocked(id)
	if err != nil {
		return nil, err
	}
	if err := guardLocked(s, Guard{States: []agent.SessionState{agent.StateRunning}, LeaseOwner: owner}); err != nil {
		return nil, err
	}
	s.State = agent.StateWaitingApproval
	s.LeaseOwner = ""
	s.LeaseExpiresAt = nil
	return copyEvents(m.appendLocked(id, evs, m.now())), nil
}

// RecordStep adds one provider call to a running session held by owner.
func (m *Memory) RecordStep(_ context.Context, id uuid.UUID, owner string, prompt, completion int64) (agent.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.sessionLocked(id)
	if err != nil {
		return agent.Session{}, err
	}
	if err := guardLocked(s, Guard{States: []agent.SessionState{agent.StateRunning}, LeaseOwner: owner}); err != nil {
		return agent.Session{}, err
	}
	s.PromptTokens += prompt
	s.CompletionTokens += completion
	s.Steps++
	return copySession(s), nil
}

// EndSession ends a session with a final state, clears its lease, and
// appends events. It denies every pending approval of the session.
func (m *Memory) EndSession(_ context.Context, id uuid.UUID, in EndInput) ([]agent.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !endable(in.State) {
		return nil, fmt.Errorf("%w: end state %q", ErrInvalid, in.State)
	}
	evs, err := normalizeEvents(in.Events)
	if err != nil {
		return nil, err
	}
	s, err := m.sessionLocked(id)
	if err != nil {
		return nil, err
	}
	if s.State.Terminal() || (len(in.From) > 0 && !containsState(in.From, s.State)) {
		return nil, fmt.Errorf("%w: session state %s", ErrConflict, s.State)
	}
	if in.LeaseOwner != "" && s.LeaseOwner != in.LeaseOwner {
		return nil, fmt.Errorf("%w: lease owner changed", ErrConflict)
	}
	now := m.now()
	s.State = in.State
	s.EndedAt = &now
	s.LeaseOwner = ""
	s.LeaseExpiresAt = nil
	pre, err := deniedEvents(m.denyPendingLocked(id, in.By, endedApprovalReason(in.State), now), in.By)
	if err != nil {
		return nil, err
	}
	return copyEvents(m.appendLocked(id, append(pre, evs...), now)), nil
}

// denyPendingLocked denies the pending approvals of session sid because the
// session ended. It returns their ids in a fixed order.
func (m *Memory) denyPendingLocked(sid uuid.UUID, by, reason string, now time.Time) []uuid.UUID {
	var ids []uuid.UUID
	for _, a := range m.approvals {
		if a.SessionID != sid || a.State != agent.ApprovalPending {
			continue
		}
		a.State = agent.ApprovalDenied
		a.ResolvedBy = by
		a.ResolvedAt = &now
		a.Reason = reason
		ids = append(ids, a.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

// endable reports whether a session may end in the state.
func endable(s agent.SessionState) bool {
	switch s {
	case agent.StateCompleted, agent.StateFailed, agent.StateCancelled, agent.StateExpired:
		return true
	}
	return false
}

// AppendEvent appends events to a session when the guard passes.
func (m *Memory) AppendEvent(_ context.Context, id uuid.UUID, g Guard, events ...agent.EventInput) ([]agent.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	evs, err := normalizeEvents(events)
	if err != nil {
		return nil, err
	}
	s, err := m.sessionLocked(id)
	if err != nil {
		return nil, err
	}
	if err := guardLocked(s, g); err != nil {
		return nil, err
	}
	return copyEvents(m.appendLocked(id, evs, m.now())), nil
}

// Events returns the events of a session after the sequence number, in order.
func (m *Memory) Events(_ context.Context, id uuid.UUID, after, limit int) ([]agent.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	limit = clampLimit(limit, 500, 1000)
	var out []agent.Event
	for _, e := range m.events[id] {
		if e.Seq > after {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	return copyEvents(out), nil
}

// CreateApprovals stores approval requests of a running session held by
// owner, and appends events.
func (m *Memory) CreateApprovals(_ context.Context, sessionID uuid.UUID, owner string, approvals []agent.Approval, events []agent.EventInput) ([]agent.Approval, []agent.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	evs, err := normalizeEvents(events)
	if err != nil {
		return nil, nil, err
	}
	s, err := m.sessionLocked(sessionID)
	if err != nil {
		return nil, nil, err
	}
	if err := guardLocked(s, Guard{States: []agent.SessionState{agent.StateRunning}, LeaseOwner: owner}); err != nil {
		return nil, nil, err
	}
	now := m.now()
	// Validate the whole batch before any row is stored, so a refused batch
	// leaves no approval behind.
	seen := make(map[uuid.UUID]struct{}, len(approvals))
	for _, a := range approvals {
		if err := validateApproval(a); err != nil {
			return nil, nil, err
		}
		if _, dup := m.approvals[a.ID]; dup {
			return nil, nil, fmt.Errorf("%w: approval %s exists", ErrConflict, a.ID)
		}
		if _, dup := seen[a.ID]; dup {
			return nil, nil, fmt.Errorf("%w: approval %s is duplicated", ErrConflict, a.ID)
		}
		seen[a.ID] = struct{}{}
	}
	out := make([]agent.Approval, 0, len(approvals))
	for _, a := range approvals {
		a.State = agent.ApprovalPending
		a.SessionID = sessionID
		if a.RequestedAt.IsZero() {
			a.RequestedAt = now
		}
		a.Arguments = slices.Clone(nonEmptyJSON(a.Arguments))
		stored := a
		m.approvals[a.ID] = &stored
		out = append(out, copyApproval(&stored))
	}
	return out, copyEvents(m.appendLocked(sessionID, evs, now)), nil
}

// ResolveApproval resolves one pending approval for the owner. A resolution
// after the expiry answers ErrExpired, before and after the sweep.
func (m *Memory) ResolveApproval(_ context.Context, id uuid.UUID, r Resolution) (ResolveResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := resolutionValid(r.State); err != nil {
		return ResolveResult{}, err
	}
	evs, err := normalizeEvents(r.Events)
	if err != nil {
		return ResolveResult{}, err
	}
	a, ok := m.approvals[id]
	if !ok {
		return ResolveResult{}, fmt.Errorf("%w: approval %s", ErrNotFound, id)
	}
	s, err := m.sessionLocked(a.SessionID)
	if err != nil {
		return ResolveResult{}, err
	}
	if s.Principal != r.Owner {
		return ResolveResult{}, fmt.Errorf("%w: approval %s", ErrNotFound, id)
	}
	if a.State == agent.ApprovalExpired {
		return ResolveResult{}, fmt.Errorf("%w: approval %s", ErrExpired, id)
	}
	if s.State != agent.StateWaitingApproval {
		return ResolveResult{}, fmt.Errorf("%w: session state %s", ErrConflict, s.State)
	}
	now := m.now()
	if a.State != agent.ApprovalPending {
		return ResolveResult{}, fmt.Errorf("%w: approval is %s", ErrConflict, a.State)
	}
	if !a.ExpiresAt.After(now) {
		return ResolveResult{}, fmt.Errorf("%w: approval %s", ErrExpired, id)
	}
	a.State = r.State
	a.ResolvedBy = r.By
	a.ResolvedAt = &now
	a.Reason = r.Reason
	m.appendLocked(a.SessionID, evs, now)
	left := 0
	for _, other := range m.approvals {
		if other.SessionID == a.SessionID && other.State == agent.ApprovalPending {
			left++
		}
	}
	return ResolveResult{Approval: copyApproval(a), Session: copySession(s), PendingLeft: left}, nil
}

// ListApprovals returns one page of approvals, newest first.
func (m *Memory) ListApprovals(_ context.Context, q ApprovalQuery) (ApprovalPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cT, cID, hasCursor, err := decodeCursor(q.Cursor)
	if err != nil {
		return ApprovalPage{}, err
	}
	limit := clampLimit(q.Limit, 50, 200)
	var rows []agent.Approval
	for _, a := range m.approvals {
		if q.Principal != "" {
			s, ok := m.sessions[a.SessionID]
			if !ok || s.Principal != q.Principal {
				continue
			}
		}
		if q.SessionID != uuid.Nil && a.SessionID != q.SessionID {
			continue
		}
		if q.State != "" && a.State != q.State {
			continue
		}
		if q.TargetKind != "" && a.TargetKind != q.TargetKind {
			continue
		}
		if q.TargetRef != "" && a.TargetRef != q.TargetRef {
			continue
		}
		if hasCursor && !approvalBefore(*a, cT, cID) {
			continue
		}
		rows = append(rows, copyApproval(a))
	}
	sort.Slice(rows, func(i, j int) bool {
		return approvalBefore(rows[j], rows[i].RequestedAt, rows[i].ID)
	})
	page := ApprovalPage{}
	if len(rows) > limit {
		page.Items = rows[:limit]
		last := page.Items[limit-1]
		page.NextCursor = encodeCursor(last.RequestedAt, last.ID)
	} else {
		page.Items = rows
	}
	return page, nil
}

func approvalBefore(a agent.Approval, t time.Time, id uuid.UUID) bool {
	if !a.RequestedAt.Equal(t) {
		return a.RequestedAt.Before(t)
	}
	return a.ID.String() < id.String()
}

// ExpireDue expires the pending approvals past their expiry. A waiting
// session that loses its last pending approval ends as expired. It returns
// the sessions it ended.
func (m *Memory) ExpireDue(_ context.Context) ([]SweptSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	due := map[uuid.UUID][]*agent.Approval{}
	for _, a := range m.approvals {
		if a.State != agent.ApprovalPending || a.ExpiresAt.After(now) {
			continue
		}
		if s, ok := m.sessions[a.SessionID]; !ok || s.State != agent.StateWaitingApproval {
			continue
		}
		due[a.SessionID] = append(due[a.SessionID], a)
	}
	ids := make([]uuid.UUID, 0, len(due))
	for id := range due {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	if len(ids) > 100 {
		ids = ids[:100]
	}
	var ended []SweptSession
	for _, sid := range ids {
		s, ok := m.sessions[sid]
		if !ok || s.State != agent.StateWaitingApproval {
			continue
		}
		expired := due[sid]
		for _, a := range expired {
			a.State = agent.ApprovalExpired
			a.ResolvedAt = &now
		}
		s.State = agent.StateExpired
		s.EndedAt = &now
		s.LeaseOwner = ""
		s.LeaseExpiresAt = nil
		var evs []agent.EventInput
		for _, a := range expired {
			ev, err := agent.NewEvent(agent.EventApprovalResolved, agent.ApprovalResolvedPayload{
				ApprovalID: a.ID.String(), State: agent.ApprovalExpired,
			})
			if err != nil {
				return ended, err
			}
			evs = append(evs, ev)
		}
		denied, err := deniedEvents(m.denyPendingLocked(sid, "", endedApprovalReason(agent.StateExpired), now), "")
		if err != nil {
			return ended, err
		}
		evs = append(evs, denied...)
		done, _ := agent.NewEvent(agent.EventDone, agent.DonePayload{State: agent.StateExpired})
		evs = append(evs, done)
		out := m.appendLocked(sid, evs, now)
		ended = append(ended, SweptSession{ID: sid, Seq: out[len(out)-1].Seq})
	}
	return ended, nil
}

// SweepLeases fails the sessions whose lease expired, and the waiting sessions
// that lost their resume. It returns the sessions it failed.
func (m *Memory) SweepLeases(_ context.Context) ([]SweptSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	cut := now.Add(-LostResumeAfter)
	var expiredLease, lostResume []uuid.UUID
	for id, s := range m.sessions {
		switch {
		case s.State == agent.StateRunning && s.LeaseExpiresAt != nil && s.LeaseExpiresAt.Before(now):
			expiredLease = append(expiredLease, id)
		case s.State == agent.StateWaitingApproval && !m.hasOpenApprovalLocked(id) && m.lastActivityLocked(id, s).Before(cut):
			lostResume = append(lostResume, id)
		}
	}
	lost := make([]SweptSession, 0, len(expiredLease)+len(lostResume))
	for _, id := range firstN(expiredLease, 100) {
		s := m.sessions[id]
		s.State = agent.StateFailed
		s.EndedAt = &now
		s.LeaseOwner = ""
		s.LeaseExpiresAt = nil
		pre, err := deniedEvents(m.denyPendingLocked(id, "", endedApprovalReason(agent.StateFailed), now), "")
		if err != nil {
			return lost, err
		}
		out := m.appendLocked(id, append(pre, lostEvents(leaseExpiredMessage)...), now)
		lost = append(lost, SweptSession{ID: id, Seq: out[len(out)-1].Seq})
	}
	for _, id := range firstN(lostResume, 100) {
		s := m.sessions[id]
		s.State = agent.StateFailed
		s.EndedAt = &now
		out := m.appendLocked(id, lostEvents(resumeLostMessage), now)
		lost = append(lost, SweptSession{ID: id, Seq: out[len(out)-1].Seq})
	}
	return lost, nil
}

func firstN(ids []uuid.UUID, n int) []uuid.UUID {
	if len(ids) > n {
		return ids[:n]
	}
	return ids
}

// hasOpenApprovalLocked reports whether a session has a pending or expired
// approval.
func (m *Memory) hasOpenApprovalLocked(id uuid.UUID) bool {
	for _, a := range m.approvals {
		if a.SessionID == id && (a.State == agent.ApprovalPending || a.State == agent.ApprovalExpired) {
			return true
		}
	}
	return false
}

// lastActivityLocked returns the time of the last event, or the start time
// when the session has none.
func (m *Memory) lastActivityLocked(id uuid.UUID, s *agent.Session) time.Time {
	evs := m.events[id]
	if len(evs) == 0 {
		return s.StartedAt
	}
	return evs[len(evs)-1].CreatedAt
}

// Sweep runs ExpireDue, then SweepLeases.
func (m *Memory) Sweep(ctx context.Context) (expired, lost []SweptSession, err error) {
	return sweepBoth(ctx, m)
}

// GetSettings returns one scope's settings. A scope with no row gives the
// defaults with Stored false.
func (m *Memory) GetSettings(_ context.Context, scope string) (agent.Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !scopeValid(scope) {
		return agent.Settings{}, fmt.Errorf("%w: scope %q", ErrInvalid, scope)
	}
	st, ok := m.settings[scope]
	if !ok {
		return defaultSettings(scope), nil
	}
	return cloneSettings(*st), nil
}

// SetInstructionsSHA records the revision and appends the event under the same lock.
func (m *Memory) SetInstructionsSHA(_ context.Context, id uuid.UUID, owner, sha string, ev agent.EventInput) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	evs, err := normalizeEvents([]agent.EventInput{ev})
	if err != nil {
		return err
	}
	s, err := m.sessionLocked(id)
	if err != nil {
		return err
	}
	if err := guardLocked(s, Guard{States: []agent.SessionState{agent.StateRunning}, LeaseOwner: owner}); err != nil {
		return err
	}
	instr := sha
	s.InstructionsSHA = &instr
	m.appendLocked(id, evs, m.now())
	return nil
}

// PutTeamAgentUnattended sets or removes one TeamAgent's unattended grant on
// the global scope.
func (m *Memory) PutTeamAgentUnattended(_ context.Context, key string, grant *agent.UnattendedGrant, by string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.settings[agent.ScopeGlobal]
	if grant == nil {
		delete(st.TeamAgentUnattended, key)
	} else {
		if st.TeamAgentUnattended == nil {
			st.TeamAgentUnattended = map[string]agent.UnattendedGrant{}
		}
		g := *grant
		g.Tools = append([]string(nil), grant.Tools...)
		st.TeamAgentUnattended[key] = g
	}
	st.UpdatedAt = m.now()
	st.UpdatedBy = by
	return nil
}

// PutSettings applies a patch to one scope and returns the result.
func (m *Memory) PutSettings(_ context.Context, scope string, p SettingsPatch, by string) (agent.Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !scopeValid(scope) {
		return agent.Settings{}, fmt.Errorf("%w: scope %q", ErrInvalid, scope)
	}
	if err := validatePatch(scope, p); err != nil {
		return agent.Settings{}, err
	}
	now := m.now()
	base := defaultSettings(scope)
	if cur, ok := m.settings[scope]; ok {
		base = *cur
	}
	next := cloneSettings(base)
	applyPatch(&next, scope, p, by, now)
	if p.Check != nil {
		if err := p.Check(cloneSettings(next)); err != nil {
			return agent.Settings{}, err
		}
	}
	m.settings[scope] = &next
	return cloneSettings(next), nil
}

// defaultSettings returns the settings of a scope with no row. A user scope
// is enabled and shares logs, and the global scope is not paused.
func defaultSettings(scope string) agent.Settings {
	d := agent.Settings{Scope: scope, Enabled: true, DataSharing: agent.DataLogs, TenantDataSharing: map[string]agent.DataSharing{}}
	if scope == agent.ScopeGlobal {
		d.Enabled = false
		d.DataSharing = ""
	}
	return d
}

func cloneSettings(s agent.Settings) agent.Settings {
	out := s
	out.TenantDataSharing = map[string]agent.DataSharing{}
	for k, v := range s.TenantDataSharing {
		out.TenantDataSharing[k] = v
	}
	if s.TeamAgentUnattended != nil {
		out.TeamAgentUnattended = make(map[string]agent.UnattendedGrant, len(s.TeamAgentUnattended))
		for k, g := range s.TeamAgentUnattended {
			g.Tools = append([]string(nil), g.Tools...)
			out.TeamAgentUnattended[k] = g
		}
	}
	return out
}
