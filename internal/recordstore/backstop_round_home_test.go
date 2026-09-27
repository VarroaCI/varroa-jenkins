package recordstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// collectBackstopActions subscribes to the ledger action subject and
// answers a function that waits for n actions.
func collectBackstopActions(t *testing.T, conn *bus.Conn) func(n int) []LedgerAction {
	t.Helper()
	received := make(chan LedgerAction, 8)
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
	return func(n int) []LedgerAction {
		t.Helper()
		var out []LedgerAction
		for len(out) < n {
			select {
			case a := <-received:
				out = append(out, a)
			case <-time.After(3 * time.Second):
				t.Fatalf("want %d actions, got %+v", n, out)
			}
		}
		select {
		case extra := <-received:
			t.Fatalf("an extra action was published: %+v", extra)
		case <-time.After(300 * time.Millisecond):
		}
		return out
	}
}

// TestRunBackstopSchedule_FiresOnTheHomeSlot asserts the wake_cell and
// the retry_run of a backstop fire both name the member of the job's
// home slot, not the cell the stored schedule row names.
func TestRunBackstopSchedule_FiresOnTheHomeSlot(t *testing.T) {
	pool, db := setupLedgerPool(t)
	jobPath := testTenant + "/home-fire"
	// The row is stored from slot 0's cell before the home moves.
	ingestCronSchedule(t, NewIngester(pool, nil, nil), testTenant, jobPath, 1, time.Now().UTC().Add(-10*time.Minute))
	seedGrantPlacement(t, db, testCluster, testTenant, jsTestNS, "cell-a", 1, "{}")
	setPlacementMembers(t, db, testTenant,
		[]member{activeMember("cell-a", 0, "i0"), activeMember("cell-b1", 1, "i1")},
		map[string]int32{jobPath: 1})

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	wait := collectBackstopActions(t, conn)

	if err := runBackstopSchedule(context.Background(), pool, conn, BackstopConfig{}, time.Now()); err != nil {
		t.Fatalf("runBackstopSchedule: %v", err)
	}

	actions := wait(2)
	if actions[0].Kind != LedgerActionWakeCell || actions[1].Kind != LedgerActionRetryRun {
		t.Fatalf("kinds = [%s %s], want [wake_cell retry_run]", actions[0].Kind, actions[1].Kind)
	}
	for _, a := range actions {
		if a.Namespace != "cells" || a.Controller != "cell-b1" {
			t.Fatalf("%s address = %s/%s, want cells/cell-b1", a.Kind, a.Namespace, a.Controller)
		}
	}
}

// TestRunBackstopSchedule_UnplacedControllerKeepsTheRowCell asserts a
// tenant with no placement row fires on the cell its schedule row names.
func TestRunBackstopSchedule_UnplacedControllerKeepsTheRowCell(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	jobPath := testTenant + "/unplaced-fire"
	ingestCronSchedule(t, NewIngester(pool, nil, nil), testTenant, jobPath, 0, time.Now().UTC().Add(-10*time.Minute))

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	wait := collectBackstopActions(t, conn)

	if err := runBackstopSchedule(context.Background(), pool, conn, BackstopConfig{}, time.Now()); err != nil {
		t.Fatalf("runBackstopSchedule: %v", err)
	}

	for _, a := range wait(2) {
		if a.Namespace != jsTestNS || a.Controller != "cell-a" {
			t.Fatalf("%s address = %s/%s, want %s/cell-a", a.Kind, a.Namespace, a.Controller, jsTestNS)
		}
	}
}

// TestRunBackstopSchedule_UnboundHomeSlotFiresNothing asserts a home
// slot that holds no member fires nothing and writes no activity row, so
// a later round can fire the occurrence.
func TestRunBackstopSchedule_UnboundHomeSlotFiresNothing(t *testing.T) {
	pool, db := setupLedgerPool(t)
	jobPath := testTenant + "/unbound-fire"
	ingestCronSchedule(t, NewIngester(pool, nil, nil), testTenant, jobPath, 1, time.Now().UTC().Add(-10*time.Minute))
	seedGrantPlacement(t, db, testCluster, testTenant, jsTestNS, "cell-a", 1, "{}")
	setPlacementMembers(t, db, testTenant,
		[]member{activeMember("cell-a", 0, "i0")}, map[string]int32{jobPath: 1})

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	wait := collectBackstopActions(t, conn)

	if err := runBackstopSchedule(context.Background(), pool, conn, BackstopConfig{}, time.Now()); err != nil {
		t.Fatalf("runBackstopSchedule: %v", err)
	}

	wait(0)
	if n := countBackstopActivity(t, db, jobPath); n != 0 {
		t.Fatalf("backstop activity rows = %d, want 0", n)
	}
}
