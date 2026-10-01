package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// insertReleasingLock inserts one releasing lock of testTenant named
// fleetLockStateMachineTestName, owned by cellInstance.
func insertReleasingLock(t *testing.T, db *sql.DB, runID, cellInstance string) {
	t.Helper()
	now := time.Now().UTC()
	insertLockRowForTenant(t, db, fleetLockStateMachineTestName, runID, testTenant, "acme/release",
		FleetLockStateReleasing, now.Add(-10*time.Minute), now.Add(5*time.Minute))
	if _, err := db.Exec(`UPDATE varroa.fleet_locks SET cell_instance = $1 WHERE name = $2 AND run_id = $3`,
		cellInstance, fleetLockStateMachineTestName, runID); err != nil {
		t.Fatalf("set cell_instance: %v", err)
	}
}

// TestTenantSyncAnnounceAnswersTheUnconfirmedEntries asserts the reply
// lists every stored entry that names a cell and carries no confirmation.
// The operator reads those pods again, so the stored row is the durable
// list.
func TestTenantSyncAnnounceAnswersTheUnconfirmedEntries(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	conn := newTestSyncConn(t)
	now := time.Now().UTC()
	confirmed := now.Add(-time.Minute)

	sendTenantSync(t, pool, conn, announceMembers(1, []member{
		activeMember("cell-a", 0, "pod-a1"), activeMember("cell-b", 1, "pod-b1"),
	}))
	dropped := announceMembers(2, []member{activeMember("cell-a", 0, "pod-a1")})
	dropped.DeadInstances = []TenantSyncDeadInstance{
		{Instance: "pod-c1", LastSeenAt: now, Cell: "cell-c", Namespace: "cells"},
		{Instance: "pod-d1", LastSeenAt: now, Cell: "cell-d", Namespace: "cells", ConfirmedGoneAt: &confirmed},
		{Instance: "pod-e1", LastSeenAt: now},
	}
	resp := sendTenantSync(t, pool, conn, dropped)
	if !resp.Ok {
		t.Fatalf("announce: %+v", resp)
	}

	got := map[string]TenantSyncDeadInstance{}
	for _, d := range resp.Unconfirmed {
		got[d.Instance] = d
	}
	if len(got) != 2 {
		t.Fatalf("unconfirmed = %+v, want pod-b1 and pod-c1", resp.Unconfirmed)
	}
	if b := got["pod-b1"]; b.Cell != "cell-b" || b.Namespace != "cells" || b.Derived != DeadInstanceDerivedDropped {
		t.Fatalf("the derived entry = %+v, want a dropped entry on cells/cell-b", b)
	}
	if c := got["pod-c1"]; c.Cell != "cell-c" || c.Namespace != "cells" {
		t.Fatalf("the operator entry = %+v, want cells/cell-c", c)
	}
}

// TestTenantSyncAnnounceConfirmationOfADroppedEntry asserts a confirmation
// with no cell clears the dropped tag, keeps the stored cell, and leaves
// the reply's unconfirmed list.
func TestTenantSyncAnnounceConfirmationOfADroppedEntry(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, announceMembers(1, []member{
		activeMember("cell-a", 0, "pod-a1"), activeMember("cell-b", 1, "pod-b1"),
	}))
	sendTenantSync(t, pool, conn, announceMembers(2, []member{activeMember("cell-a", 0, "pod-a1")}))

	confirmed := time.Now().UTC()
	req := announceMembers(2, []member{activeMember("cell-a", 0, "pod-a1")})
	req.DeadInstances = []TenantSyncDeadInstance{{Instance: "pod-b1", LastSeenAt: confirmed, ConfirmedGoneAt: &confirmed}}
	resp := sendTenantSync(t, pool, conn, req)
	if !resp.Ok || len(resp.Unconfirmed) != 0 {
		t.Fatalf("reply = %+v, want ok with no unconfirmed entry", resp)
	}
	got, ok := derivedEntry(t, db, "pod-b1")
	if !ok || got.ConfirmedGoneAt == nil || got.Derived != "" || got.Cell != "cell-b" || got.Namespace != "cells" {
		t.Fatalf("stored entry = %+v (found %v), want a confirmed untagged entry on cells/cell-b", got, ok)
	}
}

// TestDeadInstancesTrimKeepsAFleetLockInstance asserts the trim keeps the
// entry of a held or releasing lock's owning instance. The run of a
// releasing lock is INTERRUPTED, so the run arms of the keep set miss it.
func TestDeadInstancesTrimKeepsAFleetLockInstance(t *testing.T) {
	for _, state := range []string{FleetLockStateReleasing, FleetLockStateHeld} {
		t.Run(state, func(t *testing.T) {
			pool, db := setupLedgerPool(t)
			conn := newTestSyncConn(t)
			base := time.Now().UTC()
			insertLockRowForTenant(t, db, fleetLockStateMachineTestName, "run-1", testTenant, "acme/release",
				state, base.Add(-10*time.Minute), base.Add(5*time.Minute))
			if _, err := db.Exec(`UPDATE varroa.fleet_locks SET cell_instance = 'pod-locked' WHERE run_id = 'run-1'`); err != nil {
				t.Fatalf("set cell_instance: %v", err)
			}

			sendTenantSync(t, pool, conn, TenantSyncRequest{
				Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
				Generation: 1, CellInstance: "pod-current",
				DeadInstances: []TenantSyncDeadInstance{{Instance: "pod-locked", LastSeenAt: base.Add(-time.Hour)}},
			})
			entries := make([]TenantSyncDeadInstance, 0, 12)
			for i := 0; i < 12; i++ {
				entries = append(entries, TenantSyncDeadInstance{
					Instance: "pod-extra-" + string(rune('a'+i)), LastSeenAt: base.Add(-time.Duration(i) * time.Minute),
				})
			}
			sendTenantSync(t, pool, conn, TenantSyncRequest{
				Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
				Generation: 1, CellInstance: "pod-current", DeadInstances: entries,
			})

			if _, ok := derivedEntry(t, db, "pod-locked"); !ok {
				t.Fatalf("the trim dropped the lock's owning instance: %+v", readTenantPlacement(t, db, testTenant).deadInstances)
			}
		})
	}
}

// TestReleaseConfirmedFleetLocksRound asserts the backstop round's release
// check reads the stored placement row. A confirmation past the grace
// releases the lock with no new announce; anything less releases nothing.
func TestReleaseConfirmedFleetLocksRound(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-time.Minute)
	young := now.Add(-time.Second)
	cases := []struct {
		name      string
		dead      []TenantSyncDeadInstance
		noRow     bool
		wantFreed bool
	}{
		{name: "a confirmation past the grace releases",
			dead: []TenantSyncDeadInstance{{Instance: "pod-a", LastSeenAt: old, ConfirmedGoneAt: &old}}, wantFreed: true},
		{name: "a confirmation inside the grace keeps the lock",
			dead: []TenantSyncDeadInstance{{Instance: "pod-a", LastSeenAt: old, ConfirmedGoneAt: &young}}},
		{name: "an unconfirmed entry keeps the lock",
			dead: []TenantSyncDeadInstance{{Instance: "pod-a", LastSeenAt: now.Add(-time.Hour), Derived: DeadInstanceDerivedDropped}}},
		{name: "a tenant with no placement row keeps the lock", noRow: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, db := setupLedgerPool(t)
			insertReleasingLock(t, db, "run-1", "pod-a")
			if !tc.noRow {
				seedPlacementLivenessWithPolicy(t, db, "pod-b", tc.dead, `{}`)
			}
			notifier, sent, _ := newCaptureNotifier(t)
			defer notifier.Close()

			released, err := ReleaseConfirmedFleetLocksRound(context.Background(), pool, now, testGrace, notifier, nil)
			if err != nil {
				t.Fatalf("ReleaseConfirmedFleetLocksRound: %v", err)
			}
			_, _, found := fleetLockRowState(t, db, "run-1")
			if tc.wantFreed {
				if released != 1 || found || sent.count() != 1 {
					t.Fatalf("released = %d, row found = %v, notifications = %d; want 1, false, 1",
						released, found, sent.count())
				}
				if got := sent.payload(0); got.Kind != "lock" || got.State != FleetLockStateReleased || got.RunID != "run-1" {
					t.Fatalf("notification = %+v, want a released lock for run-1", got)
				}
				return
			}
			if released != 0 || !found || sent.count() != 0 {
				t.Fatalf("released = %d, row found = %v, notifications = %d; want 0, true, 0",
					released, found, sent.count())
			}
		})
	}
}

// TestRunBackstopRoundReleasesAConfirmedFleetLock asserts the once-a-minute
// round runs the release check.
func TestRunBackstopRoundReleasesAConfirmedFleetLock(t *testing.T) {
	pool, db := setupLedgerPool(t)
	now := time.Now().UTC()
	confirmed := now.Add(-time.Minute)
	insertReleasingLock(t, db, "run-1", "pod-a")
	seedPlacementLivenessWithPolicy(t, db, "pod-b",
		[]TenantSyncDeadInstance{{Instance: "pod-a", LastSeenAt: confirmed, ConfirmedGoneAt: &confirmed}}, `{}`)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	if _, err := RunBackstopRound(context.Background(), pool, conn, BackstopConfig{Now: func() time.Time { return now }}); err != nil {
		t.Fatalf("RunBackstopRound: %v", err)
	}
	if _, _, found := fleetLockRowState(t, db, "run-1"); found {
		t.Fatal("the backstop round kept a lock whose owner is confirmed gone")
	}
}
