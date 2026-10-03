package recordstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// forceReleaseRequest names the one releasing lock insertReleasingLock writes.
func forceReleaseRequest() FleetLockForceReleaseRequest {
	return FleetLockForceReleaseRequest{Name: fleetLockStateMachineTestName, RunID: "run-1",
		Actor: "ops@example.com", Reason: "the old pod is gone"}
}

// TestForceReleaseFleetLock asserts each outcome. Only a releasing row whose
// cell instance the placement names as no member is deleted, and only that
// delete publishes a notification.
func TestForceReleaseFleetLock(t *testing.T) {
	cases := []struct {
		name        string
		insert      bool
		state       string
		placementOn string
		want        string
	}{
		{name: "a releasing row with no live holder is released", insert: true, state: FleetLockStateReleasing,
			placementOn: "pod-b", want: FleetLockForceReleased},
		{name: "a releasing row with no placement row is released", insert: true, state: FleetLockStateReleasing,
			want: FleetLockForceReleased},
		{name: "a missing row answers not_found", want: FleetLockForceNotFound},
		{name: "a held row answers not_releasing", insert: true, state: FleetLockStateHeld,
			placementOn: "pod-b", want: FleetLockForceNotReleasing},
		{name: "a waiting row answers not_releasing", insert: true, state: FleetLockStateWaiting,
			placementOn: "pod-b", want: FleetLockForceNotReleasing},
		{name: "a holder the placement still runs answers holder_live", insert: true, state: FleetLockStateReleasing,
			placementOn: "pod-a", want: FleetLockForceHolderLive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, db := setupLedgerPool(t)
			now := time.Now().UTC()
			if tc.insert {
				insertLockRowForTenant(t, db, fleetLockStateMachineTestName, "run-1", testTenant, "acme/release",
					tc.state, now.Add(-10*time.Minute), now.Add(5*time.Minute))
				if _, err := db.Exec(`UPDATE varroa.fleet_locks SET cell_instance = 'pod-a' WHERE run_id = 'run-1'`); err != nil {
					t.Fatalf("set cell_instance: %v", err)
				}
			}
			if tc.placementOn != "" {
				seedPlacementLivenessWithPolicy(t, db, tc.placementOn, nil, `{}`)
			}
			notifier, sent, _ := newCaptureNotifier(t)
			defer notifier.Close()

			resp, err := ForceReleaseFleetLock(context.Background(), pool, forceReleaseRequest(), notifier)
			if err != nil {
				t.Fatalf("ForceReleaseFleetLock: %v", err)
			}
			if resp.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q (%+v)", resp.Outcome, tc.want, resp)
			}
			_, _, found := fleetLockRowState(t, db, "run-1")
			if tc.want == FleetLockForceReleased {
				if found || sent.count() != 1 {
					t.Fatalf("row found = %v, notifications = %d; want false, 1", found, sent.count())
				}
				got := sent.payload(0)
				if got.Kind != "lock" || got.State != FleetLockStateReleased || got.RunID != "run-1" ||
					got.Tenant != testTenant || got.JobPath != "acme/release" {
					t.Fatalf("notification = %+v", got)
				}
				if resp.Tenant != testTenant || resp.Cluster != testCluster || resp.CellInstance != "pod-a" {
					t.Fatalf("reply = %+v, want the row's owner", resp)
				}
				return
			}
			if sent.count() != 0 {
				t.Fatalf("a refusal published %d notifications", sent.count())
			}
			if tc.want == FleetLockForceHolderLive && resp.HolderCell != "cell-a" {
				t.Fatalf("holder_live reply = %+v, want the holder cell cell-a", resp)
			}
			if tc.want != FleetLockForceHolderLive && (resp.HolderCell != "" || resp.HolderNamespace != "") {
				t.Fatalf("reply = %+v, want no holder cell outside holder_live", resp)
			}
			if tc.insert && !found {
				t.Fatal("a refusal deleted the row")
			}
		})
	}
}

// TestForceReleaseFleetLock_HolderLiveNamesTheMemberCell asserts that a
// holder_live reply names the cell of the member that runs the row's cell
// instance, also when that member is not slot 0.
func TestForceReleaseFleetLock_HolderLiveNamesTheMemberCell(t *testing.T) {
	pool, db := setupLedgerPool(t)
	insertReleasingLock(t, db, "run-2", "pod-b1")
	members, err := json.Marshal([]member{
		{Cell: "cell-a", Namespace: cellUsageNamespace, Role: MemberRoleActive, Slot: slotPtr(0), Instance: "pod-a0"},
		{Cell: "cell-b", Namespace: cellUsageNamespace, Role: MemberRoleActive, Slot: slotPtr(1), Instance: "pod-b1"},
	})
	if err != nil {
		t.Fatalf("marshal members: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements (cluster, tenant, namespace, cell, cell_instance, cell_alive, members)
		VALUES ($1, $2, $3, 'cell-a', 'pod-a0', true, $4::jsonb)
	`, testCluster, testTenant, cellUsageNamespace, string(members)); err != nil {
		t.Fatalf("seed member placement: %v", err)
	}

	req := forceReleaseRequest()
	req.RunID = "run-2"
	resp, err := ForceReleaseFleetLock(context.Background(), pool, req, nil)
	if err != nil {
		t.Fatalf("ForceReleaseFleetLock: %v", err)
	}
	if resp.Outcome != FleetLockForceHolderLive || resp.HolderCell != "cell-b" ||
		resp.HolderNamespace != cellUsageNamespace || resp.CellInstance != "pod-b1" {
		t.Fatalf("reply = %+v, want holder_live on %s/cell-b for pod-b1", resp, cellUsageNamespace)
	}
}

// TestForceReleaseFleetLock_RestartedHolderCellReleasesTheLock asserts the
// path a holder_live refusal names. A restart of the holder cell gives a
// new pod, the operator confirms the old instance gone, and the backstop
// round then releases the lock with no force release.
func TestForceReleaseFleetLock_RestartedHolderCellReleasesTheLock(t *testing.T) {
	pool, db := setupLedgerPool(t)
	now := time.Now().UTC()
	insertReleasingLock(t, db, "run-1", "pod-a")
	seedPlacementLivenessWithPolicy(t, db, "pod-a", nil, `{}`)

	resp, err := ForceReleaseFleetLock(context.Background(), pool, forceReleaseRequest(), nil)
	if err != nil {
		t.Fatalf("ForceReleaseFleetLock: %v", err)
	}
	if resp.Outcome != FleetLockForceHolderLive || resp.HolderCell != "cell-a" {
		t.Fatalf("reply before the restart = %+v, want holder_live on cell-a", resp)
	}

	confirmed := now.Add(-time.Minute)
	dead, err := json.Marshal([]TenantSyncDeadInstance{{Instance: "pod-a", LastSeenAt: confirmed, ConfirmedGoneAt: &confirmed}})
	if err != nil {
		t.Fatalf("marshal dead instances: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET cell_instance = 'pod-a2', cell_alive_at = $3, dead_instances = $4::jsonb
		WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant, now, string(dead)); err != nil {
		t.Fatalf("store the restarted placement: %v", err)
	}

	released, err := ReleaseConfirmedFleetLocksRound(context.Background(), pool, now, testGrace, nil, nil)
	if err != nil {
		t.Fatalf("ReleaseConfirmedFleetLocksRound: %v", err)
	}
	if _, _, found := fleetLockRowState(t, db, "run-1"); released != 1 || found {
		t.Fatalf("released = %d, row found = %v; want 1, false", released, found)
	}
}

// TestFleetLockReleaseResponderRoundTrip asserts the responder answers over
// the bus, and refuses a request with no reason before any write.
func TestFleetLockReleaseResponderRoundTrip(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)
	insertReleasingLock(t, db, "run-1", "pod-a")

	sub, err := RegisterFleetLockReleaseResponder(conn, pool, nil, nil)
	if err != nil {
		t.Fatalf("RegisterFleetLockReleaseResponder: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	send := func(req FleetLockForceReleaseRequest) FleetLockForceReleaseResponse {
		t.Helper()
		data, _ := json.Marshal(req)
		msg, err := conn.Request(bus.FleetLockReleaseSubject, data, 5*time.Second)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		var resp FleetLockForceReleaseResponse
		if err := json.Unmarshal(msg.Data, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}

	noReason := forceReleaseRequest()
	noReason.Reason = ""
	if resp := send(noReason); resp.Error == "" {
		t.Fatalf("a request with no reason = %+v, want an error", resp)
	}
	if _, _, found := fleetLockRowState(t, db, "run-1"); !found {
		t.Fatal("a refused request deleted the row")
	}
	if resp := send(forceReleaseRequest()); resp.Outcome != FleetLockForceReleased {
		t.Fatalf("reply = %+v, want released", resp)
	}
	if resp := send(forceReleaseRequest()); resp.Outcome != FleetLockForceNotFound {
		t.Fatalf("a repeat = %+v, want not_found", resp)
	}
}
