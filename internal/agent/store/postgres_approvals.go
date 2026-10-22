package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// approvalCols lists the columns of agent_approvals in the order scanApproval reads.
const approvalCols = `id, session_id, call_id, tool, arguments, target_kind, target_ref, state,
  requested_at, expires_at, COALESCE(resolved_by, ''), resolved_at, COALESCE(reason, '')`

// scanApproval reads one row of approvalCols.
func scanApproval(row pgx.Row) (agent.Approval, error) {
	var a agent.Approval
	var targetKind, state string
	err := row.Scan(&a.ID, &a.SessionID, &a.CallID, &a.Tool, &a.Arguments, &targetKind, &a.TargetRef,
		&state, &a.RequestedAt, &a.ExpiresAt, &a.ResolvedBy, &a.ResolvedAt, &a.Reason)
	if err != nil {
		return agent.Approval{}, err
	}
	a.TargetKind = agent.TargetKind(targetKind)
	a.State = agent.ApprovalState(state)
	return a, nil
}

// readApproval returns one approval by id.
func readApproval(ctx context.Context, q rowQuerier, id uuid.UUID) (agent.Approval, error) {
	a, err := scanApproval(q.QueryRow(ctx, `SELECT `+approvalCols+` FROM varroa.agent_approvals WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return agent.Approval{}, fmt.Errorf("%w: approval %s", ErrNotFound, id)
	}
	if err != nil {
		return agent.Approval{}, fmt.Errorf("read approval: %w", err)
	}
	return a, nil
}

// GetApproval returns one approval.
func (p *Postgres) GetApproval(ctx context.Context, id uuid.UUID) (agent.Approval, error) {
	return readApproval(ctx, p.pool, id)
}

// CreateApprovals stores approval requests of a running session held by
// owner, and appends events.
func (p *Postgres) CreateApprovals(ctx context.Context, sessionID uuid.UUID, owner string, approvals []agent.Approval, events []agent.EventInput) ([]agent.Approval, []agent.Event, error) {
	evs, err := normalizeEvents(events)
	if err != nil {
		return nil, nil, err
	}
	now := p.now()
	for _, a := range approvals {
		if err := validateApproval(a); err != nil {
			return nil, nil, err
		}
	}
	var outA []agent.Approval
	var outE []agent.Event
	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if err := guardTx(ctx, tx, sessionID, Guard{States: []agent.SessionState{agent.StateRunning}, LeaseOwner: owner}); err != nil {
			return err
		}
		outA = make([]agent.Approval, 0, len(approvals))
		for _, a := range approvals {
			requested := a.RequestedAt
			if requested.IsZero() {
				requested = now
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO varroa.agent_approvals (id, session_id, call_id, tool, arguments, target_kind,
				  target_ref, state, requested_at, expires_at)
				VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, 'pending', $8, $9)`,
				a.ID, sessionID, a.CallID, a.Tool, string(nonEmptyJSON(a.Arguments)), string(a.TargetKind),
				a.TargetRef, requested, a.ExpiresAt,
			); err != nil {
				return fmt.Errorf("insert approval: %w", err)
			}
			a.State = agent.ApprovalPending
			a.SessionID = sessionID
			a.RequestedAt = requested
			outA = append(outA, a)
		}
		var err error
		outE, err = appendEvents(ctx, tx, sessionID, evs, now)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return outA, outE, nil
}

// nonEmptyJSON returns {} for an empty argument document.
func nonEmptyJSON(b []byte) []byte {
	if len(b) == 0 {
		return []byte("{}")
	}
	return b
}

// ResolveApproval resolves one pending approval for the owner.
func (p *Postgres) ResolveApproval(ctx context.Context, id uuid.UUID, r Resolution) (ResolveResult, error) {
	if err := resolutionValid(r.State); err != nil {
		return ResolveResult{}, err
	}
	evs, err := normalizeEvents(r.Events)
	if err != nil {
		return ResolveResult{}, err
	}
	var res ResolveResult
	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var sessionID uuid.UUID
		err := tx.QueryRow(ctx, `SELECT session_id FROM varroa.agent_approvals WHERE id = $1`, id).Scan(&sessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: approval %s", ErrNotFound, id)
		}
		if err != nil {
			return fmt.Errorf("find approval session: %w", err)
		}
		state, _, principal, err := lockSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if principal != r.Owner {
			return fmt.Errorf("%w: approval %s", ErrNotFound, id)
		}
		if state != agent.StateWaitingApproval {
			return fmt.Errorf("%w: session state %s", ErrConflict, state)
		}
		now := p.now()
		tag, err := tx.Exec(ctx, `
			UPDATE varroa.agent_approvals
			SET state = $2, resolved_by = $3, resolved_at = $4, reason = $5
			WHERE id = $1 AND state = 'pending' AND expires_at > $4`,
			id, string(r.State), r.By, now, nullable(r.Reason),
		)
		if err != nil {
			return fmt.Errorf("resolve approval: %w", err)
		}
		if tag.RowsAffected() != 1 {
			cur, err := readApproval(ctx, tx, id)
			if err != nil {
				return err
			}
			if cur.State == agent.ApprovalPending {
				return fmt.Errorf("%w: approval %s", ErrExpired, id)
			}
			return fmt.Errorf("%w: approval is %s", ErrConflict, cur.State)
		}
		if _, err := appendEvents(ctx, tx, sessionID, evs, now); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM varroa.agent_approvals WHERE session_id = $1 AND state = 'pending'`,
			sessionID,
		).Scan(&res.PendingLeft); err != nil {
			return fmt.Errorf("count pending approvals: %w", err)
		}
		if res.Approval, err = readApproval(ctx, tx, id); err != nil {
			return err
		}
		res.Session, err = readSession(ctx, tx, sessionID)
		return err
	})
	if err != nil {
		return ResolveResult{}, err
	}
	return res, nil
}

// ListApprovals returns one page of approvals, newest first.
func (p *Postgres) ListApprovals(ctx context.Context, q ApprovalQuery) (ApprovalPage, error) {
	cT, cID, hasCursor, err := decodeCursor(q.Cursor)
	if err != nil {
		return ApprovalPage{}, err
	}
	limit := clampLimit(q.Limit, 50, 200)
	rows, err := p.pool.Query(ctx, `
		SELECT a.id, a.session_id, a.call_id, a.tool, a.arguments, a.target_kind, a.target_ref, a.state,
		  a.requested_at, a.expires_at, COALESCE(a.resolved_by, ''), a.resolved_at, COALESCE(a.reason, '')
		FROM varroa.agent_approvals a
		JOIN varroa.agent_sessions s ON s.id = a.session_id
		WHERE ($1 = '' OR s.principal = $1)
		  AND ($2::uuid IS NULL OR a.session_id = $2::uuid)
		  AND ($3 = '' OR a.state = $3)
		  AND ($4 = '' OR a.target_kind = $4)
		  AND ($5 = '' OR a.target_ref = $5)
		  AND (NOT $6::boolean OR (a.requested_at, a.id) < ($7::timestamptz, $8::uuid))
		ORDER BY a.requested_at DESC, a.id DESC
		LIMIT $9`,
		q.Principal, uuidOrNil(q.SessionID), string(q.State), string(q.TargetKind), q.TargetRef,
		hasCursor, cT, cID, limit+1,
	)
	if err != nil {
		return ApprovalPage{}, fmt.Errorf("list approvals: %w", err)
	}
	defer rows.Close()
	var items []agent.Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return ApprovalPage{}, fmt.Errorf("scan approval: %w", err)
		}
		items = append(items, a)
	}
	if err := rows.Err(); err != nil {
		return ApprovalPage{}, fmt.Errorf("list approvals: %w", err)
	}
	page := ApprovalPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = encodeCursor(last.RequestedAt, last.ID)
	}
	return page, nil
}

// ExpireDue expires the pending approvals past their expiry, one session per
// transaction. It returns the sessions it ended.
func (p *Postgres) ExpireDue(ctx context.Context) ([]SweptSession, error) {
	now := p.now()
	rows, err := p.pool.Query(ctx, `
		SELECT DISTINCT session_id FROM varroa.agent_approvals
		WHERE state = 'pending' AND expires_at <= $1
		LIMIT 100`, now)
	if err != nil {
		return nil, fmt.Errorf("select due approvals: %w", err)
	}
	var sessions []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan due approval: %w", err)
		}
		sessions = append(sessions, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select due approvals: %w", err)
	}

	var ended []SweptSession
	for _, sid := range sessions {
		var seq int
		did := false
		err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
			if _, _, _, err := lockSession(ctx, tx, sid); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `
				UPDATE varroa.agent_sessions SET state = 'expired', ended_at = $2, lease_owner = NULL, lease_expires_at = NULL
				WHERE id = $1 AND state = 'waiting_approval'`, sid, now)
			if err != nil {
				return fmt.Errorf("expire session: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return nil
			}
			expired, err := expireApprovals(ctx, tx, sid, now)
			if err != nil {
				return err
			}
			var evs []agent.EventInput
			for _, id := range expired {
				ev, err := agent.NewEvent(agent.EventApprovalResolved, agent.ApprovalResolvedPayload{
					ApprovalID: id.String(), State: agent.ApprovalExpired,
				})
				if err != nil {
					return err
				}
				evs = append(evs, ev)
			}
			done, err := agent.NewEvent(agent.EventDone, agent.DonePayload{State: agent.StateExpired})
			if err != nil {
				return err
			}
			evs = append(evs, done)
			out, err := appendEvents(ctx, tx, sid, evs, now)
			if err != nil {
				return err
			}
			seq = out[len(out)-1].Seq
			did = true
			return nil
		})
		if err != nil {
			return ended, err
		}
		if did {
			ended = append(ended, SweptSession{ID: sid, Seq: seq})
		}
	}
	return ended, nil
}

// expireApprovals moves the due pending approvals of a session to expired and
// returns their ids.
func expireApprovals(ctx context.Context, tx pgx.Tx, sid uuid.UUID, now time.Time) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
		UPDATE varroa.agent_approvals SET state = 'expired', resolved_at = $2
		WHERE session_id = $1 AND state = 'pending' AND expires_at <= $2
		RETURNING id`, sid, now)
	if err != nil {
		return nil, fmt.Errorf("expire approvals: %w", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan expired approval: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SweepLeases fails the sessions whose lease expired, and the waiting sessions
// that lost their resume. It returns the sessions it failed.
func (p *Postgres) SweepLeases(ctx context.Context) ([]SweptSession, error) {
	now := p.now()
	cut := now.Add(-LostResumeAfter)
	const lostResumeCond = `
		s.state = 'waiting_approval'
		AND NOT EXISTS (SELECT 1 FROM varroa.agent_approvals a
		                WHERE a.session_id = s.id AND a.state IN ('pending', 'expired'))
		AND COALESCE((SELECT MAX(e.created_at) FROM varroa.agent_session_events e WHERE e.session_id = s.id),
		             s.started_at) < $2`
	rows, err := p.pool.Query(ctx, `
		SELECT s.id, 'lease' FROM varroa.agent_sessions s
		WHERE s.state = 'running' AND s.lease_expires_at < $1
		UNION ALL
		SELECT s.id, 'resume' FROM varroa.agent_sessions s
		WHERE `+lostResumeCond+`
		LIMIT 200`, now, cut)
	if err != nil {
		return nil, fmt.Errorf("select lost sessions: %w", err)
	}
	type candidate struct {
		id   uuid.UUID
		kind string
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.kind); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan lost session: %w", err)
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select lost sessions: %w", err)
	}

	var lost []SweptSession
	for _, c := range cands {
		var seq int
		did := false
		err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
			if _, _, _, err := lockSession(ctx, tx, c.id); err != nil {
				return err
			}
			msg := leaseExpiredMessage
			var n int64
			if c.kind == "lease" {
				tag, err := tx.Exec(ctx, `
					UPDATE varroa.agent_sessions SET state = 'failed', ended_at = $2, lease_owner = NULL, lease_expires_at = NULL
					WHERE id = $1 AND state = 'running' AND lease_expires_at < $2`, c.id, now)
				if err != nil {
					return fmt.Errorf("fail lease-lost session: %w", err)
				}
				n = tag.RowsAffected()
			} else {
				msg = resumeLostMessage
				tag, err := tx.Exec(ctx, `
					UPDATE varroa.agent_sessions s SET state = 'failed', ended_at = $1
					WHERE s.id = $3 AND `+lostResumeCond, now, cut, c.id)
				if err != nil {
					return fmt.Errorf("fail resume-lost session: %w", err)
				}
				n = tag.RowsAffected()
			}
			if n != 1 {
				return nil
			}
			out, err := appendEvents(ctx, tx, c.id, lostEvents(msg), now)
			if err != nil {
				return err
			}
			seq = out[len(out)-1].Seq
			did = true
			return nil
		})
		if err != nil {
			return lost, err
		}
		if did {
			lost = append(lost, SweptSession{ID: c.id, Seq: seq})
		}
	}
	return lost, nil
}

// Sweep runs ExpireDue, then SweepLeases.
func (p *Postgres) Sweep(ctx context.Context) (expired, lost []SweptSession, err error) {
	return sweepBoth(ctx, p)
}

// PutSettings is implemented in postgres_settings.go.
