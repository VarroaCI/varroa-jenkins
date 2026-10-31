package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/pgdial"
)

// DefaultCallTimeout bounds each store call. The connect timeout fails first
// on a new connection. This deadline ends a ping, a query, or a lock wait on
// a pooled connection that does not answer. A DSN that sets a longer
// connect_timeout must also use a longer bound, or the deadline ends the dial.
const DefaultCallTimeout = pgdial.DefaultConnectTimeout + 3*time.Second

// errCallDeadline is the cause of the per-call deadline. A deadline that the
// caller set earlier has another cause, so it does not read as unreachable.
var errCallDeadline = errors.New("agent store call deadline")

// WithTimeout returns a Store that bounds each call of inner by d. A call
// that hits the bound answers ErrUnavailable.
func WithTimeout(inner Store, d time.Duration) Store {
	return &timeoutStore{inner: inner, d: d}
}

// timeoutStore implements Store with a per-call deadline. It does not embed
// Store, so each method must name its bound.
type timeoutStore struct {
	inner Store
	d     time.Duration
}

var _ Store = (*timeoutStore)(nil)

// classify maps an inner error to the Store contract. It keeps nil and the
// sentinel errors as they are. The per-call deadline, or an unreachable
// database, becomes ErrUnavailable. A caller's own cancel or deadline passes
// through, because the store may be healthy.
func classify(ctx context.Context, err error) error {
	if err == nil || errors.Is(err, ErrUnavailable) {
		return err
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) || errors.Is(err, ErrExpired) ||
		errors.Is(err, ErrInvalid) || errors.Is(err, ErrSchemaTooOld) {
		return err
	}
	if errors.Is(context.Cause(ctx), errCallDeadline) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if ctx.Err() != nil {
		return err
	}
	if pgdial.IsUnreachable(err) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return err
}

// CreateSession bounds the inner call with the per-call deadline.
func (t *timeoutStore) CreateSession(ctx context.Context, s agent.Session, events []agent.EventInput) (agent.Session, []agent.Event, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, v1, err := t.inner.CreateSession(ctx, s, events)
	return v0, v1, classify(ctx, err)
}

// GetSession bounds the inner call with the per-call deadline.
func (t *timeoutStore) GetSession(ctx context.Context, id uuid.UUID) (agent.Session, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, err := t.inner.GetSession(ctx, id)
	return v0, classify(ctx, err)
}

// GetApproval bounds the inner call with the per-call deadline.
func (t *timeoutStore) GetApproval(ctx context.Context, id uuid.UUID) (agent.Approval, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, err := t.inner.GetApproval(ctx, id)
	return v0, classify(ctx, err)
}

// ListSessions bounds the inner call with the per-call deadline.
func (t *timeoutStore) ListSessions(ctx context.Context, q SessionQuery) (SessionPage, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, err := t.inner.ListSessions(ctx, q)
	return v0, classify(ctx, err)
}

// DaySpend bounds the inner call with the per-call deadline.
func (t *timeoutStore) DaySpend(ctx context.Context, principal string) (int64, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, err := t.inner.DaySpend(ctx, principal)
	return v0, classify(ctx, err)
}

// CountRunning bounds the inner call with the per-call deadline.
func (t *timeoutStore) CountRunning(ctx context.Context, principal string) (int, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, err := t.inner.CountRunning(ctx, principal)
	return v0, classify(ctx, err)
}

// TakeLease bounds the inner call with the per-call deadline.
func (t *timeoutStore) TakeLease(ctx context.Context, id uuid.UUID, owner string, from []agent.SessionState) (agent.Session, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, err := t.inner.TakeLease(ctx, id, owner, from)
	return v0, classify(ctx, err)
}

// RenewLease bounds the inner call with the per-call deadline.
func (t *timeoutStore) RenewLease(ctx context.Context, id uuid.UUID, owner string) error {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	err := t.inner.RenewLease(ctx, id, owner)
	return classify(ctx, err)
}

// ClearLease bounds the inner call with the per-call deadline.
func (t *timeoutStore) ClearLease(ctx context.Context, id uuid.UUID, owner string, events []agent.EventInput) ([]agent.Event, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, err := t.inner.ClearLease(ctx, id, owner, events)
	return v0, classify(ctx, err)
}

// RecordStep bounds the inner call with the per-call deadline.
func (t *timeoutStore) RecordStep(ctx context.Context, id uuid.UUID, owner string, prompt, completion int64) (agent.Session, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, err := t.inner.RecordStep(ctx, id, owner, prompt, completion)
	return v0, classify(ctx, err)
}

// EndSession bounds the inner call with the per-call deadline.
func (t *timeoutStore) EndSession(ctx context.Context, id uuid.UUID, in EndInput) ([]agent.Event, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, err := t.inner.EndSession(ctx, id, in)
	return v0, classify(ctx, err)
}

// AppendEvent bounds the inner call with the per-call deadline.
func (t *timeoutStore) AppendEvent(ctx context.Context, id uuid.UUID, g Guard, events ...agent.EventInput) ([]agent.Event, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, err := t.inner.AppendEvent(ctx, id, g, events...)
	return v0, classify(ctx, err)
}

// Events bounds the inner call with the per-call deadline.
func (t *timeoutStore) Events(ctx context.Context, id uuid.UUID, after, limit int) ([]agent.Event, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, err := t.inner.Events(ctx, id, after, limit)
	return v0, classify(ctx, err)
}

// CreateApprovals bounds the inner call with the per-call deadline.
func (t *timeoutStore) CreateApprovals(ctx context.Context, sessionID uuid.UUID, owner string, approvals []agent.Approval, events []agent.EventInput) ([]agent.Approval, []agent.Event, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, v1, err := t.inner.CreateApprovals(ctx, sessionID, owner, approvals, events)
	return v0, v1, classify(ctx, err)
}

// ResolveApproval bounds the inner call with the per-call deadline.
func (t *timeoutStore) ResolveApproval(ctx context.Context, id uuid.UUID, r Resolution) (ResolveResult, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, err := t.inner.ResolveApproval(ctx, id, r)
	return v0, classify(ctx, err)
}

// ListApprovals bounds the inner call with the per-call deadline.
func (t *timeoutStore) ListApprovals(ctx context.Context, q ApprovalQuery) (ApprovalPage, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, err := t.inner.ListApprovals(ctx, q)
	return v0, classify(ctx, err)
}

// ExpireDue bounds the inner call with the per-call deadline.
func (t *timeoutStore) ExpireDue(ctx context.Context) ([]SweptSession, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, err := t.inner.ExpireDue(ctx)
	return v0, classify(ctx, err)
}

// SweepLeases bounds the inner call with the per-call deadline.
func (t *timeoutStore) SweepLeases(ctx context.Context) ([]SweptSession, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, err := t.inner.SweepLeases(ctx)
	return v0, classify(ctx, err)
}

// Sweep bounds the inner call with the per-call deadline.
func (t *timeoutStore) Sweep(ctx context.Context) (expired, lost []SweptSession, err error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, v1, err := t.inner.Sweep(ctx)
	return v0, v1, classify(ctx, err)
}

// GetSettings bounds the inner call with the per-call deadline.
func (t *timeoutStore) GetSettings(ctx context.Context, scope string) (agent.Settings, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, err := t.inner.GetSettings(ctx, scope)
	return v0, classify(ctx, err)
}

// PutSettings bounds the inner call with the per-call deadline.
func (t *timeoutStore) PutSettings(ctx context.Context, scope string, p SettingsPatch, by string) (agent.Settings, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	v0, err := t.inner.PutSettings(ctx, scope, p, by)
	return v0, classify(ctx, err)
}

// SetInstructionsSHA bounds the inner call with the per-call deadline.
func (t *timeoutStore) SetInstructionsSHA(ctx context.Context, id uuid.UUID, owner, sha string, ev agent.EventInput) error {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	err := t.inner.SetInstructionsSHA(ctx, id, owner, sha, ev)
	return classify(ctx, err)
}

// PutTenantAgentUnattended bounds the inner call with the per-call deadline.
func (t *timeoutStore) PutTenantAgentUnattended(ctx context.Context, key string, grant *agent.UnattendedGrant, by string) error {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errCallDeadline)
	defer cancel()
	err := t.inner.PutTenantAgentUnattended(ctx, key, grant, by)
	return classify(ctx, err)
}
