package cells

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestFencedCellsCoversEveryMember asserts a drain deletes no member's
// cell: the fence set names every member, active or standby, and not the
// scalar cell alone.
func TestFencedCellsCoversEveryMember(t *testing.T) {
	slot0, slot1 := int32(0), int32(1)
	placement := &v1alpha1.TenantPlacementStatus{
		Cell: "cell-a", CellNamespace: "cells-ns",
		Members: []v1alpha1.TenantMemberStatus{
			{Cell: "cell-a", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive, Slot: &slot0},
			{Cell: "cell-b", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive, Slot: &slot1},
			{Cell: "cell-c", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleStandby},
		},
	}

	fenced := map[string]bool{}
	for _, m := range placement.Members {
		fenced[cellKey(m.CellNamespace, m.Cell)] = true
	}
	fenced[cellKey(placement.CellNamespace, placement.Cell)] = true

	for _, name := range []string{"cell-a", "cell-b", "cell-c"} {
		if !fenced[cellKey("cells-ns", name)] {
			t.Fatalf("%s is not fenced: %v", name, fenced)
		}
	}
}

// TestUnmetMemberDemandCountsEverySlotAndStandby asserts a tenant's demand
// on its pool is the members it still lacks. Counting it as one unit would
// stop a pool with no warm cell from creating one once slot 0 binds, and
// every other slot would stay pending for good.
func TestUnmetMemberDemandCountsEverySlotAndStandby(t *testing.T) {
	tenant := slotTenant(2, 1)
	owned := map[string]bool{"cell-a": true, "cell-b": true, "cell-c": true}

	unplaced := &v1alpha1.Tenant{Spec: tenant.Spec}
	if got := unmetMemberDemand(unplaced, owned); got != 3 {
		t.Fatalf("an unplaced tenant asks for %d cells, want 3", got)
	}

	slot0 := int32(0)
	tenant.Status.Placement = &v1alpha1.TenantPlacementStatus{
		Cell: "cell-a", CellNamespace: "cells-ns",
		Members: []v1alpha1.TenantMemberStatus{
			{Cell: "cell-a", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive, Slot: &slot0},
		},
	}
	if got := unmetMemberDemand(tenant, owned); got != 2 {
		t.Fatalf("a tenant holding slot 0 asks for %d more cells, want 2", got)
	}

	slot1 := int32(1)
	tenant.Status.Placement.Members = append(tenant.Status.Placement.Members,
		v1alpha1.TenantMemberStatus{Cell: "cell-b", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive, Slot: &slot1},
		v1alpha1.TenantMemberStatus{Cell: "cell-c", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleStandby})
	if got := unmetMemberDemand(tenant, owned); got != 0 {
		t.Fatalf("a fully placed tenant asks for %d more cells, want 0", got)
	}
}

// TestUnmetMemberDemandReadsTheScalarForAnEmptyList asserts a tenant the
// pass has not written a member list for counts one bound member.
func TestUnmetMemberDemandReadsTheScalarForAnEmptyList(t *testing.T) {
	tenant := slotTenant(1, 0)
	tenant.Status.Placement = &v1alpha1.TenantPlacementStatus{Cell: "cell-a", CellNamespace: "cells-ns"}

	if got := unmetMemberDemand(tenant, map[string]bool{"cell-a": true}); got != 0 {
		t.Fatalf("a placed single-member tenant asks for %d cells, want 0", got)
	}
	if got := unmetMemberDemand(tenant, map[string]bool{"cell-other": true}); got != 1 {
		t.Fatalf("a tenant on an unowned cell asks for %d cells, want 1", got)
	}
}

// TestUnmetMemberDemandIgnoresACellAnotherPoolOwns asserts a member on a
// cell this pool does not own leaves its demand on this pool unmet.
func TestUnmetMemberDemandIgnoresACellAnotherPoolOwns(t *testing.T) {
	tenant := slotTenant(2, 0)
	slot0 := int32(0)
	tenant.Status.Placement = &v1alpha1.TenantPlacementStatus{
		Cell: "cell-elsewhere", CellNamespace: "other-ns",
		Members: []v1alpha1.TenantMemberStatus{
			{Cell: "cell-elsewhere", CellNamespace: "other-ns", Role: v1alpha1.TenantMemberRoleActive, Slot: &slot0},
		},
	}
	if got := unmetMemberDemand(tenant, map[string]bool{"cell-a": true}); got != 2 {
		t.Fatalf("a tenant on another pool's cell asks for %d cells, want 2", got)
	}
}

// poolFillPool is the pool desiredFreeCount sizes: warm 0, ten tenants to
// a cell.
func poolFillPool() *v1alpha1.CellPool {
	return &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "jenkins-lts"},
		Spec: v1alpha1.CellPoolSpec{
			ProfileRef:        testProfileName,
			Warm:              0,
			MaxTenantsPerCell: 10,
		},
	}
}

// filledCell is one owned cell that already holds the named tenants, and
// therefore has spare tenant capacity but is no free cell of its own.
func filledCell(name string, tenants ...string) *v1alpha1.Controller {
	return &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testOperatorNamespace},
		Status:     v1alpha1.ControllerStatus{Cell: &v1alpha1.CellStatus{Tenants: tenants}},
	}
}

// memberOn builds one member status on the named cell. A nil slot is a
// standby.
func memberOn(cell string, slot *int32) v1alpha1.TenantMemberStatus {
	role := v1alpha1.TenantMemberRoleActive
	if slot == nil {
		role = v1alpha1.TenantMemberRoleStandby
	}
	return v1alpha1.TenantMemberStatus{
		Cell: cell, CellNamespace: testOperatorNamespace, Role: role, Slot: slot,
	}
}

// TestPoolCreatesACellWhenEveryCellHoldsAMember asserts the pool creates a
// cell for a tenant whose unmet demand no existing cell can take.
//
// A cell holds at most one member of a tenant. Offsetting the demand with
// the spare capacity of the tenant's own member cells left a vacated
// standby unreplaced for good: the pool read the demand as met while no
// cell could take it.
func TestPoolCreatesACellWhenEveryCellHoldsAMember(t *testing.T) {
	r := &WarmPoolReconciler{Config: WarmPoolConfig{MaxTenantsPerCell: 10}}
	tenant := slotTenant(2, 1)
	tenant.Status.Placement = &v1alpha1.TenantPlacementStatus{
		Cell: "cell-0", CellNamespace: testOperatorNamespace,
		Members: []v1alpha1.TenantMemberStatus{
			memberOn("cell-0", intPtr(0)),
			memberOn("cell-1", intPtr(1)),
		},
	}
	owned := []*v1alpha1.Controller{
		filledCell("cell-0", tenant.Name, "other"),
		filledCell("cell-1", tenant.Name),
	}

	got := r.desiredFreeCount(poolFillPool(), owned, []*v1alpha1.Tenant{tenant}, 10)
	if got != 1 {
		t.Fatalf("desired free cells = %d, want 1 for the unreplaced standby", got)
	}
}

// TestPoolCreatesNoCellWhenOneCellCanTakeTheMember asserts a cell that
// holds no member of the tenant still offsets that tenant's demand.
func TestPoolCreatesNoCellWhenOneCellCanTakeTheMember(t *testing.T) {
	r := &WarmPoolReconciler{Config: WarmPoolConfig{MaxTenantsPerCell: 10}}
	tenant := slotTenant(2, 1)
	tenant.Status.Placement = &v1alpha1.TenantPlacementStatus{
		Cell: "cell-0", CellNamespace: testOperatorNamespace,
		Members: []v1alpha1.TenantMemberStatus{
			memberOn("cell-0", intPtr(0)),
			memberOn("cell-1", intPtr(1)),
		},
	}
	owned := []*v1alpha1.Controller{
		filledCell("cell-0", tenant.Name),
		filledCell("cell-1", tenant.Name),
		filledCell("cell-2", "other"),
	}

	got := r.desiredFreeCount(poolFillPool(), owned, []*v1alpha1.Tenant{tenant}, 10)
	if got != 0 {
		t.Fatalf("desired free cells = %d, want 0 while a cell can take the standby", got)
	}
}

// TestPoolFillKeepsTheSingleMemberArithmetic asserts a tenant that asks for
// one member is sized exactly as before: a placed tenant asks for nothing,
// and an unplaced one is offset by a cell with spare capacity.
func TestPoolFillKeepsTheSingleMemberArithmetic(t *testing.T) {
	r := &WarmPoolReconciler{Config: WarmPoolConfig{MaxTenantsPerCell: 10}}
	placed := slotTenant(1, 0)
	placed.Status.Placement = &v1alpha1.TenantPlacementStatus{
		Cell: "cell-0", CellNamespace: testOperatorNamespace,
	}
	owned := []*v1alpha1.Controller{filledCell("cell-0", placed.Name)}
	if got := r.desiredFreeCount(poolFillPool(), owned, []*v1alpha1.Tenant{placed}, 10); got != 0 {
		t.Fatalf("desired free cells = %d, want 0 for a placed single-member tenant", got)
	}

	unplaced := slotTenant(1, 0)
	unplaced.Name = "unplaced"
	spare := []*v1alpha1.Controller{filledCell("cell-0", "other")}
	if got := r.desiredFreeCount(poolFillPool(), spare, []*v1alpha1.Tenant{unplaced}, 10); got != 0 {
		t.Fatalf("desired free cells = %d, want 0 while a cell has spare capacity", got)
	}
}

// TestPoolFillGivesOneCellOneMemberOfEachTenant asserts one cell with spare
// capacity offsets one member of each tenant, and never two members of one
// tenant.
func TestPoolFillGivesOneCellOneMemberOfEachTenant(t *testing.T) {
	r := &WarmPoolReconciler{Config: WarmPoolConfig{MaxTenantsPerCell: 10}}
	first := slotTenant(2, 0)
	first.Status.Placement = &v1alpha1.TenantPlacementStatus{
		Cell: "cell-0", CellNamespace: testOperatorNamespace,
		Members: []v1alpha1.TenantMemberStatus{memberOn("cell-0", intPtr(0))},
	}
	second := slotTenant(1, 0)
	second.Name = "billing"
	owned := []*v1alpha1.Controller{filledCell("cell-0", first.Name)}

	// first lacks slot 1, which cell-0 cannot take because it holds slot 0
	// already. second lacks its only member, which cell-0 can take.
	got := r.desiredFreeCount(poolFillPool(), owned, []*v1alpha1.Tenant{first, second}, 10)
	if got != 1 {
		t.Fatalf("desired free cells = %d, want 1", got)
	}
}
