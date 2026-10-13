package recordstore

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestRunCompleted_StampsTheClaimIncarnation asserts an owner completion
// stamps the incarnation that owned the run at claim time. A replacement
// Team takes the name afterwards, so the delivery is dropped, not sent.
func TestRunCompleted_StampsTheClaimIncarnation(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	rec, srv := newWebhookReceiver(t, http.StatusNoContent)

	seedTenantPlacement(t, db, testTenant, testNamespace, "cell-a", 0, "pod-a", "pod-a", nil)
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements
		SET incarnation = 'uid-a', notification_webhook = $3::jsonb
		WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant, `{"url":"`+srv.URL+`","key":"whsec-test"}`); err != nil {
		t.Fatalf("seed claim-time placement: %v", err)
	}
	ledger := NewLedger(pool, NewPlacementFence(pool, nil))
	claim := newTestClaimRequest("manual:webhook-claim-incarnation", 0)
	resp := ledger.Claim(context.Background(), claim)
	if resp.Decision != DecisionGranted {
		t.Fatalf("want granted claim, got %+v", resp)
	}
	runID := uuid.MustParse(resp.RunID)

	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET incarnation = 'uid-b'
		WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant); err != nil {
		t.Fatalf("replace tenant incarnation: %v", err)
	}

	g := NewIngester(pool, nil, nil)
	g.SetWebhooks("https://varroa.example.test", nil)
	env := newTestEnvelope(t, runID, "manual:webhook-claim-incarnation", KindRunCompleted,
		RunCompletedPayload{Result: strPtr("SUCCESS")})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	var incarnation string
	if err := db.QueryRow(`SELECT incarnation FROM varroa.webhook_deliveries WHERE run_id = $1`,
		runID).Scan(&incarnation); err != nil {
		t.Fatalf("read delivery incarnation: %v", err)
	}
	if incarnation != "uid-a" {
		t.Fatalf("delivery incarnation = %q, want the claim-time uid-a", incarnation)
	}

	newTestDispatcher(pool, srv, time.Now()).round(context.Background())
	if rec.count() != 0 {
		t.Fatalf("receiver got %d requests, want 0 for a predecessor's run", rec.count())
	}
	if state, _ := webhookState(t, db, runID); state != "dropped" {
		t.Fatalf("state = %q, want dropped", state)
	}
}
