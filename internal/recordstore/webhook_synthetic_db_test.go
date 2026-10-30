package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedSyntheticWebhookPlacement stores a placement row for testTenant with
// a completion webhook and the given incarnation.
func seedSyntheticWebhookPlacement(t *testing.T, db *sql.DB, url, incarnation string) {
	t.Helper()
	seedTenantPlacement(t, db, testTenant, testNamespace, "cell-a", 0, "pod-a", "pod-a", nil)
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET notification_webhook = $3::jsonb, incarnation = $4
		WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant, `{"url":"`+url+`","key":"whsec-test"}`, incarnation); err != nil {
		t.Fatalf("seed synthetic webhook placement: %v", err)
	}
}

// readWebhookBody decodes the stored body of the row of runID that carries
// delivery key key.
func readWebhookBody(t *testing.T, db *sql.DB, runID uuid.UUID, key string) WebhookPayload {
	t.Helper()
	var raw []byte
	if err := db.QueryRow(`SELECT body FROM varroa.webhook_deliveries WHERE run_id = $1 AND delivery_key = $2`,
		runID, key).Scan(&raw); err != nil {
		t.Fatalf("read webhook body %q: %v", key, err)
	}
	var body WebhookPayload
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode webhook body: %v", err)
	}
	return body
}

// readDeliveryID returns the delivery_id of the row of runID with key.
func readDeliveryID(t *testing.T, db *sql.DB, runID uuid.UUID, key string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`SELECT delivery_id::text FROM varroa.webhook_deliveries WHERE run_id = $1 AND delivery_key = $2`,
		runID, key).Scan(&id); err != nil {
		t.Fatalf("read delivery id %q: %v", key, err)
	}
	return id
}

// TestMarkInterrupted_EnqueuesSyntheticDelivery asserts a synthetic end of a
// claimed run enqueues one pending delivery that names its reason.
func TestMarkInterrupted_EnqueuesSyntheticDelivery(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedSyntheticWebhookPlacement(t, db, "https://hooks.example.test/r", "uid-a")
	runID := startRunForFinalize(t, pool, "manual:"+newTestUUIDv7String())
	c := fetchRunningCandidate(t, pool, runID)

	stampRunIntentIncarnation(t, db, runID)
	changed, err := markInterrupted(context.Background(), pool, c, ReasonNoHeartbeat, time.Now(), "")
	if err != nil || !changed {
		t.Fatalf("markInterrupted: changed=%v err=%v", changed, err)
	}

	key := webhookKeyInterruptedPrefix + ReasonNoHeartbeat
	if state, _ := webhookStateFor(t, db, runID, key); state != "pending" {
		t.Fatalf("state = %q, want pending", state)
	}
	body := readWebhookBody(t, db, runID, key)
	if body.Result != "INTERRUPTED" || !body.Synthetic || body.InterruptedReason != ReasonNoHeartbeat {
		t.Fatalf("synthetic body = %+v", body)
	}
	if body.Event != webhookEventRunCompleted {
		t.Fatalf("event = %q, want %q", body.Event, webhookEventRunCompleted)
	}
}

// TestMarkInterrupted_NoWebhookEnqueuesNothing asserts a tenant with no
// webhook gets no synthetic row.
func TestMarkInterrupted_NoWebhookEnqueuesNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedTenantPlacement(t, db, testTenant, testNamespace, "cell-a", 0, "pod-a", "pod-a", nil)
	runID := startRunForFinalize(t, pool, "manual:"+newTestUUIDv7String())
	c := fetchRunningCandidate(t, pool, runID)

	if _, err := markInterrupted(context.Background(), pool, c, ReasonNoHeartbeat, time.Now(), ""); err != nil {
		t.Fatalf("markInterrupted: %v", err)
	}
	if n := webhookRowCount(t, db, runID); n != 0 {
		t.Fatalf("rows = %d, want 0 for a tenant without a webhook", n)
	}
}

// TestMarkInterrupted_SkippedMarkEnqueuesNothing asserts a second mark of an
// already ended run writes no second row.
func TestMarkInterrupted_SkippedMarkEnqueuesNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedSyntheticWebhookPlacement(t, db, "https://hooks.example.test/r", "uid-a")
	runID := startRunForFinalize(t, pool, "manual:"+newTestUUIDv7String())
	c := fetchRunningCandidate(t, pool, runID)

	if changed, err := markInterrupted(context.Background(), pool, c, ReasonNoHeartbeat, time.Now(), ""); err != nil || !changed {
		t.Fatalf("first mark: changed=%v err=%v", changed, err)
	}
	if changed, err := markInterrupted(context.Background(), pool, c, ReasonNoHeartbeat, time.Now(), ""); err != nil || changed {
		t.Fatalf("second mark: changed=%v err=%v, want no change", changed, err)
	}
	if n := webhookRowCount(t, db, runID); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

// TestMarkInterrupted_UnclaimedQueuedRunEnqueuesNothing asserts a run with no
// run_intents row, which no cell claimed, sends no delivery.
func TestMarkInterrupted_UnclaimedQueuedRunEnqueuesNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedSyntheticWebhookPlacement(t, db, "https://hooks.example.test/r", "uid-a")
	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	runID := seedOlderRun(t, db, req, time.Minute)
	c := fetchRunningCandidate(t, pool, runID)

	if changed, err := markInterrupted(context.Background(), pool, c, ReasonNoHeartbeat, time.Now(), ""); err != nil || !changed {
		t.Fatalf("markInterrupted: changed=%v err=%v", changed, err)
	}
	if n := webhookRowCount(t, db, runID); n != 0 {
		t.Fatalf("rows = %d, want 0 for an unclaimed run", n)
	}
}

// TestRunCompleted_SupersedesSyntheticDelivery asserts a real completion after
// a synthetic end names that delivery, drops it while pending, and sends a
// delivery of its own without the synthetic flag.
func TestRunCompleted_SupersedesSyntheticDelivery(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedSyntheticWebhookPlacement(t, db, "https://hooks.example.test/r", "uid-a")
	intentKey := "manual:" + newTestUUIDv7String()
	runID := startRunForFinalize(t, pool, intentKey)
	c := fetchRunningCandidate(t, pool, runID)
	stampRunIntentIncarnation(t, db, runID)
	if changed, err := markInterrupted(context.Background(), pool, c, ReasonNoHeartbeat, time.Now(), ""); err != nil || !changed {
		t.Fatalf("markInterrupted: changed=%v err=%v", changed, err)
	}
	synthKey := webhookKeyInterruptedPrefix + ReasonNoHeartbeat
	synthID := readDeliveryID(t, db, runID, synthKey)

	g := NewIngester(pool, nil, nil)
	g.SetWebhooks("https://varroa.example.test", nil)
	env := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: strPtr("SUCCESS")})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	if state, _ := webhookStateFor(t, db, runID, synthKey); state != "dropped" {
		t.Fatalf("synthetic state = %q, want dropped", state)
	}
	if state, _ := webhookStateFor(t, db, runID, webhookKeyCompleted); state != "pending" {
		t.Fatalf("completed state = %q, want pending", state)
	}
	body := readWebhookBody(t, db, runID, webhookKeyCompleted)
	if body.Supersedes != synthID {
		t.Fatalf("supersedes = %q, want %q", body.Supersedes, synthID)
	}
	if body.Synthetic {
		t.Fatalf("real completion body carries synthetic=true: %+v", body)
	}
}

// TestMarkInterrupted_EmptyClaimIncarnationWritesDroppedRow asserts that a
// synthetic end of a run whose claim stored no incarnation writes its row
// dropped, with the enqueue reason.
func TestMarkInterrupted_EmptyClaimIncarnationWritesDroppedRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedSyntheticWebhookPlacement(t, db, "https://hooks.example.test/r", "")
	runID := startRunForFinalize(t, pool, "manual:"+newTestUUIDv7String())
	c := fetchRunningCandidate(t, pool, runID)

	changed, err := markInterrupted(context.Background(), pool, c, ReasonNoHeartbeat, time.Now(), "")
	if err != nil || !changed {
		t.Fatalf("markInterrupted: changed=%v err=%v", changed, err)
	}

	key := webhookKeyInterruptedPrefix + ReasonNoHeartbeat
	if state, _ := webhookStateFor(t, db, runID, key); state != "dropped" {
		t.Fatalf("state = %q, want dropped", state)
	}
	var lastErr string
	if err := db.QueryRow(`SELECT COALESCE(last_error, '') FROM varroa.webhook_deliveries
		WHERE run_id = $1 AND delivery_key = $2`, runID, key).Scan(&lastErr); err != nil {
		t.Fatalf("read dropped row: %v", err)
	}
	if lastErr != webhookErrNoIncarnation {
		t.Fatalf("last_error = %q, want %q", lastErr, webhookErrNoIncarnation)
	}
	if n := webhookRowCount(t, db, runID); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

// TestRunCompleted_EmptyIncarnationStillSupersedesSynthetic asserts that a
// completion of a run whose claim stored no incarnation leaves the synthetic
// row and its own row both dropped.
func TestRunCompleted_EmptyIncarnationStillSupersedesSynthetic(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedSyntheticWebhookPlacement(t, db, "https://hooks.example.test/r", "")
	intentKey := "manual:" + newTestUUIDv7String()
	runID := startRunForFinalize(t, pool, intentKey)
	c := fetchRunningCandidate(t, pool, runID)
	if changed, err := markInterrupted(context.Background(), pool, c, ReasonNoHeartbeat, time.Now(), ""); err != nil || !changed {
		t.Fatalf("markInterrupted: changed=%v err=%v", changed, err)
	}

	g := NewIngester(pool, nil, nil)
	g.SetWebhooks("https://varroa.example.test", nil)
	env := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: strPtr("SUCCESS")})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	if state, _ := webhookStateFor(t, db, runID, webhookKeyInterruptedPrefix+ReasonNoHeartbeat); state != "dropped" {
		t.Fatalf("synthetic state = %q, want dropped", state)
	}
	if state, _ := webhookState(t, db, runID); state != "dropped" {
		t.Fatalf("completed state = %q, want dropped", state)
	}
	if n := webhookRowCount(t, db, runID); n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
}

// TestRunCompleted_RedeliveryAfterSyntheticAddsNothing asserts a redelivered
// completion adds no row after a synthetic end.
func TestRunCompleted_RedeliveryAfterSyntheticAddsNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedSyntheticWebhookPlacement(t, db, "https://hooks.example.test/r", "uid-a")
	intentKey := "manual:" + newTestUUIDv7String()
	runID := startRunForFinalize(t, pool, intentKey)
	c := fetchRunningCandidate(t, pool, runID)
	if changed, err := markInterrupted(context.Background(), pool, c, ReasonNoHeartbeat, time.Now(), ""); err != nil || !changed {
		t.Fatalf("markInterrupted: changed=%v err=%v", changed, err)
	}

	g := NewIngester(pool, nil, nil)
	g.SetWebhooks("https://varroa.example.test", nil)
	env := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: strPtr("SUCCESS")})
	for i := 0; i < 2; i++ {
		if _, err := g.Apply(context.Background(), env); err != nil {
			t.Fatalf("apply run.completed %d: %v", i, err)
		}
	}
	if n := webhookRowCount(t, db, runID); n != 2 {
		t.Fatalf("rows = %d, want 2 (one synthetic, one completion)", n)
	}
}

// TestSyntheticDelivery_ReplacedTenantIsDropped asserts a synthetic delivery
// of a run claimed under a predecessor incarnation is dropped, never sent, once
// the tenant name is taken by a new incarnation.
func TestSyntheticDelivery_ReplacedTenantIsDropped(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	rec, srv := newWebhookReceiver(t, http.StatusNoContent)
	seedSyntheticWebhookPlacement(t, db, srv.URL, "uid-a")

	ledger := NewLedger(pool, NewPlacementFence(pool, nil))
	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionGranted {
		t.Fatalf("want granted claim, got %+v", resp)
	}
	runID := uuid.MustParse(resp.RunID)

	g := NewIngester(pool, nil, nil)
	started := newTestEnvelope(t, runID, req.IntentKey, KindRunStarted, RunStartedPayload{})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	c := fetchRunningCandidate(t, pool, runID)
	if changed, err := markInterrupted(context.Background(), pool, c, ReasonNoHeartbeat, time.Now(), ""); err != nil || !changed {
		t.Fatalf("markInterrupted: changed=%v err=%v", changed, err)
	}

	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET incarnation = 'uid-b'
		WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant); err != nil {
		t.Fatalf("replace tenant incarnation: %v", err)
	}

	newTestDispatcher(pool, srv, time.Now()).round(context.Background())
	if rec.count() != 0 {
		t.Fatalf("receiver got %d requests, want 0 for a predecessor's synthetic delivery", rec.count())
	}
	if state, _ := webhookStateFor(t, db, runID, webhookKeyInterruptedPrefix+ReasonNoHeartbeat); state != "dropped" {
		t.Fatalf("state = %q, want dropped", state)
	}
}

// TestEndStaleMarkedIntent_EnqueuesStartLostDelivery asserts a start-lost end
// of a marked intent enqueues one synthetic delivery that carries the intent's
// claim-time incarnation.
func TestEndStaleMarkedIntent_EnqueuesStartLostDelivery(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedStaleMarkedRun(t, db, ClaimExpiryAge+time.Minute, `{}`)
	stampRunIntentIncarnation(t, db, runID)
	setSyntheticWebhook(t, db, "https://hooks.example.test/r", "uid-a")
	if _, err := db.Exec(`UPDATE varroa.run_intents SET incarnation = 'uid-a' WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("stamp intent incarnation: %v", err)
	}

	now := time.Now().UTC()
	candidates, err := selectStaleMarkedIntents(context.Background(), pool, now.Add(-ClaimExpiryAge), markedLostBatch)
	if err != nil {
		t.Fatalf("selectStaleMarkedIntents: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
	if !endStaleMarkedIntent(context.Background(), pool, nil, nil, nil, candidates[0], now, "", slog.Default()) {
		t.Fatal("endStaleMarkedIntent changed = false, want true")
	}

	key := webhookKeyInterruptedPrefix + ReasonStartLost
	if state, _ := webhookStateFor(t, db, runID, key); state != "pending" {
		t.Fatalf("state = %q, want pending", state)
	}
	var incarnation string
	if err := db.QueryRow(`SELECT incarnation FROM varroa.webhook_deliveries WHERE run_id = $1 AND delivery_key = $2`,
		runID, key).Scan(&incarnation); err != nil {
		t.Fatalf("read delivery incarnation: %v", err)
	}
	if incarnation != "uid-a" {
		t.Fatalf("delivery incarnation = %q, want uid-a", incarnation)
	}
	body := readWebhookBody(t, db, runID, key)
	if body.Result != "INTERRUPTED" || !body.Synthetic || body.InterruptedReason != ReasonStartLost {
		t.Fatalf("synthetic body = %+v", body)
	}
}

// setSyntheticWebhook stores a completion webhook and an incarnation on a
// placement row that a seed helper already wrote.
func setSyntheticWebhook(t *testing.T, db *sql.DB, url, incarnation string) {
	t.Helper()
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET notification_webhook = $3::jsonb, incarnation = $4
		WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant, `{"url":"`+url+`","key":"whsec-test"}`, incarnation); err != nil {
		t.Fatalf("set synthetic webhook: %v", err)
	}
}

// TestStartLostThenSweepMark_EnqueuesTwoSyntheticRows asserts that a start-lost
// mark, superseded by the owner's run.started, and a later heartbeat-lost mark
// of the same run enqueue one synthetic delivery each.
func TestStartLostThenSweepMark_EnqueuesTwoSyntheticRows(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedStaleMarkedRun(t, db, ClaimExpiryAge+time.Minute, `{}`)
	stampRunIntentIncarnation(t, db, runID)
	setSyntheticWebhook(t, db, "https://hooks.example.test/r", "uid-a")

	now := time.Now().UTC()
	candidates, err := selectStaleMarkedIntents(context.Background(), pool, now.Add(-ClaimExpiryAge), markedLostBatch)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("selectStaleMarkedIntents = %d candidates, err %v", len(candidates), err)
	}
	if !endStaleMarkedIntent(context.Background(), pool, nil, nil, nil, candidates[0], now, "", slog.Default()) {
		t.Fatal("endStaleMarkedIntent changed = false, want true")
	}

	g := NewIngester(pool, nil, nil)
	started := newTestEnvelope(t, runID, "manual:"+runID.String(), KindRunStarted, RunStartedPayload{})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if status != "RUNNING" {
		t.Fatalf("status after run.started = %q, want RUNNING", status)
	}

	c := fetchRunningCandidate(t, pool, runID)
	if changed, err := markInterrupted(context.Background(), pool, c, ReasonHeartbeatLost, time.Now(), ""); err != nil || !changed {
		t.Fatalf("heartbeat-lost mark: changed=%v err=%v", changed, err)
	}

	if n := webhookRowCount(t, db, runID); n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
	for _, reason := range []string{ReasonStartLost, ReasonHeartbeatLost} {
		if state, _ := webhookStateFor(t, db, runID, webhookKeyInterruptedPrefix+reason); state != "pending" {
			t.Fatalf("%s state = %q, want pending", reason, state)
		}
	}
}
