package recordstore

import (
	"context"
	"testing"
)

// TestRunCompleted_ForeignCompletionEnqueuesNothing asserts a completion that
// comes from an instance other than the run's owner enqueues no delivery, even
// when the tenant declares a webhook.
func TestRunCompleted_ForeignCompletionEnqueuesNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:webhook-foreign")
	seedTenantPlacement(t, db, testTenant, testNamespace, "cell-a", 0, "pod-a", "", nil)
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET notification_webhook = $1::jsonb
		WHERE cluster = $2 AND tenant = $3
	`, `{"url":"https://receiver.example.test/hook","key":"whsec-test"}`, testCluster, testTenant); err != nil {
		t.Fatalf("store tenant webhook: %v", err)
	}

	env := newTestEnvelope(t, runID, "manual:webhook-foreign", KindRunCompleted,
		RunCompletedPayload{Result: strPtr("SUCCESS")})
	env.CellInstance = "pod-other"
	if _, err := NewIngester(pool, nil, nil).Apply(context.Background(), env); err != nil {
		t.Fatalf("apply foreign run.completed: %v", err)
	}
	if n := webhookRowCount(t, db, runID); n != 0 {
		t.Fatalf("delivery rows = %d, want 0 for a non-owner completion", n)
	}
}
