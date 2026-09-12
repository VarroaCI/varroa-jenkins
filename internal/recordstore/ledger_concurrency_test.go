package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// seedTenantPlacementPolicy inserts a minimal varroa.tenant_placements
// row on testCluster/testTenant carrying only cluster, tenant, and
// policy, for a test that does not need the placement-fencing columns.
func seedTenantPlacementPolicy(t *testing.T, db *sql.DB, policyJSON string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements (cluster, tenant, policy) VALUES ($1, $2, $3::jsonb)
	`, testCluster, testTenant, policyJSON); err != nil {
		t.Fatalf("seed tenant placement policy: %v", err)
	}
}

// seedOlderRun inserts one varroa.runs row directly, status RUNNING,
// matching req's group (cluster/tenant/job_path), with created_at
// ageOffset before now. Concurrency-cancel selection reads runs
// directly, so no run_intents row is needed.
func seedOlderRun(t *testing.T, db *sql.DB, req ClaimRequest, ageOffset time.Duration) uuid.UUID {
	t.Helper()
	runID := uuid.Must(uuid.NewV7())
	createdAt := time.Now().UTC().Add(-ageOffset)
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'RUNNING')
	`, runID, createdAt, "manual:older-"+runID.String(), req.Cluster, req.Namespace, req.Cell, req.Tenant, req.JobPath); err != nil {
		t.Fatalf("seed older run: %v", err)
	}
	return runID
}

// subscribeCancelActions subscribes to bus.LedgerActionSubject(testCluster)
// and returns a buffered channel of every decoded LedgerAction.
func subscribeCancelActions(t *testing.T, conn *bus.Conn, buffer int) chan LedgerAction {
	t.Helper()
	received := make(chan LedgerAction, buffer)
	sub, err := conn.Subscribe(bus.LedgerActionSubject(testCluster), func(msg *nats.Msg) {
		var a LedgerAction
		_ = json.Unmarshal(msg.Data, &a)
		received <- a
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	conn.NATSConn().Flush()
	return received
}

// TestConcurrencyCancelPublishesForOlderRun asserts a granted claim for a
// cancelInProgress tenant publishes one cancel_run action for an older,
// non-terminal run of the same group, and counts it under both
// recorder_concurrency_cancel_total and recorder_actions_published_total.
func TestConcurrencyCancelPublishesForOlderRun(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedTenantPlacementPolicy(t, db, `{"concurrency":"cancelInProgress"}`)

	req := newTestClaimRequest("manual:concurrency-basic", 0)
	older := seedOlderRun(t, db, req, 10*time.Minute)

	conn := newTestActionConn(t)
	received := subscribeCancelActions(t, conn, 4)

	reader, restore := swapMeterProvider(t)
	defer restore()
	concurrencyMetrics, err := NewConcurrencyMetrics()
	if err != nil {
		t.Fatalf("new concurrency metrics: %v", err)
	}
	actionMetrics, err := NewActionMetrics()
	if err != nil {
		t.Fatalf("new action metrics: %v", err)
	}

	ledger := NewLedger(pool, nil)
	ledger.SetConcurrencyCancel(conn, concurrencyMetrics, actionMetrics, nil)
	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionGranted {
		t.Fatalf("claim: want granted, got %+v", resp)
	}

	select {
	case a := <-received:
		if a.Kind != LedgerActionCancelRun || a.RunID != older.String() || a.JobPath != testJobPath {
			t.Fatalf("got %+v, want a cancel_run for the older run %s", a, older)
		}
		if a.Namespace != testNamespace || a.Controller != "cell-a" {
			t.Fatalf("got namespace=%q controller=%q, want %q/%q", a.Namespace, a.Controller, testNamespace, "cell-a")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("want a cancel_run action for the older run")
	}

	if got := metricValue(t, reader, "recorder_concurrency_cancel_total"); got != 1 {
		t.Fatalf("recorder_concurrency_cancel_total = %d, want 1", got)
	}
	if got := sumCounterByAttr(t, reader, "recorder_actions_published_total", "kind", LedgerActionCancelRun); got != 1 {
		t.Fatalf("recorder_actions_published_total{kind=cancel_run} = %d, want 1", got)
	}
}

// TestConcurrencyCancelBoundedAtEight asserts eleven older non-terminal
// runs produce eight published cancels, oldest first, and three counted
// skipped.
func TestConcurrencyCancelBoundedAtEight(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedTenantPlacementPolicy(t, db, `{"concurrency":"cancelInProgress"}`)

	req := newTestClaimRequest("manual:concurrency-bound", 0)
	const olderCount = 11
	oldest := make(map[uuid.UUID]bool, olderCount)
	for i := 0; i < olderCount; i++ {
		// Each older run is strictly older than the next, and all are
		// older than the new claim's own created_at (~now).
		id := seedOlderRun(t, db, req, time.Duration(olderCount-i)*time.Minute+time.Minute)
		oldest[id] = true
	}

	conn := newTestActionConn(t)
	received := subscribeCancelActions(t, conn, olderCount)

	reader, restore := swapMeterProvider(t)
	defer restore()
	concurrencyMetrics, err := NewConcurrencyMetrics()
	if err != nil {
		t.Fatalf("new concurrency metrics: %v", err)
	}
	actionMetrics, err := NewActionMetrics()
	if err != nil {
		t.Fatalf("new action metrics: %v", err)
	}

	ledger := NewLedger(pool, nil)
	ledger.SetConcurrencyCancel(conn, concurrencyMetrics, actionMetrics, nil)
	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionGranted {
		t.Fatalf("claim: want granted, got %+v", resp)
	}

	got := 0
	deadline := time.After(3 * time.Second)
	seen := map[string]bool{}
collect:
	for {
		select {
		case a := <-received:
			if a.Kind != LedgerActionCancelRun {
				t.Fatalf("got action kind %q, want cancel_run", a.Kind)
			}
			if !oldest[uuid.MustParse(a.RunID)] {
				t.Fatalf("got cancel for unknown run %s", a.RunID)
			}
			if seen[a.RunID] {
				t.Fatalf("run %s cancelled twice", a.RunID)
			}
			seen[a.RunID] = true
			got++
		case <-time.After(300 * time.Millisecond):
			break collect
		case <-deadline:
			break collect
		}
	}
	if got != 8 {
		t.Fatalf("published cancels = %d, want 8", got)
	}

	if got := metricValue(t, reader, "recorder_concurrency_cancel_total"); got != 8 {
		t.Fatalf("recorder_concurrency_cancel_total = %d, want 8", got)
	}
	if got := metricValue(t, reader, "recorder_concurrency_cancel_skipped_total"); got != 3 {
		t.Fatalf("recorder_concurrency_cancel_skipped_total = %d, want 3", got)
	}
}

// TestConcurrencyCancelAllowTenantPublishesNothing asserts a tenant
// whose policy is allow (the default) triggers no cancel_run action.
func TestConcurrencyCancelAllowTenantPublishesNothing(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedTenantPlacementPolicy(t, db, `{"concurrency":"allow"}`)

	req := newTestClaimRequest("manual:concurrency-allow", 0)
	seedOlderRun(t, db, req, 10*time.Minute)

	conn := newTestActionConn(t)
	received := subscribeCancelActions(t, conn, 4)

	ledger := NewLedger(pool, nil)
	ledger.SetConcurrencyCancel(conn, nil, nil, nil)
	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionGranted {
		t.Fatalf("claim: want granted, got %+v", resp)
	}

	select {
	case a := <-received:
		t.Fatalf("want no cancel_run action for an allow tenant, got %+v", a)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestConcurrencyCancelAnswersClaimBeforePublishing asserts Claim
// returns its decision before the concurrency cancel publishes,
// following design D10's "answer the claim before the cancel".
func TestConcurrencyCancelAnswersClaimBeforePublishing(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedTenantPlacementPolicy(t, db, `{"concurrency":"cancelInProgress"}`)

	req := newTestClaimRequest("manual:concurrency-ordering", 0)
	seedOlderRun(t, db, req, 10*time.Minute)

	conn := newTestActionConn(t)
	received := subscribeCancelActions(t, conn, 4)

	ledger := NewLedger(pool, nil)
	ledger.SetConcurrencyCancel(conn, nil, nil, nil)
	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionGranted {
		t.Fatalf("claim: want granted, got %+v", resp)
	}

	// The claim's own database work (a transaction commit) already takes
	// longer than a bare function return, so the cancel's own
	// goroutine, database read, and publish have essentially never
	// finished by the time Claim returns.
	select {
	case a := <-received:
		t.Fatalf("want the claim answered before any cancel_run publish, got %+v already published", a)
	default:
	}

	select {
	case a := <-received:
		if a.Kind != LedgerActionCancelRun {
			t.Fatalf("got %+v, want a cancel_run action eventually", a)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("want a cancel_run action published eventually")
	}
}
