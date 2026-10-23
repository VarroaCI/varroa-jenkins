package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// FromPool returns a store on an existing pool, such as the one the Tenant agent
// runner already holds. It checks the schema, as Open does. The caller owns the
// pool and closes it.
func FromPool(ctx context.Context, pool *pgxpool.Pool, o Options) (*Postgres, error) {
	p := &Postgres{pool: pool, o: o}
	if err := p.CheckSchema(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// Pool returns the pool the store runs on.
func (p *Postgres) Pool() *pgxpool.Pool { return p.pool }

// createSessionTx inserts one session row and its events in tx. The caller
// normalizes the events and sets now. It returns the session with its
// defaults filled and the stored events.
func createSessionTx(ctx context.Context, tx pgx.Tx, s agent.Session, evs []agent.EventInput, now time.Time) (agent.Session, []agent.Event, error) {
	ctxRaw, err := json.Marshal(s.Context)
	if err != nil {
		return agent.Session{}, nil, fmt.Errorf("%w: context: %v", ErrInvalid, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.agent_sessions (id, kind, principal, tenant, cluster, trigger, context,
		  provider, model, key_source, data_sharing, instructions_sha, tenant_agent_generation,
		  state, skip_reason, lease_owner, lease_expires_at, started_at, ended_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`,
		s.ID, string(s.Kind), s.Principal, s.Tenant, s.Cluster, s.Trigger, string(ctxRaw),
		s.Provider, s.Model, string(s.KeySource), string(s.DataSharing), s.InstructionsSHA,
		s.TenantAgentGeneration, string(s.State), nullable(string(s.SkipReason)),
		nullable(s.LeaseOwner), s.LeaseExpiresAt, s.StartedAt, s.EndedAt,
	); err != nil {
		return agent.Session{}, nil, fmt.Errorf("insert session: %w", err)
	}
	out, err := appendEvents(ctx, tx, s.ID, evs, now)
	if err != nil {
		return agent.Session{}, nil, err
	}
	return s, out, nil
}

// CreateUnderLock runs fn in one transaction that holds the advisory lock
// named by lockKey. Two creates with one key run one after the other, so a
// check made in fn cannot pass twice. When fn returns ok, the session it
// returns is inserted in the same transaction with the events it returns, and
// created is true. When ok is false, nothing is written.
//
// The store's clock fills the session defaults. The database clock is never
// read, so tests control the time.
func (p *Postgres) CreateUnderLock(ctx context.Context, lockKey string,
	fn func(ctx context.Context, tx pgx.Tx) (agent.Session, []agent.EventInput, bool, error),
) (agent.Session, []agent.Event, bool, error) {
	var out agent.Session
	var evs []agent.Event
	created := false
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
			return fmt.Errorf("take create lock: %w", err)
		}
		s, in, ok, err := fn(ctx, tx)
		if err != nil || !ok {
			return err
		}
		now := p.now()
		s, err = fillSession(s, now)
		if err != nil {
			return err
		}
		normalized, err := normalizeEvents(in)
		if err != nil {
			return err
		}
		out, evs, err = createSessionTx(ctx, tx, s, normalized, now)
		if err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return agent.Session{}, nil, false, err
	}
	return out, evs, created, nil
}

// SetInstructionsSHA records the instructions revision and appends one event
// in one transaction. It holds the same guard as AppendEvent: the session must
// be running under owner.
func (p *Postgres) SetInstructionsSHA(ctx context.Context, id uuid.UUID, owner, sha string, ev agent.EventInput) error {
	evs, err := normalizeEvents([]agent.EventInput{ev})
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if err := guardTx(ctx, tx, id, Guard{States: []agent.SessionState{agent.StateRunning}, LeaseOwner: owner}); err != nil {
			return err
		}
		if err := update(ctx, tx, `UPDATE varroa.agent_sessions SET instructions_sha = $2 WHERE id = $1`, id, sha); err != nil {
			return err
		}
		_, err := appendEvents(ctx, tx, id, evs, p.now())
		return err
	})
}

// PutTenantAgentUnattended sets one TenantAgent's unattended grant on the global
// scope, keyed namespace/name. A nil grant removes the entry. Only the key
// named here changes; the other settings keys stay as they are.
func (p *Postgres) PutTenantAgentUnattended(ctx context.Context, key string, grant *agent.UnattendedGrant, by string) error {
	now := p.now()
	var tag pgconn.CommandTag
	var err error
	if grant == nil {
		tag, err = p.pool.Exec(ctx, `
			UPDATE varroa.agent_settings
			SET settings = settings #- ARRAY['tenant_agent_unattended', $1::text],
			    updated_at = $2, updated_by = $3
			WHERE scope = 'global'`, key, now, by)
	} else {
		entry, merr := json.Marshal(grant)
		if merr != nil {
			return fmt.Errorf("%w: grant: %v", ErrInvalid, merr)
		}
		tag, err = p.pool.Exec(ctx, `
			UPDATE varroa.agent_settings
			SET settings = jsonb_set(COALESCE(settings, '{}'::jsonb), '{tenant_agent_unattended}',
			      COALESCE(settings->'tenant_agent_unattended', '{}'::jsonb) || jsonb_build_object($1::text, $2::jsonb)),
			    updated_at = $3, updated_by = $4
			WHERE scope = 'global'`, key, string(entry), now, by)
	}
	if err != nil {
		return fmt.Errorf("put tenant agent grant: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: global settings row missing", ErrNotFound)
	}
	return nil
}
