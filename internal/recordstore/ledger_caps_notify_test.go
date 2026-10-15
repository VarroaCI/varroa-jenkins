package recordstore

import (
	"context"
	"testing"
)

// TestClaim_CappedClaimPublishesWaitOnChangeOnly asserts that a capped run
// publishes one waiting notification when its row appears, nothing while
// the row keeps its values, and one cleared notification when the grant
// deletes it.
func TestClaim_CappedClaimPublishesWaitOnChangeOnly(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedCapPolicy(t, db, capTenantLimitOne)
	notifier, sent, _ := newCaptureNotifier(t)
	ledger := NewLedger(pool, nil)
	ledger.SetWaitNotifier(notifier)

	first := capRequest("cell-a")
	mustGrant(t, ledger, first)

	waiting := capRequest("cell-a")
	if resp := ledger.Claim(context.Background(), waiting); resp.ReasonCode != ReasonCapped {
		t.Fatalf("waiting claim = %+v, want CAPPED", resp)
	}
	if got := sent.count(); got != 1 {
		t.Fatalf("payloads after the first capped claim = %d, want 1", got)
	}
	if p := sent.payload(0); p.Kind != "wait" || p.State != WaitStateWaiting || p.RunID != waiting.RunID {
		t.Fatalf("first payload = %+v, want a waiting wait for %s", p, waiting.RunID)
	}

	if resp := ledger.Claim(context.Background(), waiting); resp.ReasonCode != ReasonCapped {
		t.Fatalf("repeat claim = %+v, want CAPPED", resp)
	}
	if got := sent.count(); got != 1 {
		t.Fatalf("a repeat with unchanged values published %d payloads, want 1", got)
	}

	capConsumeIntent(t, db, first.RunID)
	mustGrant(t, ledger, waiting)
	notifier.Close()
	if got := sent.count(); got != 2 {
		t.Fatalf("payloads after the grant = %d, want 2", got)
	}
	if p := sent.payload(1); p.Kind != "wait" || p.State != WaitStateCleared || p.RunID != waiting.RunID {
		t.Fatalf("last payload = %+v, want a cleared wait for %s", p, waiting.RunID)
	}
}

// TestClaim_UncappedTenantPublishesNoWait asserts that a claim of a tenant
// with no cap publishes no wait notification.
func TestClaim_UncappedTenantPublishesNoWait(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedCapPolicy(t, db, map[string]any{})
	notifier, sent, _ := newCaptureNotifier(t)
	ledger := NewLedger(pool, nil)
	ledger.SetWaitNotifier(notifier)

	mustGrant(t, ledger, capRequest("cell-a"))
	notifier.Close()
	if got := sent.count(); got != 0 {
		t.Fatalf("an uncapped claim published %d payloads, want 0", got)
	}
}

// TestClaim_RemovedLimitPublishesWaitCleared asserts that a waiting run
// whose limit was removed publishes one cleared notification when it is
// granted.
func TestClaim_RemovedLimitPublishesWaitCleared(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedCapPolicy(t, db, capTenantLimitOne)
	notifier, sent, _ := newCaptureNotifier(t)
	ledger := NewLedger(pool, nil)
	ledger.SetWaitNotifier(notifier)

	mustGrant(t, ledger, capRequest("cell-a"))
	waiting := capRequest("cell-a")
	if resp := ledger.Claim(context.Background(), waiting); resp.ReasonCode != ReasonCapped {
		t.Fatalf("waiting claim = %+v, want CAPPED", resp)
	}

	if _, err := db.Exec(`UPDATE varroa.tenant_placements SET policy = '{}'::jsonb WHERE cluster = $1 AND tenant = $2`,
		testCluster, testTenant); err != nil {
		t.Fatalf("remove cap policy: %v", err)
	}
	mustGrant(t, ledger, waiting)
	notifier.Close()
	if got := sent.count(); got != 2 {
		t.Fatalf("payloads = %d, want 2 (waiting, then cleared)", got)
	}
	if p := sent.payload(1); p.Kind != "wait" || p.State != WaitStateCleared || p.RunID != waiting.RunID {
		t.Fatalf("last payload = %+v, want a cleared wait for %s", p, waiting.RunID)
	}
}
