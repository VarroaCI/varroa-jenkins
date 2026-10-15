package recordstore

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestWebhookDispatch_DropsAReplacedTenantsDelivery asserts a delivery queued
// under one tenant incarnation is dropped, and never sent, once the tenant
// name is taken by a new incarnation with a webhook of its own.
func TestWebhookDispatch_DropsAReplacedTenantsDelivery(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	rec, srv := newWebhookReceiver(t, http.StatusNoContent)
	runID := seedWebhookRun(t, pool, db, "manual:webhook-replaced", srv.URL)

	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET incarnation = 'replacement-uid'
		WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant); err != nil {
		t.Fatalf("replace tenant incarnation: %v", err)
	}

	newTestDispatcher(pool, srv, time.Now()).round(context.Background())
	if rec.count() != 0 {
		t.Fatalf("receiver got %d requests, want 0 for a predecessor's delivery", rec.count())
	}
	if state, _ := webhookState(t, db, runID); state != "dropped" {
		t.Fatalf("state = %q, want dropped", state)
	}
}
