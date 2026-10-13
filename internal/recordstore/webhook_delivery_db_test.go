package recordstore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// webhookReceiver records the requests a dispatcher sends and answers with
// status.
type webhookReceiver struct {
	mu       sync.Mutex
	status   int
	requests []webhookRequest
}

type webhookRequest struct {
	header http.Header
	body   []byte
}

func newWebhookReceiver(t *testing.T, status int) (*webhookReceiver, *httptest.Server) {
	t.Helper()
	rec := &webhookReceiver{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.requests = append(rec.requests, webhookRequest{header: r.Header.Clone(), body: body})
		rec.mu.Unlock()
		w.WriteHeader(rec.status)
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func (r *webhookReceiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// newTestDispatcher returns a dispatcher whose client reaches the loopback
// httptest receiver. Production never replaces the guarded client, which
// refuses loopback addresses, so this replacement is a test-only seam.
func newTestDispatcher(pool *pgxpool.Pool, srv *httptest.Server, now time.Time) *WebhookDispatcher {
	d := NewWebhookDispatcher(pool, NewActivityIngester(pool), nil, nil)
	d.client = srv.Client()
	d.client.Timeout = webhookTimeout
	d.now = func() time.Time { return now }
	return d
}

// seedWebhookRun grants one run of testTenant, stores a tenant webhook that
// points at url, and applies its owner run.completed. It returns the run id.
func seedWebhookRun(t *testing.T, pool *pgxpool.Pool, db *sql.DB, intentKey, url string) uuid.UUID {
	t.Helper()
	runID := seedGrantedIntent(t, pool, intentKey)
	seedTenantPlacement(t, db, testTenant, testNamespace, "cell-a", 0, "pod-a", "", nil)
	if url != "" {
		if _, err := db.Exec(`
			UPDATE varroa.tenant_placements SET notification_webhook = $1::jsonb
			WHERE cluster = $2 AND tenant = $3
		`, `{"url":"`+url+`","key":"whsec-test"}`, testCluster, testTenant); err != nil {
			t.Fatalf("store tenant webhook: %v", err)
		}
	}
	g := NewIngester(pool, nil, nil)
	g.SetWebhooks("https://varroa.example.test", nil)
	env := newTestEnvelope(t, runID, intentKey, KindRunCompleted,
		RunCompletedPayload{Result: strPtr("SUCCESS")})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	return runID
}

func webhookRowCount(t *testing.T, db *sql.DB, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.webhook_deliveries WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("count webhook deliveries: %v", err)
	}
	return n
}

// webhookState reads the owner completion row of runID.
func webhookState(t *testing.T, db *sql.DB, runID uuid.UUID) (string, int) {
	t.Helper()
	return webhookStateFor(t, db, runID, webhookKeyCompleted)
}

// webhookStateFor reads the row of runID that carries delivery key key.
func webhookStateFor(t *testing.T, db *sql.DB, runID uuid.UUID, key string) (string, int) {
	t.Helper()
	var state string
	var attempts int
	if err := db.QueryRow(`SELECT state, attempts FROM varroa.webhook_deliveries WHERE run_id = $1 AND delivery_key = $2`,
		runID, key).Scan(&state, &attempts); err != nil {
		t.Fatalf("read webhook delivery %q: %v", key, err)
	}
	return state, attempts
}

// TestRunCompleted_EnqueuesOnceForOwner asserts one outbox row for the
// owner's terminal write, and none for a redelivery of the same completion.
func TestRunCompleted_EnqueuesOnceForOwner(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedWebhookRun(t, pool, db, "manual:webhook-once", "https://receiver.example.test/hook")
	if n := webhookRowCount(t, db, runID); n != 1 {
		t.Fatalf("delivery rows = %d, want 1", n)
	}

	env := newTestEnvelope(t, runID, "manual:webhook-once", KindRunCompleted,
		RunCompletedPayload{Result: strPtr("SUCCESS")})
	if _, err := NewIngester(pool, nil, nil).Apply(context.Background(), env); err != nil {
		t.Fatalf("redeliver run.completed: %v", err)
	}
	if n := webhookRowCount(t, db, runID); n != 1 {
		t.Fatalf("delivery rows after redelivery = %d, want 1", n)
	}
}

// TestRunCompleted_NoWebhookEnqueuesNothing asserts a tenant that declares
// no webhook gets no outbox row.
func TestRunCompleted_NoWebhookEnqueuesNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedWebhookRun(t, pool, db, "manual:webhook-none", "")
	if n := webhookRowCount(t, db, runID); n != 0 {
		t.Fatalf("delivery rows = %d, want 0", n)
	}
}

// TestWebhookDispatch_DeliversSignedBody asserts the four headers, the
// signature over the timestamp and the stored bytes, and the delivered state.
func TestWebhookDispatch_DeliversSignedBody(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	rec, srv := newWebhookReceiver(t, http.StatusNoContent)
	runID := seedWebhookRun(t, pool, db, "manual:webhook-deliver", srv.URL)

	now := time.Unix(1700000000, 0)
	newTestDispatcher(pool, srv, now).round(context.Background())

	if rec.count() != 1 {
		t.Fatalf("receiver got %d requests, want 1", rec.count())
	}
	req := rec.requests[0]
	if got := req.header.Get("X-Varroa-Event"); got != "run.completed" {
		t.Fatalf("X-Varroa-Event = %q", got)
	}
	ts := req.header.Get("X-Varroa-Timestamp")
	if ts != strconv.FormatInt(now.Unix(), 10) {
		t.Fatalf("X-Varroa-Timestamp = %q", ts)
	}
	if req.header.Get("X-Varroa-Delivery") == "" {
		t.Fatalf("X-Varroa-Delivery is empty")
	}
	mac := hmac.New(sha256.New, []byte("whsec-test"))
	mac.Write([]byte(ts + "."))
	mac.Write(req.body)
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))
	if got := req.header.Get("X-Varroa-Signature"); got != want {
		t.Fatalf("X-Varroa-Signature = %q, want %q", got, want)
	}
	if state, _ := webhookState(t, db, runID); state != "delivered" {
		t.Fatalf("state = %q, want delivered", state)
	}
}

// TestWebhookDispatch_FailsAfterSixAttemptsAndWritesActivity asserts the
// retry ladder ends in failed on the sixth attempt, and that the failure
// writes one tenant.webhook_failed activity event.
func TestWebhookDispatch_FailsAfterSixAttemptsAndWritesActivity(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	rec, srv := newWebhookReceiver(t, http.StatusInternalServerError)
	runID := seedWebhookRun(t, pool, db, "manual:webhook-fail", srv.URL)

	d := newTestDispatcher(pool, srv, time.Now())
	for i := 0; i < webhookMaxAttempts; i++ {
		// Make the row due again without waiting out the backoff.
		if _, err := db.Exec(`UPDATE varroa.webhook_deliveries SET next_attempt_at = now() - interval '1 second' WHERE run_id = $1`, runID); err != nil {
			t.Fatalf("make row due: %v", err)
		}
		d.round(context.Background())
	}
	if rec.count() != webhookMaxAttempts {
		t.Fatalf("receiver got %d requests, want %d", rec.count(), webhookMaxAttempts)
	}
	state, attempts := webhookState(t, db, runID)
	if state != "failed" || attempts != webhookMaxAttempts {
		t.Fatalf("state = %q attempts = %d, want failed and %d", state, attempts, webhookMaxAttempts)
	}
	var events int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.activity WHERE type = 'tenant.webhook_failed' AND severity = 'error'`).Scan(&events); err != nil {
		t.Fatalf("count activity: %v", err)
	}
	if events != 1 {
		t.Fatalf("tenant.webhook_failed events = %d, want 1", events)
	}
}

// TestWebhookDispatch_DropsWhenWebhookRemoved asserts a delivery whose tenant
// no longer declares a webhook ends dropped and sends nothing.
func TestWebhookDispatch_DropsWhenWebhookRemoved(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	rec, srv := newWebhookReceiver(t, http.StatusNoContent)
	runID := seedWebhookRun(t, pool, db, "manual:webhook-drop", srv.URL)
	if _, err := db.Exec(`UPDATE varroa.tenant_placements SET notification_webhook = NULL WHERE cluster = $1 AND tenant = $2`,
		testCluster, testTenant); err != nil {
		t.Fatalf("clear tenant webhook: %v", err)
	}

	newTestDispatcher(pool, srv, time.Now()).round(context.Background())
	if rec.count() != 0 {
		t.Fatalf("receiver got %d requests, want 0", rec.count())
	}
	if state, _ := webhookState(t, db, runID); state != "dropped" {
		t.Fatalf("state = %q, want dropped", state)
	}
}
