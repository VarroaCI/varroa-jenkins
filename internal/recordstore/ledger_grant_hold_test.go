package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// freshConsumer answers a consumer with no backlog.
func freshConsumer(context.Context) (*nats.ConsumerInfo, error) {
	return &nats.ConsumerInfo{}, nil
}

// newHeldLedger returns a Ledger over pool with the grant hold wired to
// consumer and the default grace.
func newHeldLedger(t *testing.T, consumer func(context.Context) (*nats.ConsumerInfo, error)) *Ledger {
	t.Helper()
	pool, _ := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	ledger.hold = newGrantHold(consumer, 0, 0)
	return ledger
}

// newHeldLedgerWithDB is newHeldLedger that also answers the test's own
// database handle.
func newHeldLedgerWithDB(t *testing.T, consumer func(context.Context) (*nats.ConsumerInfo, error)) (*Ledger, *sql.DB) {
	t.Helper()
	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	ledger.hold = newGrantHold(consumer, 0, 0)
	return ledger, db
}

// seedHoldPlacement inserts the tenant's placement row with pod-a
// listed dead. A non-nil confirmedAgo stamps a pod-read confirmation that
// long before now.
func seedHoldPlacement(t *testing.T, db *sql.DB, confirmedAgo *time.Duration) {
	t.Helper()
	now := time.Now().UTC()
	entry := TenantSyncDeadInstance{Instance: "pod-a", LastSeenAt: now.Add(-time.Minute)}
	if confirmedAgo != nil {
		at := now.Add(-*confirmedAgo)
		entry.ConfirmedGoneAt = &at
	}
	deadJSON, err := json.Marshal([]TenantSyncDeadInstance{entry})
	if err != nil {
		t.Fatalf("marshal dead instances: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements
			(cluster, tenant, namespace, cell, generation, cell_instance, rehydrated_instance,
			 cell_alive, dead_instances)
		VALUES ($1, $2, $3, 'cell-a', 1, 'pod-b', 'pod-b', true, $4::jsonb)
	`, testCluster, testTenant, testNamespace, string(deadJSON)); err != nil {
		t.Fatalf("seed tenant placement: %v", err)
	}
}

// readGrantedAt reads one intent's granted_at.
func readGrantedAt(t *testing.T, db *sql.DB, intentKey string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := db.QueryRow(`SELECT granted_at FROM varroa.run_intents WHERE intent_key = $1`, intentKey).Scan(&at); err != nil {
		t.Fatalf("read granted_at: %v", err)
	}
	return at
}

// readOwner reads one intent's cell_instance.
func readOwner(t *testing.T, db *sql.DB, intentKey string) string {
	t.Helper()
	var owner sql.NullString
	if err := db.QueryRow(`SELECT cell_instance FROM varroa.run_intents WHERE intent_key = $1`, intentKey).Scan(&owner); err != nil {
		t.Fatalf("read cell_instance: %v", err)
	}
	return owner.String
}

// repeatFrom returns req as a repeat from cell-b/instance.
func repeatFrom(req ClaimRequest, instance string) ClaimRequest {
	repeat := req
	repeat.Cell = "cell-b"
	repeat.CellInstance = instance
	return repeat
}

// assertHeld fails the test unless resp is a NOT_READY veto that names
// runID.
func assertHeld(t *testing.T, resp ClaimResponse, runID string) {
	t.Helper()
	if resp.Decision != DecisionVetoed || resp.ReasonCode != ReasonNotReady || resp.RunID != runID {
		t.Fatalf("want vetoed NOT_READY for run %s, got %+v", runID, resp)
	}
	if resp.Message == "" {
		t.Fatal("want a message on the held answer")
	}
}

// TestRepeatOtherInstanceHeldWhileOwnerLive: a repeat from another
// instance waits while no dead-instance entry confirms the stored owner
// gone and the grant is new. The row keeps its owner.
func TestRepeatOtherInstanceHeldWhileOwnerLive(t *testing.T) {
	t.Parallel()

	ledger, db := newHeldLedgerWithDB(t, freshConsumer)

	req := newTestClaimRequest("manual:hold-live", 1)
	first := mustGrant(t, ledger, req)
	before := readGrantedAt(t, db, req.IntentKey)

	assertHeld(t, ledger.Claim(context.Background(), repeatFrom(req, "pod-b")), first.RunID)
	if owner := readOwner(t, db, req.IntentKey); owner != "pod-a" {
		t.Fatalf("want the owner kept at pod-a, got %s", owner)
	}
	if after := readGrantedAt(t, db, req.IntentKey); !after.Equal(*before) {
		t.Fatalf("granted_at moved from %v to %v", before, after)
	}
}

// TestRepeatOtherInstanceMovesAfterConfirmedGone: a pod-read confirmation
// older than the grace releases the hold, and the move refreshes
// granted_at.
func TestRepeatOtherInstanceMovesAfterConfirmedGone(t *testing.T) {
	t.Parallel()

	ledger, db := newHeldLedgerWithDB(t, freshConsumer)

	req := newTestClaimRequest("manual:hold-gone", 1)
	first := mustGrant(t, ledger, req)
	if _, err := db.Exec(`UPDATE varroa.run_intents SET granted_at = now() - interval '1 minute' WHERE intent_key = $1`,
		req.IntentKey); err != nil {
		t.Fatalf("age the grant: %v", err)
	}
	before := readGrantedAt(t, db, req.IntentKey)
	ago := 45 * time.Second
	seedHoldPlacement(t, db, &ago)

	resp := ledger.Claim(context.Background(), repeatFrom(req, "pod-b"))
	if resp.Decision != DecisionGranted || resp.RunID != first.RunID {
		t.Fatalf("want granted run %s, got %+v", first.RunID, resp)
	}
	if owner := readOwner(t, db, req.IntentKey); owner != "pod-b" {
		t.Fatalf("want the owner moved to pod-b, got %s", owner)
	}
	if after := readGrantedAt(t, db, req.IntentKey); !after.After(*before) {
		t.Fatalf("want granted_at refreshed past %v, got %v", before, after)
	}
}

// TestRepeatOtherInstanceHeldWithinGrace: a confirmation newer than the
// grace keeps the hold, and so does an entry with no confirmation.
func TestRepeatOtherInstanceHeldWithinGrace(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ago  *time.Duration
	}{
		{name: "confirmed inside the grace", ago: func() *time.Duration { d := 5 * time.Second; return &d }()},
		{name: "listed with no confirmation", ago: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger, db := newHeldLedgerWithDB(t, freshConsumer)
			req := newTestClaimRequest("manual:hold-grace", 1)
			first := mustGrant(t, ledger, req)
			seedHoldPlacement(t, db, tc.ago)

			assertHeld(t, ledger.Claim(context.Background(), repeatFrom(req, "pod-b")), first.RunID)
			if owner := readOwner(t, db, req.IntentKey); owner != "pod-a" {
				t.Fatalf("want the owner kept at pod-a, got %s", owner)
			}
		})
	}
}

// TestRepeatOtherInstanceMovesAfterHeartbeatWindow: a grant older than
// the moved window releases the hold with no dead-instance entry. A live
// owner that started the build would have sent a heartbeat by then.
func TestRepeatOtherInstanceMovesAfterHeartbeatWindow(t *testing.T) {
	t.Parallel()

	ledger, db := newHeldLedgerWithDB(t, freshConsumer)

	req := newTestClaimRequest("manual:hold-window", 1)
	first := mustGrant(t, ledger, req)
	if _, err := db.Exec(`UPDATE varroa.run_intents SET granted_at = now() - interval '6 minutes' WHERE intent_key = $1`,
		req.IntentKey); err != nil {
		t.Fatalf("age the grant: %v", err)
	}

	resp := ledger.Claim(context.Background(), repeatFrom(req, "pod-b"))
	if resp.Decision != DecisionGranted || resp.RunID != first.RunID {
		t.Fatalf("want granted run %s, got %+v", first.RunID, resp)
	}
	if owner := readOwner(t, db, req.IntentKey); owner != "pod-b" {
		t.Fatalf("want the owner moved to pod-b, got %s", owner)
	}
}

// TestRepeatOtherInstanceHeldWhileConsumerBacklogged: a release test that
// passes still holds the claim while the ingest consumer is behind or its
// state is not readable, because the owner's start can be in transit.
func TestRepeatOtherInstanceHeldWhileConsumerBacklogged(t *testing.T) {
	t.Parallel()

	stale := time.Now().Add(-5 * time.Minute)
	cases := []struct {
		name     string
		consumer func(context.Context) (*nats.ConsumerInfo, error)
	}{
		{name: "pending over the bound", consumer: func(context.Context) (*nats.ConsumerInfo, error) {
			return &nats.ConsumerInfo{NumPending: DefaultSweepMaxPending}, nil
		}},
		{name: "pending with a stale floor", consumer: func(context.Context) (*nats.ConsumerInfo, error) {
			return &nats.ConsumerInfo{NumPending: 3, AckFloor: nats.SequenceInfo{Last: &stale}}, nil
		}},
		{name: "consumer state not readable", consumer: func(context.Context) (*nats.ConsumerInfo, error) {
			return nil, errors.New("no responders")
		}},
		{name: "a redelivered message in flight", consumer: func(context.Context) (*nats.ConsumerInfo, error) {
			return &nats.ConsumerInfo{NumRedelivered: 1, NumAckPending: 1}, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger, db := newHeldLedgerWithDB(t, tc.consumer)
			req := newTestClaimRequest("manual:hold-backlog", 1)
			first := mustGrant(t, ledger, req)
			ago := 45 * time.Second
			seedHoldPlacement(t, db, &ago)

			assertHeld(t, ledger.Claim(context.Background(), repeatFrom(req, "pod-b")), first.RunID)
			if owner := readOwner(t, db, req.IntentKey); owner != "pod-a" {
				t.Fatalf("want the owner kept at pod-a, got %s", owner)
			}
		})
	}
}

// TestHeldRepeatAnswersDuplicateOnceStartIngested: the owner's start lands
// while the repeat waits. The next retry answers duplicate and leaves the
// started run's own row alone.
func TestHeldRepeatAnswersDuplicateOnceStartIngested(t *testing.T) {
	t.Parallel()

	ledger, db := newHeldLedgerWithDB(t, freshConsumer)

	req := newTestClaimRequest("manual:hold-started", 1)
	first := mustGrant(t, ledger, req)
	repeat := repeatFrom(req, "pod-b")
	assertHeld(t, ledger.Claim(context.Background(), repeat), first.RunID)

	insertStubRunsRow(t, db, first.RunID)
	resp := ledger.Claim(context.Background(), repeat)
	if resp.Decision != DecisionDuplicate || resp.RunID != first.RunID {
		t.Fatalf("want duplicate for run %s, got %+v", first.RunID, resp)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, first.RunID).Scan(&status); err != nil {
		t.Fatalf("read runs status: %v", err)
	}
	if status != StatusRunning {
		t.Fatalf("want the started run kept RUNNING, got %s", status)
	}
	if owner := readOwner(t, db, req.IntentKey); owner != "pod-a" {
		t.Fatalf("want the owner kept at pod-a, got %s", owner)
	}
}

// TestRepeatSameInstanceNeverHeld: a repeat from the stored owner moves no
// owner, so the hold never reads the consumer for it.
func TestRepeatSameInstanceNeverHeld(t *testing.T) {
	t.Parallel()

	ledger := newHeldLedger(t, func(context.Context) (*nats.ConsumerInfo, error) {
		t.Fatal("the hold read the consumer for a same-instance repeat")
		return nil, nil
	})

	req := newTestClaimRequest("manual:hold-same", 1)
	first := mustGrant(t, ledger, req)
	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionGranted || resp.RunID != first.RunID {
		t.Fatalf("want granted run %s, got %+v", first.RunID, resp)
	}
}

// TestRepeatEmptyStoredInstanceNeverHeld: an intent that names no owner
// holds no grant to wait for. A request that names no instance is never
// the owner, so the hold covers it.
func TestRepeatEmptyStoredInstanceNeverHeld(t *testing.T) {
	t.Parallel()

	ledger := newHeldLedger(t, freshConsumer)

	req := newTestClaimRequest("manual:hold-empty-stored", 1)
	req.CellInstance = ""
	first := mustGrant(t, ledger, req)
	resp := ledger.Claim(context.Background(), repeatFrom(req, "pod-b"))
	if resp.Decision != DecisionGranted || resp.RunID != first.RunID {
		t.Fatalf("want granted run %s for an unowned intent, got %+v", first.RunID, resp)
	}

	owned := newTestClaimRequest("manual:hold-empty-request", 1)
	second := mustGrant(t, ledger, owned)
	assertHeld(t, ledger.Claim(context.Background(), repeatFrom(owned, "")), second.RunID)
}

// TestRefireClaimPassesHold: the re-fire publishes only for an owner the
// operator confirmed gone past the grace. The repeat claim from the
// replacement cell therefore passes the hold at once.
func TestRefireClaimPassesHold(t *testing.T) {
	t.Parallel()

	ledger, db := newHeldLedgerWithDB(t, freshConsumer)

	req := newTestClaimRequest("manual:hold-refire", 1)
	req.CellInstance = refireDeadInstance
	first := mustGrant(t, ledger, req)
	seedRefirePlacement(t, db, "pod-new", "pod-new", true)

	resp := ledger.Claim(context.Background(), repeatFrom(req, "pod-new"))
	if resp.Decision != DecisionGranted || resp.RunID != first.RunID {
		t.Fatalf("want granted run %s, got %+v", first.RunID, resp)
	}
	if owner := readOwner(t, db, req.IntentKey); owner != "pod-new" {
		t.Fatalf("want the owner moved to pod-new, got %s", owner)
	}
}

// TestGrantedAtWrittenOnInsertTransferAndMove: the insert, a transfer to
// a new run, and an ownership move each write granted_at. A same-instance
// repeat leaves it alone.
func TestGrantedAtWrittenOnInsertTransferAndMove(t *testing.T) {
	t.Parallel()

	ledger, db := newHeldLedgerWithDB(t, freshConsumer)
	age := func(key string) {
		t.Helper()
		if _, err := db.Exec(`UPDATE varroa.run_intents SET granted_at = now() - interval '10 minutes' WHERE intent_key = $1`,
			key); err != nil {
			t.Fatalf("age the grant: %v", err)
		}
	}

	req := newTestClaimRequest("manual:granted-at", 1)
	mustGrant(t, ledger, req)
	inserted := readGrantedAt(t, db, req.IntentKey)
	if inserted == nil {
		t.Fatal("want granted_at set on the insert")
	}

	mustGrant(t, ledger, req)
	if same := readGrantedAt(t, db, req.IntentKey); !same.Equal(*inserted) {
		t.Fatalf("a same-instance repeat moved granted_at from %v to %v", inserted, same)
	}

	age(req.IntentKey)
	aged := readGrantedAt(t, db, req.IntentKey)
	mustGrant(t, ledger, repeatFrom(req, "pod-b"))
	if moved := readGrantedAt(t, db, req.IntentKey); !moved.After(*aged) {
		t.Fatalf("want the move to refresh granted_at past %v, got %v", aged, moved)
	}

	age(req.IntentKey)
	aged = readGrantedAt(t, db, req.IntentKey)
	transfer := newTestClaimRequest(req.IntentKey, 2)
	mustGrant(t, ledger, transfer)
	assertStoredRunID(t, db, req.IntentKey, transfer.RunID)
	if moved := readGrantedAt(t, db, req.IntentKey); !moved.After(*aged) {
		t.Fatalf("want the transfer to refresh granted_at past %v, got %v", aged, moved)
	}
}

// TestUnwiredLedgerRunsNoHold: a Ledger that SetGrantHold never touched
// moves the owner on a repeat from another instance at once.
func TestUnwiredLedgerRunsNoHold(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	if ledger.GrantHoldWired() {
		t.Fatal("a new ledger holds the grant hold")
	}

	req := newTestClaimRequest("manual:hold-unwired", 1)
	first := mustGrant(t, ledger, req)
	resp := ledger.Claim(context.Background(), repeatFrom(req, "pod-b"))
	if resp.Decision != DecisionGranted || resp.RunID != first.RunID {
		t.Fatalf("want granted run %s, got %+v", first.RunID, resp)
	}
	if owner := readOwner(t, db, req.IntentKey); owner != "pod-b" {
		t.Fatalf("want the owner moved to pod-b, got %s", owner)
	}
}

// TestGrantHoldReadsConsumerWithNoRowLock: the consumer read for a
// released claim runs between two transactions, so it holds no lock on
// the intent row or the placement row.
func TestGrantHoldReadsConsumerWithNoRowLock(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	req := newTestClaimRequest("manual:hold-nolock", 1)
	reads := 0
	ledger.hold = newGrantHold(func(ctx context.Context) (*nats.ConsumerInfo, error) {
		reads++
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin probe transaction: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		probes := []struct{ query, arg string }{
			{`SELECT 1 FROM varroa.run_intents WHERE intent_key = $1 FOR UPDATE NOWAIT`, req.IntentKey},
			{`SELECT 1 FROM varroa.tenant_placements WHERE tenant = $1 FOR UPDATE NOWAIT`, req.Tenant},
		}
		for _, probe := range probes {
			if _, err := tx.ExecContext(ctx, probe.query, probe.arg); err != nil {
				t.Fatalf("a row is locked during the consumer read: %v", err)
			}
		}
		return &nats.ConsumerInfo{}, nil
	}, 0, 0)

	first := mustGrant(t, ledger, req)
	ago := 45 * time.Second
	seedHoldPlacement(t, db, &ago)

	resp := ledger.Claim(context.Background(), repeatFrom(req, "pod-b"))
	if resp.Decision != DecisionGranted || resp.RunID != first.RunID {
		t.Fatalf("want granted run %s, got %+v", first.RunID, resp)
	}
	if reads != 1 {
		t.Fatalf("want one consumer read, got %d", reads)
	}
}

// TestGrantHoldGraceNeverBelowAckWait: a confirmed-gone grace below the
// ingest AckWait takes the AckWait, so an unacknowledged start is a
// redelivery by the time release test (a) passes.
func TestGrantHoldGraceNeverBelowAckWait(t *testing.T) {
	cases := []struct {
		name  string
		grace time.Duration
		want  time.Duration
	}{
		{name: "zero takes the default", grace: 0, want: DefaultConfirmedGoneGrace},
		{name: "a short grace takes the AckWait", grace: 5 * time.Second, want: ackWait},
		{name: "a long grace is kept", grace: 90 * time.Second, want: 90 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := newGrantHold(freshConsumer, tc.grace, 0).grace; got != tc.want {
				t.Fatalf("grace = %v, want %v", got, tc.want)
			}
		})
	}
}
