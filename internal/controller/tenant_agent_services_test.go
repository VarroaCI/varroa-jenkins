package controller

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// slotMember builds one active member entry.
func slotMember(cell string, slot int32) v1alpha1.TenantMemberStatus {
	s := slot
	return v1alpha1.TenantMemberStatus{
		Cell: cell, CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive, Slot: &s, Applied: true,
	}
}

// standbyMember builds one standby member entry.
func standbyMember(cell string) v1alpha1.TenantMemberStatus {
	return v1alpha1.TenantMemberStatus{
		Cell: cell, CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleStandby, Applied: true,
	}
}

// TestActiveSlotHoldersNamesOneCellForEachSlot asserts every active slot
// answers its own holder, and that a standby answers none.
func TestActiveSlotHoldersNamesOneCellForEachSlot(t *testing.T) {
	p := &v1alpha1.TenantPlacementStatus{
		Cell: "cell-a", CellNamespace: "cells-ns",
		Members: []v1alpha1.TenantMemberStatus{
			slotMember("cell-a", 0), slotMember("cell-b", 1), standbyMember("cell-c"),
		},
	}
	holders := activeSlotHolders(p)
	if len(holders) != 2 {
		t.Fatalf("holders = %+v, want two active slots", holders)
	}
	if holders[0].cell != "cell-a" || holders[1].cell != "cell-b" {
		t.Fatalf("holders = %+v, want slot 0 on cell-a and slot 1 on cell-b", holders)
	}
	for _, h := range holders {
		if h.cell == "cell-c" {
			t.Fatal("a standby holds a Service of its own")
		}
	}
}

// TestActiveSlotHoldersReadsTheScalarForAnEmptyList asserts a tenant the
// pass has not written a member list for still gets slot 0's Service.
func TestActiveSlotHoldersReadsTheScalarForAnEmptyList(t *testing.T) {
	p := &v1alpha1.TenantPlacementStatus{Cell: "cell-a", CellNamespace: "cells-ns"}
	holders := activeSlotHolders(p)
	if len(holders) != 1 || holders[0].cell != "cell-a" {
		t.Fatalf("holders = %+v, want slot 0 on cell-a", holders)
	}
}

// TestActiveSlotHoldersFlipsOnAPromotion asserts a promotion moves a
// slot's holder, so the reconciler patches that slot's Service selector
// onto the promoted cell and creates no second Service.
func TestActiveSlotHoldersFlipsOnAPromotion(t *testing.T) {
	before := &v1alpha1.TenantPlacementStatus{
		Cell: "cell-a", CellNamespace: "cells-ns",
		Members: []v1alpha1.TenantMemberStatus{
			slotMember("cell-a", 0), slotMember("cell-b", 1), standbyMember("cell-c"),
		},
	}
	after := &v1alpha1.TenantPlacementStatus{
		Cell: "cell-a", CellNamespace: "cells-ns",
		Members: []v1alpha1.TenantMemberStatus{
			slotMember("cell-a", 0), slotMember("cell-c", 1),
		},
	}

	holders := activeSlotHolders(after)
	if holders[1].cell != "cell-c" {
		t.Fatalf("slot 1 holder = %q, want the promoted cell-c", holders[1].cell)
	}
	if len(removedSlots(before, holders)) != 0 {
		t.Fatalf("a promotion removed a slot: %+v", removedSlots(before, holders))
	}
}

// TestRemovedSlotsNamesOnlyTheReleasedSlots asserts a scale-down from two
// slots to one names slot 1 alone, so a steady tenant costs no delete
// call at all.
func TestRemovedSlotsNamesOnlyTheReleasedSlots(t *testing.T) {
	before := &v1alpha1.TenantPlacementStatus{
		Cell: "cell-a", CellNamespace: "cells-ns",
		Members: []v1alpha1.TenantMemberStatus{slotMember("cell-a", 0), slotMember("cell-b", 1)},
	}
	after := &v1alpha1.TenantPlacementStatus{
		Cell: "cell-a", CellNamespace: "cells-ns",
		Members: []v1alpha1.TenantMemberStatus{slotMember("cell-a", 0)},
	}

	removed := removedSlots(before, activeSlotHolders(after))
	if len(removed) != 1 {
		t.Fatalf("removed = %+v, want slot 1 alone", removed)
	}
	if removed[1].cell != "cell-b" {
		t.Fatalf("removed slot 1 = %+v, want cell-b", removed[1])
	}
	if len(removedSlots(before, activeSlotHolders(before))) != 0 {
		t.Fatal("a steady tenant reports a removed slot")
	}
}

// TestTenantSlotOnCellNamesTheSlotTheCellHolds asserts the tenant cloud
// dials the agent Service of the slot that cell holds.
func TestTenantSlotOnCellNamesTheSlotTheCellHolds(t *testing.T) {
	tenant := &v1alpha1.Tenant{}
	tenant.Name = "payments"
	tenant.Status.Placement = &v1alpha1.TenantPlacementStatus{
		Cell: "cell-a", CellNamespace: "cells-ns",
		Members: []v1alpha1.TenantMemberStatus{
			slotMember("cell-a", 0), slotMember("cell-b", 1), standbyMember("cell-c"),
		},
	}

	slotOne := &v1alpha1.Controller{}
	slotOne.Name, slotOne.Namespace = "cell-b", "cells-ns"
	if got := tenantSlotOnCell(tenant, slotOne); got != 1 {
		t.Fatalf("slot = %d, want 1", got)
	}

	// A standby renders the slot-0 URL, so a promotion into a slot finds a
	// cloud already in place.
	standby := &v1alpha1.Controller{}
	standby.Name, standby.Namespace = "cell-c", "cells-ns"
	if got := tenantSlotOnCell(tenant, standby); got != 0 {
		t.Fatalf("a standby's slot = %d, want 0", got)
	}
	if got := tenantRoleOnCell(tenant, standby); got != v1alpha1.TenantMemberRoleStandby {
		t.Fatalf("a standby's role = %q, want standby", got)
	}
	if got := tenantRoleOnCell(tenant, slotOne); got != v1alpha1.TenantMemberRoleActive {
		t.Fatalf("slot 1's role = %q, want active", got)
	}
}

// TestTenantSlotOnCellReadsSlotZeroForAnEmptyList asserts a tenant with no
// member list renders the slot-0 URL, which is today's behaviour.
func TestTenantSlotOnCellReadsSlotZeroForAnEmptyList(t *testing.T) {
	tenant := &v1alpha1.Tenant{}
	tenant.Name = "payments"
	cr := &v1alpha1.Controller{}
	cr.Name, cr.Namespace = "cell-a", "cells-ns"

	if got := tenantSlotOnCell(tenant, cr); got != 0 {
		t.Fatalf("slot = %d, want 0", got)
	}
	if got := tenantAgentEndpoint("payments", "cells-ns", 0); got !=
		"http://varroa-agents-payments-0.cells-ns.svc.cluster.local:8080" {
		t.Fatalf("endpoint = %q", got)
	}
	if got := tenantAgentEndpoint("payments", "cells-ns", 1); got !=
		"http://varroa-agents-payments-1.cells-ns.svc.cluster.local:8080" {
		t.Fatalf("slot 1 endpoint = %q", got)
	}
}
