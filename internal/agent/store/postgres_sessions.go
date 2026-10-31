package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// daySpendSQL sums the tokens of one principal's sessions that started on
// the day given by $2 and before $3. It reads through the index
// agent_sessions_principal_started_idx.
const daySpendSQL = `
SELECT COALESCE(SUM(prompt_tokens + completion_tokens), 0)::bigint
FROM varroa.agent_sessions
WHERE principal = $1 AND started_at >= $2 AND started_at < $3`

// ListSessions returns one page of sessions, newest first.
func (p *Postgres) ListSessions(ctx context.Context, q SessionQuery) (SessionPage, error) {
	if err := p.requireSchema(ctx); err != nil {
		return SessionPage{}, err
	}
	cT, cID, hasCursor, err := decodeCursor(q.Cursor)
	if err != nil {
		return SessionPage{}, err
	}
	limit := clampLimit(q.Limit, 50, 200)
	rows, err := p.pool.Query(ctx, `
		SELECT `+sessionCols+`
		FROM varroa.agent_sessions
		WHERE ($1 = '' OR principal = $1)
		  AND ($2 = '' OR kind = $2)
		  AND ($3 = '' OR context->>'prompt' = $3)
		  AND ($4 = '' OR tenant = $4)
		  AND ($5 = '' OR state = $5)
		  AND (NOT $6::boolean OR (started_at, id) < ($7::timestamptz, $8::uuid))
		ORDER BY started_at DESC, id DESC
		LIMIT $9`,
		q.Principal, string(q.Kind), q.Prompt, q.Tenant, string(q.State),
		hasCursor, cT, cID, limit+1,
	)
	if err != nil {
		return SessionPage{}, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var items []agent.Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return SessionPage{}, fmt.Errorf("scan session: %w", err)
		}
		items = append(items, s)
	}
	if err := rows.Err(); err != nil {
		return SessionPage{}, fmt.Errorf("list sessions: %w", err)
	}
	page := SessionPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = encodeCursor(last.StartedAt, last.ID)
	}
	return page, nil
}

// DaySpend returns the tokens that the principal spent on the current UTC day.
func (p *Postgres) DaySpend(ctx context.Context, principal string) (int64, error) {
	if err := p.requireSchema(ctx); err != nil {
		return 0, err
	}
	start := startOfDay(p.now())
	var total int64
	if err := p.pool.QueryRow(ctx, daySpendSQL, principal, start, start.Add(24*time.Hour)).Scan(&total); err != nil {
		return 0, fmt.Errorf("day spend: %w", err)
	}
	return total, nil
}

// CountRunning returns the number of running sessions of the principal.
func (p *Postgres) CountRunning(ctx context.Context, principal string) (int, error) {
	if err := p.requireSchema(ctx); err != nil {
		return 0, err
	}
	var n int
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM varroa.agent_sessions WHERE principal = $1 AND state = 'running'`, principal,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count running sessions: %w", err)
	}
	return n, nil
}

// uuidOrNil is the parameter form of a session id filter. The zero UUID
// matches no session, so an unset filter is passed as nil.
func uuidOrNil(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}
