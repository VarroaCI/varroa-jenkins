package recordstore

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestWebhookDispatch_TwoDispatchersNeverDoubleSend runs two dispatchers over
// the same five due deliveries at once. Each delivery must reach the receiver
// exactly once, because the lease keeps a row out of the second dispatcher's
// batch.
func TestWebhookDispatch_TwoDispatchersNeverDoubleSend(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	rec, srv := newWebhookReceiver(t, http.StatusNoContent)

	seedTenantPlacement(t, db, testTenant, testNamespace, "cell-a", 0, "pod-a", "", nil)
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET notification_webhook = $1::jsonb
		WHERE cluster = $2 AND tenant = $3
	`, `{"url":"`+srv.URL+`","key":"whsec-test"}`, testCluster, testTenant); err != nil {
		t.Fatalf("store tenant webhook: %v", err)
	}
	stampWebhookIncarnation(t, db)
	const rows = 5
	for i := 0; i < rows; i++ {
		key := fmt.Sprintf("manual:webhook-two-%d", i)
		runID := seedGrantedIntent(t, pool, key)
		stampRunIntentIncarnation(t, db, runID)
		env := newTestEnvelope(t, runID, key, KindRunCompleted,
			RunCompletedPayload{Result: strPtr("SUCCESS")})
		if _, err := NewIngester(pool, nil, nil).Apply(context.Background(), env); err != nil {
			t.Fatalf("apply run.completed %d: %v", i, err)
		}
	}

	now := time.Now()
	first := newTestDispatcher(pool, srv, now)
	second := newTestDispatcher(pool, srv, now)
	var wg sync.WaitGroup
	for _, d := range []*WebhookDispatcher{first, second} {
		wg.Add(1)
		go func(d *WebhookDispatcher) {
			defer wg.Done()
			d.round(context.Background())
		}(d)
	}
	wg.Wait()

	if got := rec.count(); got != rows {
		t.Fatalf("receiver got %d requests, want %d (one per delivery)", got, rows)
	}
	seen := map[string]bool{}
	for _, req := range rec.requests {
		id := req.header.Get("X-Varroa-Delivery")
		if seen[id] {
			t.Fatalf("delivery %s was sent twice", id)
		}
		seen[id] = true
	}
}

// TestWebhookDispatch_EmptyIncarnationNeverSent asserts that a run whose claim
// stored an empty incarnation sends nothing, even after the placement gains a
// real incarnation. The enqueue writes its row dropped, so the dispatcher
// never leases it and keeps the enqueue reason.
func TestWebhookDispatch_EmptyIncarnationNeverSent(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	rec, srv := newWebhookReceiver(t, http.StatusNoContent)
	seedSyntheticWebhookPlacement(t, db, srv.URL, "")
	intentKey := "manual:webhook-empty-never-sent"
	runID := seedGrantedIntent(t, pool, intentKey)

	g := NewIngester(pool, nil, nil)
	g.SetWebhooks("https://varroa.example.test", nil)
	env := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: strPtr("SUCCESS")})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	if _, err := db.Exec(`UPDATE varroa.tenant_placements SET incarnation = 'uid-a' WHERE cluster = $1 AND tenant = $2`,
		testCluster, testTenant); err != nil {
		t.Fatalf("stamp placement incarnation: %v", err)
	}

	newTestDispatcher(pool, srv, time.Now()).round(context.Background())
	if rec.count() != 0 {
		t.Fatalf("receiver got %d requests, want 0", rec.count())
	}
	if state, _ := webhookState(t, db, runID); state != "dropped" {
		t.Fatalf("state = %q, want dropped", state)
	}
	var lastErr string
	if err := db.QueryRow(`SELECT COALESCE(last_error, '') FROM varroa.webhook_deliveries
		WHERE run_id = $1 AND delivery_key = $2`, runID, webhookKeyCompleted).Scan(&lastErr); err != nil {
		t.Fatalf("read last_error: %v", err)
	}
	if lastErr != webhookErrNoIncarnation {
		t.Fatalf("last_error = %q, want the enqueue reason %q", lastErr, webhookErrNoIncarnation)
	}
}

// stampWebhookIncarnation sets the incarnation "uid-a" on the test placement
// and on its delivery row, so a test starts from one agreed tenant incarnation.
func stampWebhookIncarnation(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`UPDATE varroa.tenant_placements SET incarnation = 'uid-a' WHERE cluster = $1 AND tenant = $2`,
		testCluster, testTenant); err != nil {
		t.Fatalf("stamp placement incarnation: %v", err)
	}
	if _, err := db.Exec(`UPDATE varroa.webhook_deliveries SET incarnation = 'uid-a' WHERE cluster = $1 AND tenant = $2`,
		testCluster, testTenant); err != nil {
		t.Fatalf("stamp delivery incarnation: %v", err)
	}
}

// leaseOneWebhookRow leases the single pending delivery of the test database.
func leaseOneWebhookRow(t *testing.T, d *WebhookDispatcher) webhookRow {
	t.Helper()
	rows, err := d.lease(context.Background())
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("leased %d rows, want 1", len(rows))
	}
	return rows[0]
}

// webhookLastError reads the last_error column of the completion row of runID.
func webhookLastError(t *testing.T, db *sql.DB, runID uuid.UUID) string {
	t.Helper()
	var lastErr sql.NullString
	if err := db.QueryRow(`SELECT last_error FROM varroa.webhook_deliveries WHERE run_id = $1 AND delivery_key = $2`,
		runID, webhookKeyCompleted).Scan(&lastErr); err != nil {
		t.Fatalf("read last_error: %v", err)
	}
	return lastErr.String
}

// TestWebhookDispatch_FenceRestampAfterLeaseStillDelivers asserts a fence
// restamp that moves the placement and the pending row to a new incarnation,
// after the lease, does not drop the delivery.
func TestWebhookDispatch_FenceRestampAfterLeaseStillDelivers(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	rec, srv := newWebhookReceiver(t, http.StatusNoContent)
	runID := seedWebhookRun(t, pool, db, "manual:webhook-restamp", srv.URL)
	stampWebhookIncarnation(t, db)

	d := newTestDispatcher(pool, srv, time.Now())
	row := leaseOneWebhookRow(t, d)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin restamp: %v", err)
	}
	if _, err := tx.Exec(`UPDATE varroa.tenant_placements SET incarnation = 'uid-b' WHERE cluster = $1 AND tenant = $2`,
		testCluster, testTenant); err != nil {
		t.Fatalf("restamp placement: %v", err)
	}
	if _, err := tx.Exec(`UPDATE varroa.webhook_deliveries SET incarnation = 'uid-b' WHERE cluster = $1 AND tenant = $2`,
		testCluster, testTenant); err != nil {
		t.Fatalf("restamp delivery: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit restamp: %v", err)
	}

	d.deliver(context.Background(), row)
	if rec.count() != 1 {
		t.Fatalf("receiver got %d requests, want 1", rec.count())
	}
	if state, _ := webhookState(t, db, runID); state != "delivered" {
		t.Fatalf("state = %q, want delivered", state)
	}
}

// TestWebhookDispatch_TakeOverAfterLeaseDrops asserts a take-over that changes
// only the placement incarnation, after the lease, drops the delivery.
func TestWebhookDispatch_TakeOverAfterLeaseDrops(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	rec, srv := newWebhookReceiver(t, http.StatusNoContent)
	runID := seedWebhookRun(t, pool, db, "manual:webhook-takeover", srv.URL)
	stampWebhookIncarnation(t, db)

	d := newTestDispatcher(pool, srv, time.Now())
	row := leaseOneWebhookRow(t, d)

	if _, err := db.Exec(`UPDATE varroa.tenant_placements SET incarnation = 'uid-b' WHERE cluster = $1 AND tenant = $2`,
		testCluster, testTenant); err != nil {
		t.Fatalf("replace placement incarnation: %v", err)
	}

	d.deliver(context.Background(), row)
	if rec.count() != 0 {
		t.Fatalf("receiver got %d requests, want 0", rec.count())
	}
	if state, _ := webhookState(t, db, runID); state != "dropped" {
		t.Fatalf("state = %q, want dropped", state)
	}
	if got, want := webhookLastError(t, db, runID), "tenant webhook removed or the tenant was replaced"; got != want {
		t.Fatalf("last_error = %q, want %q", got, want)
	}
}

// TestWebhookDispatch_GuardedDropSkipsRestampedRow asserts a guarded drop
// that read a stale incarnation changes nothing, and one that read the stored
// incarnation drops the row.
func TestWebhookDispatch_GuardedDropSkipsRestampedRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	_, srv := newWebhookReceiver(t, http.StatusNoContent)
	runID := seedWebhookRun(t, pool, db, "manual:webhook-guarded", srv.URL)
	stampWebhookIncarnation(t, db)
	if _, err := db.Exec(`UPDATE varroa.webhook_deliveries SET incarnation = 'uid-b' WHERE cluster = $1 AND tenant = $2`,
		testCluster, testTenant); err != nil {
		t.Fatalf("restamp delivery: %v", err)
	}

	d := newTestDispatcher(pool, srv, time.Now())
	var id uuid.UUID
	if err := db.QueryRow(`SELECT delivery_id FROM varroa.webhook_deliveries WHERE run_id = $1 AND delivery_key = $2`,
		runID, webhookKeyCompleted).Scan(&id); err != nil {
		t.Fatalf("read delivery id: %v", err)
	}

	dropped, err := d.dropWebhookDelivery(context.Background(), id, "uid-a", "x")
	if err != nil || dropped {
		t.Fatalf("drop with stale incarnation = (%v, %v), want (false, nil)", dropped, err)
	}
	if state, _ := webhookState(t, db, runID); state != "pending" {
		t.Fatalf("state after stale drop = %q, want pending", state)
	}

	dropped, err = d.dropWebhookDelivery(context.Background(), id, "uid-b", "x")
	if err != nil || !dropped {
		t.Fatalf("drop with stored incarnation = (%v, %v), want (true, nil)", dropped, err)
	}
	if state, _ := webhookState(t, db, runID); state != "dropped" {
		t.Fatalf("state after drop = %q, want dropped", state)
	}
}

// TestWebhookDispatch_SkipsRowNoLongerPending asserts a leased row that a
// real completion moved out of pending is not sent and keeps its new state.
func TestWebhookDispatch_SkipsRowNoLongerPending(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	rec, srv := newWebhookReceiver(t, http.StatusNoContent)
	runID := seedWebhookRun(t, pool, db, "manual:webhook-not-pending", srv.URL)
	stampWebhookIncarnation(t, db)

	d := newTestDispatcher(pool, srv, time.Now())
	row := leaseOneWebhookRow(t, d)

	if _, err := db.Exec(`UPDATE varroa.webhook_deliveries SET state = 'dropped' WHERE delivery_id = $1`,
		row.deliveryID); err != nil {
		t.Fatalf("drop row: %v", err)
	}

	d.deliver(context.Background(), row)
	if rec.count() != 0 {
		t.Fatalf("receiver got %d requests, want 0", rec.count())
	}
	if state, _ := webhookState(t, db, runID); state != "dropped" {
		t.Fatalf("state = %q, want dropped", state)
	}
}

// TestWebhookDispatch_TenantMoveKeepsDelivery asserts a tenant move, which
// changes the cell and the generation but keeps the incarnation, after the
// lease, still delivers.
func TestWebhookDispatch_TenantMoveKeepsDelivery(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	rec, srv := newWebhookReceiver(t, http.StatusNoContent)
	runID := seedWebhookRun(t, pool, db, "manual:webhook-move", srv.URL)
	stampWebhookIncarnation(t, db)

	d := newTestDispatcher(pool, srv, time.Now())
	row := leaseOneWebhookRow(t, d)

	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements
		SET cell = 'cell-b', cell_instance = 'pod-b', generation = generation + 1
		WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant); err != nil {
		t.Fatalf("move tenant: %v", err)
	}

	d.deliver(context.Background(), row)
	if rec.count() != 1 {
		t.Fatalf("receiver got %d requests, want 1", rec.count())
	}
	if state, _ := webhookState(t, db, runID); state != "delivered" {
		t.Fatalf("state = %q, want delivered", state)
	}
}
