package recordstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TenantAgentSpendRow is the rollup of one Tenant's Tenant agent sessions on one
// UTC day. The recorder writes it, and the BFF reads it for the Tenant agent
// budget bar.
type TenantAgentSpendRow struct {
	Tokens   int64
	Sessions int
	Skipped  int
	Failed   int
}

// TenantAgentSpend returns the rollup row of tenant on the UTC day of day. A day
// with no row answers the zero row.
func (r *pgReader) TenantAgentSpend(ctx context.Context, tenant string, day time.Time) (TenantAgentSpendRow, error) {
	if err := r.checkSchema(ctx); err != nil {
		return TenantAgentSpendRow{}, err
	}
	var row TenantAgentSpendRow
	err := r.pool.QueryRow(ctx, `
		SELECT tokens, sessions, skipped, failed
		FROM varroa.tenant_agent_spend
		WHERE tenant = $1 AND day = $2::date`,
		tenant, day.UTC().Format("2006-01-02"),
	).Scan(&row.Tokens, &row.Sessions, &row.Skipped, &row.Failed)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantAgentSpendRow{}, nil
	}
	if err != nil {
		return TenantAgentSpendRow{}, fmt.Errorf("read tenant agent spend: %w", err)
	}
	return row, nil
}
