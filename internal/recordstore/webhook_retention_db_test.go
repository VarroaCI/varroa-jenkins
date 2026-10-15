package recordstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestMaintenance_DeletesFinishedWebhookDeliveries asserts the retention step
// deletes a finished row older than seven days, and keeps a pending row of the
// same age and a recent finished row.
func TestMaintenance_DeletesFinishedWebhookDeliveries(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	ctx := context.Background()
	old := time.Now().Add(-8 * 24 * time.Hour)

	insert := func(state string, updated time.Time) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO varroa.webhook_deliveries
				(delivery_id, run_id, cluster, tenant, body, state, updated_at)
			VALUES ($1, $2, 'c', 't', '{}'::bytea, $3, $4)
		`, id, uuid.New(), state, updated); err != nil {
			t.Fatalf("insert %s delivery: %v", state, err)
		}
		return id
	}
	oldDelivered := insert("delivered", old)
	oldPending := insert("pending", old)
	recentFailed := insert("failed", time.Now())

	deleted, err := deleteFinishedWebhookDeliveries(ctx, pool, time.Now().Add(-webhookDeliveryRetention))
	if err != nil {
		t.Fatalf("delete finished deliveries: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	for id, want := range map[uuid.UUID]int{oldDelivered: 0, oldPending: 1, recentFailed: 1} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM varroa.webhook_deliveries WHERE delivery_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count delivery %s: %v", id, err)
		}
		if n != want {
			t.Fatalf("delivery %s rows = %d, want %d", id, n, want)
		}
	}
}
