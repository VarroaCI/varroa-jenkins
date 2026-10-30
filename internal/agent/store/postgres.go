package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/pgdial"
)

// openCheckTimeout bounds the schema check that Open runs on a fresh pool.
// A silent database fails here instead of holding the caller for the
// kernel's connect timeout.
const openCheckTimeout = 10 * time.Second

// Postgres is the agent store on the record-store database. It connects as
// the varroa_client role, which holds INSERT and UPDATE on the agent tables.
type Postgres struct {
	pool *pgxpool.Pool
	o    Options
}

var _ Store = (*Postgres)(nil)

// Open connects to the record-store database and checks its schema version.
// It closes the pool and returns the error when the schema is too old or
// the database does not answer within openCheckTimeout.
func Open(ctx context.Context, dsn string, o Options) (*Postgres, error) {
	cfg, err := pgdial.PoolConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse agent store dsn: %w", err)
	}
	if o.MaxConns > 0 {
		cfg.MaxConns = o.MaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open agent store pool: %w", err)
	}
	p := &Postgres{pool: pool, o: o}
	checkCtx, cancel := context.WithTimeout(ctx, openCheckTimeout)
	defer cancel()
	if err := p.CheckSchema(checkCtx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

// CheckSchema reads the applied schema version and refuses a version below
// MinSchemaVersion.
func (p *Postgres) CheckSchema(ctx context.Context) error {
	var version int64
	err := p.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(version_id), 0) FROM varroa.goose_db_version WHERE is_applied`,
	).Scan(&version)
	if err != nil {
		return fmt.Errorf("%w: read schema version: %v", ErrSchemaTooOld, err) //nolint:errorlint // the sentinel is the one caller-visible error
	}
	return checkSchemaVersion(version)
}

// Close closes the pool.
func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) now() time.Time { return p.o.now() }

// rowQuerier is the subset of pgx that both a pool and a transaction provide.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// lockSession takes the session row lock and returns the state, the lease
// owner, and the principal. A missing row gives ErrNotFound.
func lockSession(ctx context.Context, tx pgx.Tx, id uuid.UUID) (agent.SessionState, string, string, error) {
	var state, owner, principal string
	err := tx.QueryRow(ctx,
		`SELECT state, COALESCE(lease_owner, ''), principal FROM varroa.agent_sessions WHERE id = $1 FOR UPDATE`,
		id,
	).Scan(&state, &owner, &principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	if err != nil {
		return "", "", "", fmt.Errorf("lock session: %w", err)
	}
	return agent.SessionState(state), owner, principal, nil
}

// guardTx applies a Guard to a locked session row.
func guardTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, g Guard) error {
	state, owner, _, err := lockSession(ctx, tx, id)
	if err != nil {
		return err
	}
	return g.check(state, owner)
}

// appendEvents adds events to a session. The caller holds the session row
// lock, so the sequence numbers stay gapless.
func appendEvents(ctx context.Context, tx pgx.Tx, id uuid.UUID, in []agent.EventInput, now time.Time) ([]agent.Event, error) {
	if len(in) == 0 {
		return nil, nil
	}
	var maxSeq int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM varroa.agent_session_events WHERE session_id = $1`, id,
	).Scan(&maxSeq); err != nil {
		return nil, fmt.Errorf("read event sequence: %w", err)
	}
	out := make([]agent.Event, 0, len(in))
	for _, e := range in {
		maxSeq++
		if _, err := tx.Exec(ctx, `
			INSERT INTO varroa.agent_session_events (session_id, seq, type, payload, created_at)
			VALUES ($1, $2, $3, $4::jsonb, $5)`,
			id, maxSeq, string(e.Type), string(e.Payload), now,
		); err != nil {
			return nil, fmt.Errorf("insert event: %w", err)
		}
		out = append(out, agent.Event{SessionID: id, Seq: maxSeq, Type: e.Type, Payload: e.Payload, CreatedAt: now})
	}
	return out, nil
}

// nullable returns nil for an empty string, so the column stores NULL.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// update runs one state-changing statement and checks that it changed a row.
func update(ctx context.Context, tx pgx.Tx, sql string, args ...any) error {
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: update changed %d rows", ErrConflict, tag.RowsAffected())
	}
	return nil
}

// sessionCols lists the columns of agent_sessions in the order scanSession reads.
const sessionCols = `id, kind, principal, tenant, cluster, trigger, context, provider, model,
  key_source, data_sharing, instructions_sha, tenant_agent_generation, state,
  COALESCE(skip_reason, ''), prompt_tokens, completion_tokens, steps,
  COALESCE(lease_owner, ''), lease_expires_at, started_at, ended_at`

// scanSession reads one row of sessionCols.
func scanSession(row pgx.Row) (agent.Session, error) {
	var s agent.Session
	var kind, keySource, dataSharing, state, skip string
	var ctxRaw []byte
	err := row.Scan(&s.ID, &kind, &s.Principal, &s.Tenant, &s.Cluster, &s.Trigger, &ctxRaw,
		&s.Provider, &s.Model, &keySource, &dataSharing, &s.InstructionsSHA,
		&s.TenantAgentGeneration, &state, &skip, &s.PromptTokens, &s.CompletionTokens, &s.Steps,
		&s.LeaseOwner, &s.LeaseExpiresAt, &s.StartedAt, &s.EndedAt)
	if err != nil {
		return agent.Session{}, err
	}
	s.Kind = agent.SessionKind(kind)
	s.KeySource = agent.KeySource(keySource)
	s.DataSharing = agent.DataSharing(dataSharing)
	s.State = agent.SessionState(state)
	s.SkipReason = agent.SkipReason(skip)
	s.Context = map[string]any{}
	if len(ctxRaw) > 0 {
		if err := json.Unmarshal(ctxRaw, &s.Context); err != nil {
			return agent.Session{}, fmt.Errorf("decode session context: %w", err)
		}
	}
	return s, nil
}

// readSession returns one session by id.
func readSession(ctx context.Context, q rowQuerier, id uuid.UUID) (agent.Session, error) {
	row := q.QueryRow(ctx, `SELECT `+sessionCols+` FROM varroa.agent_sessions WHERE id = $1`, id)
	s, err := scanSession(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return agent.Session{}, fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	if err != nil {
		return agent.Session{}, fmt.Errorf("read session: %w", err)
	}
	return s, nil
}

// CreateSession inserts a session row and its events in one transaction.
func (p *Postgres) CreateSession(ctx context.Context, s agent.Session, events []agent.EventInput) (agent.Session, []agent.Event, error) {
	now := p.now()
	s, err := fillSession(s, now)
	if err != nil {
		return agent.Session{}, nil, err
	}
	evs, err := normalizeEvents(events)
	if err != nil {
		return agent.Session{}, nil, err
	}
	var out []agent.Event
	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var err error
		s, out, err = createSessionTx(ctx, tx, s, evs, now)
		return err
	})
	if err != nil {
		return agent.Session{}, nil, err
	}
	return s, out, nil
}

// GetSession returns one session.
func (p *Postgres) GetSession(ctx context.Context, id uuid.UUID) (agent.Session, error) {
	return readSession(ctx, p.pool, id)
}

// TakeLease starts or resumes a session under owner.
func (p *Postgres) TakeLease(ctx context.Context, id uuid.UUID, owner string, from []agent.SessionState) (agent.Session, error) {
	if len(from) == 0 {
		return agent.Session{}, fmt.Errorf("%w: take lease needs a from state", ErrInvalid)
	}
	states := make([]string, 0, len(from))
	for _, s := range from {
		states = append(states, string(s))
	}
	expires := p.now().Add(LeaseTTL)
	var out agent.Session
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, _, _, err := lockSession(ctx, tx, id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE varroa.agent_sessions
			SET state = 'running', lease_owner = $2, lease_expires_at = $3, ended_at = NULL
			WHERE id = $1 AND state = ANY($4::text[]) AND (lease_owner IS NULL OR lease_owner = $2)`,
			id, owner, expires, states,
		)
		if err != nil {
			return fmt.Errorf("take lease: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: lease not available", ErrConflict)
		}
		out, err = readSession(ctx, tx, id)
		return err
	})
	return out, err
}

// RenewLease extends the lease of a running session held by owner.
func (p *Postgres) RenewLease(ctx context.Context, id uuid.UUID, owner string) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE varroa.agent_sessions SET lease_expires_at = $3
		WHERE id = $1 AND state = 'running' AND lease_owner = $2 AND lease_expires_at > $4`,
		id, owner, p.now().Add(LeaseTTL), p.now(),
	)
	if err != nil {
		return fmt.Errorf("renew lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: lease not held", ErrConflict)
	}
	return nil
}

// ClearLease parks a running session that owner holds.
func (p *Postgres) ClearLease(ctx context.Context, id uuid.UUID, owner string, events []agent.EventInput) ([]agent.Event, error) {
	evs, err := normalizeEvents(events)
	if err != nil {
		return nil, err
	}
	now := p.now()
	var out []agent.Event
	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if err := guardTx(ctx, tx, id, Guard{States: []agent.SessionState{agent.StateRunning}, LeaseOwner: owner}); err != nil {
			return err
		}
		if err := update(ctx, tx, `
			UPDATE varroa.agent_sessions SET state = 'waiting_approval', lease_owner = NULL, lease_expires_at = NULL
			WHERE id = $1 AND state = 'running' AND lease_owner = $2`, id, owner); err != nil {
			return err
		}
		var err error
		out, err = appendEvents(ctx, tx, id, evs, now)
		return err
	})
	return out, err
}

// RecordStep adds one provider call to a running session held by owner.
func (p *Postgres) RecordStep(ctx context.Context, id uuid.UUID, owner string, prompt, completion int64) (agent.Session, error) {
	var out agent.Session
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if err := guardTx(ctx, tx, id, Guard{States: []agent.SessionState{agent.StateRunning}, LeaseOwner: owner}); err != nil {
			return err
		}
		if err := update(ctx, tx, `
			UPDATE varroa.agent_sessions
			SET prompt_tokens = prompt_tokens + $3, completion_tokens = completion_tokens + $4, steps = steps + 1
			WHERE id = $1 AND state = 'running' AND lease_owner = $2`, id, owner, prompt, completion); err != nil {
			return err
		}
		var err error
		out, err = readSession(ctx, tx, id)
		return err
	})
	return out, err
}

// EndSession ends a session with a final state and clears its lease. It
// denies every pending approval of the session in the same transaction.
func (p *Postgres) EndSession(ctx context.Context, id uuid.UUID, in EndInput) ([]agent.Event, error) {
	if !endable(in.State) {
		return nil, fmt.Errorf("%w: end state %q", ErrInvalid, in.State)
	}
	evs, err := normalizeEvents(in.Events)
	if err != nil {
		return nil, err
	}
	now := p.now()
	var out []agent.Event
	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		state, owner, _, err := lockSession(ctx, tx, id)
		if err != nil {
			return err
		}
		if state.Terminal() || (len(in.From) > 0 && !containsState(in.From, state)) {
			return fmt.Errorf("%w: session state %s", ErrConflict, state)
		}
		if in.LeaseOwner != "" && owner != in.LeaseOwner {
			return fmt.Errorf("%w: lease owner changed", ErrConflict)
		}
		if err := update(ctx, tx, `
			UPDATE varroa.agent_sessions
			SET state = $2, ended_at = $3, lease_owner = NULL, lease_expires_at = NULL
			WHERE id = $1 AND state NOT IN ('completed','failed','cancelled','skipped','expired')`,
			id, string(in.State), now); err != nil {
			return err
		}
		ids, err := denyPendingApprovals(ctx, tx, id, in.By, endedApprovalReason(in.State), now)
		if err != nil {
			return err
		}
		pre, err := deniedEvents(ids, in.By)
		if err != nil {
			return err
		}
		out, err = appendEvents(ctx, tx, id, append(pre, evs...), now)
		return err
	})
	return out, err
}

// AppendEvent appends events to a session when the guard passes.
func (p *Postgres) AppendEvent(ctx context.Context, id uuid.UUID, g Guard, events ...agent.EventInput) ([]agent.Event, error) {
	evs, err := normalizeEvents(events)
	if err != nil {
		return nil, err
	}
	now := p.now()
	var out []agent.Event
	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if err := guardTx(ctx, tx, id, g); err != nil {
			return err
		}
		var err error
		out, err = appendEvents(ctx, tx, id, evs, now)
		return err
	})
	return out, err
}

// Events returns the events of a session after the sequence number.
func (p *Postgres) Events(ctx context.Context, id uuid.UUID, after, limit int) ([]agent.Event, error) {
	limit = clampLimit(limit, 500, 1000)
	rows, err := p.pool.Query(ctx, `
		SELECT seq, type, payload, created_at FROM varroa.agent_session_events
		WHERE session_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`, id, after, limit)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()
	var out []agent.Event
	for rows.Next() {
		var e agent.Event
		var typ string
		if err := rows.Scan(&e.Seq, &typ, &e.Payload, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.SessionID = id
		e.Type = agent.EventType(typ)
		out = append(out, e)
	}
	return out, rows.Err()
}
