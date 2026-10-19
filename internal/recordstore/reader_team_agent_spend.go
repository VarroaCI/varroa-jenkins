package recordstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TeamAgentSpendRow is the rollup of one Team's Team agent sessions on one
// UTC day. The recorder writes it, and the BFF reads it for the Team agent
// budget bar.
type TeamAgentSpendRow struct {
	Tokens   int64
	Sessions int
	Skipped  int
	Failed   int
}

// TeamAgentSpend returns the rollup row of team on the UTC day of day. A day
// with no row answers the zero row.
func (r *pgReader) TeamAgentSpend(ctx context.Context, team string, day time.Time) (TeamAgentSpendRow, error) {
	if err := r.checkSchema(ctx); err != nil {
		return TeamAgentSpendRow{}, err
	}
	var row TeamAgentSpendRow
	err := r.pool.QueryRow(ctx, `
		SELECT tokens, sessions, skipped, failed
		FROM varroa.team_agent_spend
		WHERE team = $1 AND day = $2::date`,
		team, day.UTC().Format("2006-01-02"),
	).Scan(&row.Tokens, &row.Sessions, &row.Skipped, &row.Failed)
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamAgentSpendRow{}, nil
	}
	if err != nil {
		return TeamAgentSpendRow{}, fmt.Errorf("read team agent spend: %w", err)
	}
	return row, nil
}
