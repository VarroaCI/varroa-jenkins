package recordstore

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
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
	const rows = 5
	for i := 0; i < rows; i++ {
		key := fmt.Sprintf("manual:webhook-two-%d", i)
		runID := seedGrantedIntent(t, pool, key)
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
