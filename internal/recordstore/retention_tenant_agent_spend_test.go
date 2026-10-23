package recordstore

import (
	"context"
	"testing"
	"time"
)

// TestDeleteExpiredTenantAgentSpend keeps a day inside the runs window and
// deletes a day outside it.
func TestDeleteExpiredTenantAgentSpend(t *testing.T) {
	t.Parallel()
	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	oldDay := now.AddDate(0, 0, -40).Format("2006-01-02")
	newDay := now.AddDate(0, 0, -1).Format("2006-01-02")

	for _, day := range []string{oldDay, newDay} {
		if _, err := db.Exec(`INSERT INTO varroa.tenant_agent_spend (tenant, day, tokens) VALUES ('spend-ret', $1::date, 1)`, day); err != nil {
			t.Fatalf("insert %s: %v", day, err)
		}
	}

	if _, err := deleteExpiredTenantAgentSpend(ctx, pool, now, 0); err != nil {
		t.Fatalf("zero window: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.tenant_agent_spend WHERE tenant = 'spend-ret'`).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 2 {
		t.Fatalf("a zero window must keep every row, got %d", n)
	}

	removed, err := deleteExpiredTenantAgentSpend(ctx, pool, now, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("deleteExpiredTenantAgentSpend: %v", err)
	}
	if removed < 1 {
		t.Fatalf("removed = %d, want the 40-day row gone", removed)
	}
	var kept string
	if err := db.QueryRow(`SELECT day::text FROM varroa.tenant_agent_spend WHERE tenant = 'spend-ret'`).Scan(&kept); err != nil {
		t.Fatalf("read kept row: %v", err)
	}
	if kept != newDay {
		t.Fatalf("kept day = %s, want %s", kept, newDay)
	}
}
