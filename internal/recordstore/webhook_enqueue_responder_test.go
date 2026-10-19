package recordstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestWebhookEnqueue_OneRowPerCall checks that a repeat of one session and
// call answers the same delivery and writes no second row.
func TestWebhookEnqueue_OneRowPerCall(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedTenantPlacement(t, db, testTenant, testNamespace, "cell-a", 0, "pod-a", "", nil)
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET notification_webhook = $1::jsonb
		WHERE cluster = $2 AND tenant = $3
	`, `{"url":"https://hooks.example.org/x","key":"whsec-test"}`, testCluster, testTenant); err != nil {
		t.Fatalf("store tenant webhook: %v", err)
	}
	session := uuid.NewString()
	req := TeamAgentWebhookRequest{
		Cluster: testCluster, Tenant: testTenant, SessionID: session, CallID: "call-1",
		Principal: "agent:ns/team/a", Body: json.RawMessage(`{"text":"hello"}`),
	}
	first, err := EnqueueTeamAgentWebhook(context.Background(), pool, req, time.Now().UTC())
	if err != nil || !first.OK {
		t.Fatalf("first enqueue: %+v, %v", first, err)
	}
	second, err := EnqueueTeamAgentWebhook(context.Background(), pool, req, time.Now().UTC())
	if err != nil || !second.OK {
		t.Fatalf("repeat enqueue: %+v, %v", second, err)
	}
	if first.DeliveryID != second.DeliveryID {
		t.Fatalf("repeat delivery = %s, want %s", second.DeliveryID, first.DeliveryID)
	}
	var n int
	if err := db.QueryRow(`
		SELECT count(*) FROM varroa.webhook_deliveries WHERE delivery_key = $1
	`, "team_agent.message:"+session+":call-1").Scan(&n); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if n != 1 {
		t.Fatalf("delivery rows = %d, want 1", n)
	}
}

// TestWebhookEnqueue_NoWebhook answers the no-webhook error for a tenant that
// declares no webhook, and writes nothing.
func TestWebhookEnqueue_NoWebhook(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedTenantPlacement(t, db, testTenant, testNamespace, "cell-a", 0, "pod-a", "", nil)
	req := TeamAgentWebhookRequest{
		Cluster: testCluster, Tenant: testTenant, SessionID: uuid.NewString(), CallID: "call-x",
		Principal: "agent:ns/team/a", Body: json.RawMessage(`{"text":"hello"}`),
	}
	resp, err := EnqueueTeamAgentWebhook(context.Background(), pool, req, time.Now().UTC())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if resp.OK || resp.Error != "no webhook" {
		t.Fatalf("response = %+v, want no webhook", resp)
	}
}

// TestWebhookEnqueue_SessionIDAsRunID keys a call without a run by its session
// id, which the delivery row stores as run_id.
func TestWebhookEnqueue_SessionIDAsRunID(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedTenantPlacement(t, db, testTenant, testNamespace, "cell-a", 0, "pod-a", "", nil)
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET notification_webhook = $1::jsonb
		WHERE cluster = $2 AND tenant = $3
	`, `{"url":"https://hooks.example.org/x","key":"whsec-test"}`, testCluster, testTenant); err != nil {
		t.Fatalf("store tenant webhook: %v", err)
	}
	session := uuid.NewString()
	req := TeamAgentWebhookRequest{
		Cluster: testCluster, Tenant: testTenant, SessionID: session, CallID: "call-2",
		Principal: "agent:ns/team/a", Body: json.RawMessage(`{"text":"hello"}`),
	}
	resp, err := EnqueueTeamAgentWebhook(context.Background(), pool, req, time.Now().UTC())
	if err != nil || !resp.OK {
		t.Fatalf("enqueue: %+v, %v", resp, err)
	}
	var runID string
	if err := db.QueryRow(`
		SELECT run_id::text FROM varroa.webhook_deliveries WHERE delivery_id = $1
	`, resp.DeliveryID).Scan(&runID); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if runID != session {
		t.Fatalf("run_id = %s, want the session id %s", runID, session)
	}
}
