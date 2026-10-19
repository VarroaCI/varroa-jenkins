package recordstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestWebhookEventFor checks that a Team agent message keeps its own event
// type, and that a run row still reports a run completion.
func TestWebhookEventFor(t *testing.T) {
	if got := webhookEventFor("team_agent.message:s:c"); got != "team_agent.message" {
		t.Errorf("agent key event = %q", got)
	}
	if got := webhookEventFor("completed"); got != webhookEventRunCompleted {
		t.Errorf("completion key event = %q", got)
	}
}

// TestRunCompletionKeepsAgentMessage checks that a run completion does not
// drop a pending Team agent message on the same run.
func TestRunCompletionKeepsAgentMessage(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedTenantPlacement(t, db, testTenant, testNamespace, "cell-a", 0, "pod-a", "", nil)
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET notification_webhook = $1::jsonb
		WHERE cluster = $2 AND tenant = $3
	`, `{"url":"https://hooks.example.org/x","key":"whsec-test"}`, testCluster, testTenant); err != nil {
		t.Fatalf("store tenant webhook: %v", err)
	}
	key := "manual:agent-keep-" + uuid.NewString()
	runID := seedGrantedIntent(t, pool, key)
	if _, err := EnqueueTeamAgentWebhook(context.Background(), pool, TeamAgentWebhookRequest{
		Cluster: testCluster, Tenant: testTenant, SessionID: uuid.NewString(), CallID: "c1",
		RunID: runID.String(), Principal: "agent:ns/team/a", Body: json.RawMessage(`{"text":"hi"}`),
	}, time.Now().UTC()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	env := newTestEnvelope(t, runID, key, KindRunCompleted, RunCompletedPayload{Result: strPtr("SUCCESS")})
	if _, err := NewIngester(pool, nil, nil).Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	var pending int
	if err := db.QueryRow(`
		SELECT count(*) FROM varroa.webhook_deliveries
		WHERE run_id = $1 AND delivery_key LIKE 'team_agent.message:%' AND state = 'pending'
	`, runID).Scan(&pending); err != nil {
		t.Fatalf("count agent deliveries: %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending agent messages = %d, want 1", pending)
	}
}

// TestBranchScanRunningThenFailedPublishes checks that a failure which follows
// a stored running snapshot of the same scan still publishes its event.
func TestBranchScanRunningThenFailedPublishes(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()
	base := time.Now().UTC()
	apply := func(at time.Duration, scan *BranchScanInput) {
		t.Helper()
		p := oneBranchProject("acme/running")
		p.LastScan = scan
		if _, err := g.Apply(ctx, newJobBranchesEnvelope(t, jsTestCell, base.Add(at), JobBranchesPayload{
			Hash: "h", Total: 1, Projects: []JobBranchProjectInput{p},
		})); err != nil {
			t.Fatalf("apply job.branches: %v", err)
		}
	}
	apply(time.Second, &BranchScanInput{ScanID: "scan-r", State: "running"})
	apply(2*time.Second, &BranchScanInput{ScanID: "scan-r", State: "failed", Result: "FAILURE", EndedAt: base.Format(time.RFC3339)})
	var n int
	if err := db.QueryRow(`
		SELECT count(*) FROM varroa.activity
		WHERE type = 'pipeline.branch_scan_failed' AND item_path = 'acme/running'
	`).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("branch scan failure rows = %d, want 1", n)
	}
}
