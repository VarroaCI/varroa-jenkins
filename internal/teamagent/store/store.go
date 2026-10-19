// Package store holds the record-store queries that the Team agent runner
// makes on top of the agent store: run and placement lookups, the spend
// rollup, and the resume candidates.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store runs the queries on the record-store pool.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// New returns a Store over pool. A nil now uses the wall clock.
func New(pool *pgxpool.Pool, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{pool: pool, now: now}
}

// RunID returns the run that holds one build number of a job. It returns ""
// when no record matches.
func (s *Store) RunID(ctx context.Context, tenant, jobPath string, build int64) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		SELECT run_id::text FROM varroa.runs
		WHERE tenant = $1 AND job_path = $2 AND build_number = $3
		LIMIT 1`, tenant, jobPath, build).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read run id: %w", err)
	}
	return id, nil
}

// RunTenant returns the tenant of one run. It returns "" when no run matches.
func (s *Store) RunTenant(ctx context.Context, runID string) (string, error) {
	var tenant string
	err := s.pool.QueryRow(ctx, `SELECT tenant FROM varroa.runs WHERE run_id::text = $1`, runID).Scan(&tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read run tenant: %w", err)
	}
	return tenant, nil
}

// LivePlacement returns the cluster that holds a tenant's live cell. The
// placement with the highest generation wins. ok is false when the tenant has
// no cell.
func (s *Store) LivePlacement(ctx context.Context, tenant string) (string, bool, error) {
	var cluster string
	err := s.pool.QueryRow(ctx, `
		SELECT cluster FROM varroa.tenant_placements
		WHERE tenant = $1 AND cell <> ''
		ORDER BY generation DESC
		LIMIT 1`, tenant).Scan(&cluster)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read live placement: %w", err)
	}
	return cluster, true, nil
}

// TeamDaySpend returns the tokens that one Team spent on one UTC day, summed
// from the session rows of that day. Budget checks read it, so a session that
// just ended counts at once; the rollup is for the status and the history.
func (s *Store) TeamDaySpend(ctx context.Context, team string, day time.Time) (int64, error) {
	start, end := dayBounds(day)
	var tokens int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(prompt_tokens + completion_tokens), 0) FROM varroa.agent_sessions
		WHERE kind = 'team_agent' AND team = $1 AND started_at >= $2 AND started_at < $3`,
		team, start, end).Scan(&tokens)
	if err != nil {
		return 0, fmt.Errorf("read team spend: %w", err)
	}
	return tokens, nil
}

// UpsertSpend recomputes the rollup row of one Team and UTC day from the
// session rows of that day. The rollup is idempotent: a second call writes the
// same values.
func (s *Store) UpsertSpend(ctx context.Context, team string, day time.Time) error {
	start, end := dayBounds(day)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO varroa.team_agent_spend (team, day, tokens, sessions, skipped, failed, updated_at)
		SELECT $1, $2::date,
		       COALESCE(SUM(prompt_tokens + completion_tokens), 0),
		       COUNT(*)::int,
		       COUNT(*) FILTER (WHERE state = 'skipped')::int,
		       COUNT(*) FILTER (WHERE state = 'failed')::int,
		       $4
		FROM varroa.agent_sessions
		WHERE kind = 'team_agent' AND team = $1
		  AND started_at >= $3 AND started_at < $5
		ON CONFLICT (team, day) DO UPDATE SET
		  tokens = EXCLUDED.tokens, sessions = EXCLUDED.sessions,
		  skipped = EXCLUDED.skipped, failed = EXCLUDED.failed,
		  updated_at = EXCLUDED.updated_at`, team, start.Format("2006-01-02"), start, s.now(), end)
	if err != nil {
		return fmt.Errorf("upsert team spend: %w", err)
	}
	return nil
}

// ResumeCandidates returns the sessions of the given Teams that may need a
// resume: those that wait on an approval, and those whose lease ran out. since
// bounds the scan so an old row is not read again.
func (s *Store) ResumeCandidates(ctx context.Context, teams []string, since time.Time) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM varroa.agent_sessions s
		WHERE s.kind = 'team_agent'
		  AND s.team = ANY($1::text[])
		  AND s.started_at >= $2
		  AND s.state = 'waiting_approval'
		  AND NOT EXISTS (
		    SELECT 1 FROM varroa.agent_approvals a
		    WHERE a.session_id = s.id AND a.state = 'pending')
		ORDER BY s.started_at`, teams, since)
	if err != nil {
		return nil, fmt.Errorf("read resume candidates: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan resume candidate: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// LastSession is the newest session of one principal, for the status ticker.
type LastSession struct {
	ID        uuid.UUID
	Trigger   string
	State     string
	StartedAt time.Time
}

// LastSession returns the newest session of principal. ok is false when the
// principal has none.
func (s *Store) LastSession(ctx context.Context, principal string) (LastSession, bool, error) {
	var out LastSession
	err := s.pool.QueryRow(ctx, `
		SELECT id, trigger, state, started_at FROM varroa.agent_sessions
		WHERE principal = $1
		ORDER BY started_at DESC
		LIMIT 1`, principal).Scan(&out.ID, &out.Trigger, &out.State, &out.StartedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LastSession{}, false, nil
	}
	if err != nil {
		return LastSession{}, false, fmt.Errorf("read last session: %w", err)
	}
	return out, true, nil
}

// dayBounds returns the start of the UTC day of t and the start of the next day.
// The bounds are instants, so the database time zone does not move them.
func dayBounds(t time.Time) (time.Time, time.Time) {
	u := t.UTC()
	start := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 0, 1)
}
