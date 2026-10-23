package recordstore

import (
	"context"
	"time"
)

// deleteExpiredTenantAgentSpend deletes the spend rows of UTC days that fall
// outside the runs window. A window of zero or less keeps every row. It
// returns the number of rows removed.
func deleteExpiredTenantAgentSpend(ctx context.Context, db maintDB, now time.Time, runs time.Duration) (int64, error) {
	if runs <= 0 {
		return 0, nil
	}
	tag, err := db.Exec(ctx,
		`DELETE FROM varroa.tenant_agent_spend WHERE day < ($1::timestamptz AT TIME ZONE 'UTC')::date`,
		now.Add(-runs))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
