package recordstore

import (
	"context"
	"testing"
	"time"
)

// TestTeamAgentSpendReadsTheRollupRow asserts TeamAgentSpend reads the row of
// one Team and UTC day, and answers the zero row for a day with no row.
func TestTeamAgentSpendReadsTheRollupRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	day := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO varroa.team_agent_spend (team, day, tokens, sessions, skipped, failed)
		VALUES ('payments', $1::date, 1500, 3, 1, 1)`, day.Format("2006-01-02")); err != nil {
		t.Fatalf("insert spend row: %v", err)
	}

	row, err := reader.TeamAgentSpend(ctx, "payments", day.Add(13*time.Hour))
	if err != nil {
		t.Fatalf("TeamAgentSpend: %v", err)
	}
	if row.Tokens != 1500 || row.Sessions != 3 || row.Skipped != 1 || row.Failed != 1 {
		t.Errorf("row = %+v, want the inserted rollup", row)
	}

	empty, err := reader.TeamAgentSpend(ctx, "payments", day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("TeamAgentSpend on an empty day: %v", err)
	}
	if empty != (TeamAgentSpendRow{}) {
		t.Errorf("empty day = %+v, want the zero row", empty)
	}
}
