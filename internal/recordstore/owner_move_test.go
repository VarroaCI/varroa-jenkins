package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// ownerMoveMarker is the run id of the marker signal a test publishes on
// the recorder's own connection. NATS delivers the messages of one
// connection in publish order, so a signal that the recorder published
// before the marker arrives before it.
const ownerMoveMarker = "marker"

// ledgerMessage is one message on varroa.ledger.> and the owner that a
// ledger read found when it arrived.
type ledgerMessage struct {
	subject  string
	runID    string
	kind     string
	instance string
	found    bool
}

// collectLedgerMessages subscribes to every ledger subject of conn on one
// subscription, so the channel keeps the publish order of owner moves and
// actions. For each owner move it reads the run's intent row at once: a
// signal sent after the commit finds the new owner.
func collectLedgerMessages(t *testing.T, conn *bus.Conn, db *sql.DB) chan ledgerMessage {
	t.Helper()
	received := make(chan ledgerMessage, 16)
	sub, err := conn.Subscribe("varroa.ledger.>", func(msg *nats.Msg) {
		m := ledgerMessage{subject: msg.Subject}
		switch msg.Subject {
		case bus.LedgerMoveSubject(testCluster):
			var move bus.OwnerMove
			_ = json.Unmarshal(msg.Data, &move)
			m.runID = move.RunID
			if move.RunID != ownerMoveMarker {
				var instance sql.NullString
				err := db.QueryRow(`SELECT cell_instance FROM varroa.run_intents WHERE run_id = $1`, move.RunID).
					Scan(&instance)
				m.found, m.instance = err == nil, instance.String
			}
		case bus.LedgerActionSubject(testCluster):
			var action LedgerAction
			_ = json.Unmarshal(msg.Data, &action)
			m.runID, m.kind = action.RunID, action.Kind
		}
		received <- m
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := conn.NATSConn().Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return received
}

// nextLedgerMessage returns the next message, or fails the test.
func nextLedgerMessage(t *testing.T, received chan ledgerMessage) ledgerMessage {
	t.Helper()
	select {
	case m := <-received:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no ledger message arrived")
		return ledgerMessage{}
	}
}

// expectNoLedgerMessage publishes the marker on conn and asserts that the
// marker is the next message, so nothing went out before it.
func expectNoLedgerMessage(t *testing.T, conn *bus.Conn, received chan ledgerMessage) {
	t.Helper()
	if err := PublishOwnerMove(conn, testCluster, ownerMoveMarker); err != nil {
		t.Fatalf("publish marker: %v", err)
	}
	if m := nextLedgerMessage(t, received); m.runID != ownerMoveMarker {
		t.Fatalf("message %+v arrived before the marker, want none", m)
	}
}

// expectOwnerMove asserts that the next message is the owner move of
// runID and returns it.
func expectOwnerMove(t *testing.T, received chan ledgerMessage, runID string) ledgerMessage {
	t.Helper()
	m := nextLedgerMessage(t, received)
	if m.subject != bus.LedgerMoveSubject(testCluster) || m.runID != runID {
		t.Fatalf("message = %+v, want the owner move of %s", m, runID)
	}
	return m
}

// poisonOwnerMoveCommit makes every transaction that moves a run intent to
// instance fail at its commit, through a deferred constraint trigger. The
// move statement itself succeeds, so the test proves that a rolled-back
// move publishes nothing.
func poisonOwnerMoveCommit(t *testing.T, db *sql.DB, instance string) {
	t.Helper()
	if _, err := db.Exec(`
		CREATE FUNCTION varroa.poison_owner_move() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'poisoned owner move';
		END $$
	`); err != nil {
		t.Fatalf("create poison function: %v", err)
	}
	if _, err := db.Exec(`
		CREATE CONSTRAINT TRIGGER poison_owner_move AFTER UPDATE ON varroa.run_intents
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
		WHEN (NEW.cell_instance = '` + instance + `')
		EXECUTE FUNCTION varroa.poison_owner_move()
	`); err != nil {
		t.Fatalf("create poison trigger: %v", err)
	}
}

// newOwnerMoveLedger returns a ledger that publishes owner moves on conn.
func newOwnerMoveLedger(t *testing.T, conn *bus.Conn) (*Ledger, *sql.DB) {
	t.Helper()
	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	ledger.SetOwnerMoves(conn, nil)
	return ledger, db
}

// TestClaimMoveSignalsTheOwnerMoveAfterCommit proves the claim path. A new
// intent moves no owner and sends nothing. A repeat claim from another
// instance moves the owner and sends one signal, after the commit: a
// ledger read on the signal finds the new owner.
func TestClaimMoveSignalsTheOwnerMoveAfterCommit(t *testing.T) {
	t.Parallel()

	conn := newTestActionConn(t)
	ledger, db := newOwnerMoveLedger(t, conn)
	received := collectLedgerMessages(t, conn, db)

	req := newTestClaimRequest("manual:x-move-signal", 1)
	first := mustGrant(t, ledger, req)
	expectNoLedgerMessage(t, conn, received)

	repeat := req
	repeat.Cell, repeat.CellInstance = "cell-b", "pod-b"
	if resp := ledger.Claim(context.Background(), repeat); resp.Decision != DecisionGranted || resp.RunID != first.RunID {
		t.Fatalf("want the repeat granted for %s, got %+v", first.RunID, resp)
	}
	if m := expectOwnerMove(t, received, first.RunID); !m.found || m.instance != "pod-b" {
		t.Fatalf("ledger read on the signal = %+v, want the new owner pod-b", m)
	}
	expectNoLedgerMessage(t, conn, received)
}

// TestClaimRepeatFromTheSameInstanceSignalsNothing proves that a repeat
// claim that keeps the owner sends no signal.
func TestClaimRepeatFromTheSameInstanceSignalsNothing(t *testing.T) {
	t.Parallel()

	conn := newTestActionConn(t)
	ledger, db := newOwnerMoveLedger(t, conn)
	received := collectLedgerMessages(t, conn, db)

	req := newTestClaimRequest("manual:x-same-signal", 1)
	mustGrant(t, ledger, req)
	mustGrant(t, ledger, req)
	expectNoLedgerMessage(t, conn, received)
}

// TestClaimTransferSignalsTheOldRun proves that a transfer to a new run id
// signals the old run, whose intent row is gone after the commit.
func TestClaimTransferSignalsTheOldRun(t *testing.T) {
	t.Parallel()

	conn := newTestActionConn(t)
	ledger, db := newOwnerMoveLedger(t, conn)
	received := collectLedgerMessages(t, conn, db)

	req := newTestClaimRequest("manual:x-transfer-signal", 1)
	first := mustGrant(t, ledger, req)
	expectNoLedgerMessage(t, conn, received)

	next := req
	next.RunID = newTestUUIDv7String()
	next.PlacementGeneration = 2
	next.Cell, next.CellInstance = "cell-b", "pod-b"
	if resp := ledger.Claim(context.Background(), next); resp.Decision != DecisionGranted || resp.RunID != next.RunID {
		t.Fatalf("want the transfer granted for %s, got %+v", next.RunID, resp)
	}
	if m := expectOwnerMove(t, received, first.RunID); m.found {
		t.Fatalf("ledger read on the signal = %+v, want no intent row for the old run", m)
	}
	expectNoLedgerMessage(t, conn, received)
}

// TestClaimMoveRolledBackSignalsNothing proves that a move whose commit
// fails sends no signal and keeps the old owner.
func TestClaimMoveRolledBackSignalsNothing(t *testing.T) {
	t.Parallel()

	conn := newTestActionConn(t)
	ledger, db := newOwnerMoveLedger(t, conn)
	received := collectLedgerMessages(t, conn, db)

	req := newTestClaimRequest("manual:x-rollback-signal", 1)
	mustGrant(t, ledger, req)
	poisonOwnerMoveCommit(t, db, "pod-poison")

	repeat := req
	repeat.CellInstance = "pod-poison"
	if resp := ledger.Claim(context.Background(), repeat); resp.Error == "" {
		t.Fatalf("want the commit to fail, got %+v", resp)
	}
	expectNoLedgerMessage(t, conn, received)
	if _, instance, _, _ := readIntentOwnerAndNumber(t, db, req.IntentKey); instance != "pod-a" {
		t.Fatalf("intent owner = %q, want pod-a kept", instance)
	}
}

// TestTakeOverSignalsTheOwnerMoveBeforeTheAction proves the mesh path. The
// dispatch sends one signal after its commit and before the resume
// action, so a gateway replica drops the old owner's match before the
// target can send.
func TestTakeOverSignalsTheOwnerMoveBeforeTheAction(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestActionConn(t)
	received := collectLedgerMessages(t, conn, db)

	runID := seedMeshRunningRun(t, pool, db)
	seedMeshFleetLock(t, db, runID)
	seedMeshPlacement(t, db, "pod-live", "pod-live")

	if got := runTakeover(t, pool, conn, nil, nil); got != 1 {
		t.Fatalf("published = %d, want 1", got)
	}
	if m := expectOwnerMove(t, received, runID.String()); !m.found || m.instance != "pod-live" {
		t.Fatalf("ledger read on the signal = %+v, want the new owner pod-live", m)
	}
	if m := nextLedgerMessage(t, received); m.kind != LedgerActionResumeRun || m.runID != runID.String() {
		t.Fatalf("message after the signal = %+v, want the resume_run action", m)
	}

	if got := runTakeover(t, pool, conn, nil, nil); got != 0 {
		t.Fatalf("second round published = %d, want 0", got)
	}
	expectNoLedgerMessage(t, conn, received)
}

// TestTakeOverRolledBackSignalsNothing proves that a dispatch whose commit
// fails sends neither the signal nor the action.
func TestTakeOverRolledBackSignalsNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestActionConn(t)
	received := collectLedgerMessages(t, conn, db)

	runID := seedMeshRunningRun(t, pool, db)
	seedMeshPlacement(t, db, "pod-live", "pod-live")
	poisonOwnerMoveCommit(t, db, "pod-live")

	if got := runTakeover(t, pool, conn, nil, nil); got != 0 {
		t.Fatalf("published = %d, want 0", got)
	}
	expectNoLedgerMessage(t, conn, received)
	var instance string
	if err := db.QueryRow(`SELECT cell_instance FROM varroa.run_intents WHERE run_id = $1`, runID).Scan(&instance); err != nil {
		t.Fatalf("read run_intents: %v", err)
	}
	if instance != meshDeadInstance {
		t.Fatalf("intent owner = %q, want the dead instance %q kept", instance, meshDeadInstance)
	}
}
