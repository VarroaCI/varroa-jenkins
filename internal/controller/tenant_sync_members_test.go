package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestNewNATSSyncAnnouncer_CarriesTheMemberList asserts the member list
// crosses the wire whole. The recorder stores what the request carries, so
// a dropped list reads as an empty one, and every claim then resolves the
// synthetic slot-0 member.
func TestNewNATSSyncAnnouncer_CarriesTheMemberList(t *testing.T) {
	conn := startTestSyncBus(t)

	var got recordstore.TenantSyncRequest
	sub, err := conn.SubscribeRequest(bus.TenantSyncSubject, "recorder", func(data []byte) []byte {
		_ = json.Unmarshal(data, &got)
		resp, _ := json.Marshal(recordstore.TenantSyncResponse{Ok: true})
		return resp
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	slot0, slot1 := int32(0), int32(1)
	members := []cells.AnnounceMember{
		{Cell: "cell-0", Namespace: "cells-ns", Role: "active", Slot: &slot0,
			Instance: "uid-0", RehydratedInstance: "uid-0", PendingJobs: []string{"payments/api"}},
		{Cell: "cell-2", Namespace: "cells-ns", Role: "active", Slot: &slot1,
			Instance: "uid-2", RehydratedInstance: "uid-2"},
		{Cell: "cell-1", Namespace: "cells-ns", Role: "standby", Instance: "uid-1"},
	}

	announcer := NewNATSSyncAnnouncer(conn)
	if _, err := announcer(context.Background(), cells.AnnounceRequest{
		Cluster: "core", Tenant: "payments", Namespace: "cells-ns", Cell: "cell-0",
		Generation: 2, CellInstance: "uid-0", Members: members,
	}); err != nil {
		t.Fatalf("announce: %v", err)
	}

	if len(got.Members) != len(members) {
		t.Fatalf("the wire carried %d members, want %d: %+v", len(got.Members), len(members), got.Members)
	}
	for i, want := range members {
		have := got.Members[i]
		if have.Cell != want.Cell || have.Namespace != want.Namespace || have.Role != want.Role {
			t.Errorf("member %d identity = %+v, want %+v", i, have, want)
		}
		if have.Instance != want.Instance || have.RehydratedInstance != want.RehydratedInstance {
			t.Errorf("member %d instances = %+v, want %+v", i, have, want)
		}
		if (have.Slot == nil) != (want.Slot == nil) {
			t.Errorf("member %d slot presence = %v, want %v", i, have.Slot, want.Slot)
			continue
		}
		if have.Slot != nil && *have.Slot != *want.Slot {
			t.Errorf("member %d slot = %d, want %d", i, *have.Slot, *want.Slot)
		}
	}
	if len(got.Members[0].PendingJobs) != 1 || got.Members[0].PendingJobs[0] != "payments/api" {
		t.Errorf("slot 0 pending jobs = %v", got.Members[0].PendingJobs)
	}
	if got.Members[2].Slot != nil {
		t.Errorf("the standby carries the slot %v, want none", got.Members[2].Slot)
	}
}

// TestNewNATSSyncAnnouncer_CarriesNoMemberForASingleTenantController
// asserts a controller with no cell role announces no member. The recorder
// resolves its row to one synthetic slot-0 member by design.
func TestNewNATSSyncAnnouncer_CarriesNoMemberForASingleTenantController(t *testing.T) {
	conn := startTestSyncBus(t)

	var got recordstore.TenantSyncRequest
	sub, err := conn.SubscribeRequest(bus.TenantSyncSubject, "recorder", func(data []byte) []byte {
		_ = json.Unmarshal(data, &got)
		resp, _ := json.Marshal(recordstore.TenantSyncResponse{Ok: true})
		return resp
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	announcer := NewNATSSyncAnnouncer(conn)
	if _, err := announcer(context.Background(), cells.AnnounceRequest{
		Cluster: "core", Tenant: "cells-ns/solo", Namespace: "cells-ns", Cell: "solo",
		CellInstance: "uid-solo",
	}); err != nil {
		t.Fatalf("announce: %v", err)
	}
	if len(got.Members) != 0 {
		t.Fatalf("a controller with no cell role announced %d members", len(got.Members))
	}
}

// TestMembersToWireRoundTripsThroughTheRecorderDecoder asserts the wire
// shape the announcer sends decodes back to the same member list, so the
// recorder stores what the operator decided.
func TestMembersToWireRoundTripsThroughTheRecorderDecoder(t *testing.T) {
	slot1 := int32(1)
	wire := membersToWire([]cells.AnnounceMember{
		{Cell: "cell-2", Namespace: "cells-ns", Role: "active", Slot: &slot1,
			Instance: "uid-2", RehydratedInstance: "uid-2", PendingJobs: []string{"payments/api"}},
		{Cell: "cell-1", Namespace: "cells-ns", Role: "standby", Instance: "uid-1"},
	})

	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back []recordstore.TenantSyncMember
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back) != 2 {
		t.Fatalf("decoded %d members, want 2", len(back))
	}
	if back[0].Slot == nil || *back[0].Slot != 1 || back[0].Cell != "cell-2" {
		t.Fatalf("the active member decoded as %+v", back[0])
	}
	if back[0].RehydratedInstance != "uid-2" || len(back[0].PendingJobs) != 1 {
		t.Fatalf("the active member lost its seed state: %+v", back[0])
	}
	if back[1].Slot != nil || back[1].Role != "standby" {
		t.Fatalf("the standby decoded as %+v", back[1])
	}
}

// TestNewNATSSyncAnnouncer_CarriesAMemberConfirmation asserts a replaced
// member's confirmation crosses the wire. Without the stamp the recorder's
// confirmed reading never holds for that member, so no run of it is armed
// for a resume and no unstarted intent of it is re-fired.
func TestNewNATSSyncAnnouncer_CarriesAMemberConfirmation(t *testing.T) {
	conn := startTestSyncBus(t)

	var got recordstore.TenantSyncRequest
	sub, err := conn.SubscribeRequest(bus.TenantSyncSubject, "recorder", func(data []byte) []byte {
		_ = json.Unmarshal(data, &got)
		resp, _ := json.Marshal(recordstore.TenantSyncResponse{Ok: true})
		return resp
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	confirmedAt := time.Date(2026, 9, 17, 11, 41, 20, 0, time.UTC)
	lastSeen := confirmedAt.Add(-2 * time.Minute)
	slot1 := int32(1)

	announcer := NewNATSSyncAnnouncer(conn)
	if _, err := announcer(context.Background(), cells.AnnounceRequest{
		Cluster: "core", Tenant: "smoke-b", Namespace: "cells-ns", Cell: "cell-0",
		Generation: 3, CellInstance: "uid-cell-0",
		Members: []cells.AnnounceMember{
			{Cell: "cell-2", Namespace: "cells-ns", Role: "active", Slot: &slot1, Instance: "fa69db7a"},
		},
		DeadInstances: []cells.DeadInstance{
			{Instance: "2918db60", LastSeenAt: lastSeen, ConfirmedGoneAt: &confirmedAt},
		},
	}); err != nil {
		t.Fatalf("announce: %v", err)
	}

	if len(got.DeadInstances) != 1 {
		t.Fatalf("the wire carried %d dead instances, want 1", len(got.DeadInstances))
	}
	entry := got.DeadInstances[0]
	if entry.Instance != "2918db60" || !entry.LastSeenAt.Equal(lastSeen) {
		t.Fatalf("the dead instance = %+v", entry)
	}
	if entry.ConfirmedGoneAt == nil || !entry.ConfirmedGoneAt.Equal(confirmedAt) {
		t.Fatalf("the confirmation did not cross the wire: %+v", entry.ConfirmedGoneAt)
	}
	if len(got.Members) != 1 || got.Members[0].Instance != "fa69db7a" {
		t.Fatalf("the member list = %+v", got.Members)
	}
}
