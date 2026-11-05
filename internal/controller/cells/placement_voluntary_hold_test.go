package cells

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// smallPoolCell is a cell of the pool "small" that still applies the
// tenant's folder.
func smallPoolCell(name string) CellCandidate {
	c := poolCell(name, "small", "payments")
	c.AppliedTenants = []string{"payments"}
	return c
}

// capPoolInputs builds the inputs of a move from pool "small" to pool
// "big". Pool "big" holds its maxCells cells, and each of them has one
// tenant slot, so none has a free slot.
func capPoolInputs(bound ...CellCandidate) Inputs {
	in := testInputs()
	in.Pools = []CellPoolSummary{
		{Name: "small", ProfileRef: "jenkins-lts", MaxTenantsPerCell: 10},
		{Name: "big", ProfileRef: "jenkins-lts", MaxTenantsPerCell: 1, CapReached: true},
	}
	in.Cells = append(append([]CellCandidate{}, bound...), poolCell("cell-z", "big", "other-tenant"))
	return in
}

// simulateDrainFilter applies the controller's drain filter to a cell: a
// cell drops the tenant's folder while TenantDrainsFromCell names it, and
// it applies the folder again otherwise.
func simulateDrainFilter(status *v1alpha1.TenantStatus, cell *CellCandidate) {
	if TenantDrainsFromCell(status, cell.Namespace, cell.Name) {
		cell.AppliedTenants = removeString(cell.AppliedTenants, "payments")
		return
	}
	cell.AppliedTenants = appendUnique(cell.AppliedTenants, "payments")
}

func TestPlace_VoluntaryMove_FullPoolHoldsAcrossTicks(t *testing.T) {
	in := capPoolInputs(smallPoolCell("cell-a"))
	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)
	tenant := testTenant()
	tenant.Spec.Pipelines.CellPool = "big"
	status := placedStatus(3)
	status.Conditions = []v1alpha1.TenantCondition{{
		Type: v1alpha1.TenantConditionPlaced, Status: metav1.ConditionTrue,
		Reason:             v1alpha1.TenantReasonTenantPlaced,
		LastTransitionTime: metav1.NewTime(fixedNow.Add(-time.Hour)),
	}}

	for tick := 0; tick < 3; tick++ {
		in.Now = fixedClock(fixedNow.Add(time.Duration(tick) * time.Minute))
		out, err := Place(context.Background(), in, tenant, status)
		if err != nil {
			t.Fatalf("tick %d: Place returned error: %v", tick, err)
		}
		assertMembersStale(t, status, `pool "big"`, "slot 0 cells-ns/cell-a",
			`the CellPool "big" holds its maxCells cells`)
		if c := condition(status); !c.LastTransitionTime.Time.Equal(fixedNow) {
			t.Fatalf("tick %d: LastTransitionTime = %v, want %v", tick, c.LastTransitionTime.Time, fixedNow)
		}
		if status.Placement.Cell != "cell-a" || status.Placement.Generation != 3 {
			t.Fatalf("tick %d: placement moved: %+v", tick, status.Placement)
		}
		if len(out.CellPatches) != 0 || len(out.CellClaims) != 0 {
			t.Fatalf("tick %d: a held move must patch and claim nothing: %+v %+v", tick, out.CellPatches, out.CellClaims)
		}
		simulateDrainFilter(status, &in.Cells[0])
		if !containsString(in.Cells[0].AppliedTenants, "payments") {
			t.Fatalf("tick %d: the old cell dropped the folder of a held move", tick)
		}
	}
	for _, req := range seen {
		if req.Cell != "cell-a" || req.Generation != 3 {
			t.Errorf("announce names %s gen %d, want cell-a gen 3", req.Cell, req.Generation)
		}
	}
}

func TestPlace_VoluntaryMove_FullPoolHoldsAMultiMemberTenant(t *testing.T) {
	in := capPoolInputs(smallPoolCell("cell-a"), smallPoolCell("cell-b"), smallPoolCell("cell-c"))
	tenant := slotTenant(2, 1)
	tenant.Spec.Pipelines.CellPool = "big"
	status := threeMemberStatus("cell-a")

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	assertMembersStale(t, status, "slot 0 cells-ns/cell-a", "slot 1 cells-ns/cell-b",
		"standby cells-ns/cell-c", `the CellPool "big" holds its maxCells cells`)
	for _, m := range status.Placement.Members {
		if !m.Stale {
			t.Errorf("member %s is not stale while the move holds", m.Cell)
		}
	}
	if status.Placement.Cell != "cell-a" || status.Placement.Generation != 3 {
		t.Fatalf("placement moved: %+v", status.Placement)
	}
}

func TestPlace_VoluntaryMove_DrainWithNoTargetHolds(t *testing.T) {
	// The old cell has dropped the folder, and no target exists.
	cellA := poolCell("cell-a", "small", "payments")
	in := capPoolInputs(cellA)
	tenant := testTenant()
	tenant.Spec.Pipelines.CellPool = "big"
	status := placedStatus(3)
	setMoving(status)

	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	assertMembersStale(t, status, `the CellPool "big" holds its maxCells cells`)
	if status.Placement.Cell != "cell-a" || status.Placement.Generation != 3 {
		t.Fatalf("placement moved: %+v", status.Placement)
	}
	if patchFor(out, "cell-a") != nil {
		t.Errorf("a held move must not patch the old cell: %+v", patchFor(out, "cell-a"))
	}
	if claimedCells(out)["cell-z"] {
		t.Error("a held move must not claim the full cell")
	}
}

func TestPlace_VoluntaryMove_HoldStartsWhenATargetAppears(t *testing.T) {
	in := capPoolInputs(smallPoolCell("cell-a"))
	tenant := testTenant()
	tenant.Spec.Pipelines.CellPool = "big"
	status := placedStatus(3)
	status.Conditions = []v1alpha1.TenantCondition{{
		Type: v1alpha1.TenantConditionPlaced, Status: metav1.ConditionTrue,
		Reason: v1alpha1.TenantReasonMembersStale, Message: "held",
	}}

	// Pool "big" gains a cell with a free slot.
	in.Pools[1].CapReached = false
	in.Cells = append(in.Cells, poolCell("cell-y", "big"))

	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	cond := condition(status)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.TenantReasonTenantMoving {
		t.Fatalf("expected False/TenantMoving once a target exists, got %+v", cond)
	}
	if status.Placement.Cell != "cell-a" || status.Placement.Generation != 3 {
		t.Fatalf("starting the drain must keep the placement: %+v", status.Placement)
	}
	if len(out.CellPatches) != 0 {
		t.Errorf("starting the drain must patch no cell: %+v", out.CellPatches)
	}
}

func TestPlace_VoluntaryMove_DrainIgnoresALostTarget(t *testing.T) {
	// The target cell reports CellReady=False, so no target exists. The
	// old cell still applies the folder, so the drain continues.
	target := poolCell("cell-b", "big")
	target.CellReady = false
	in := capPoolInputs(smallPoolCell("cell-a"), target)
	in.Pools[1].CapReached = false
	in.Now = fixedClock(fixedNow.Add(time.Minute))
	tenant := testTenant()
	tenant.Spec.Pipelines.CellPool = "big"
	status := placedStatus(3)
	setMoving(status)

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	cond := condition(status)
	if cond == nil || cond.Reason != v1alpha1.TenantReasonTenantMoving || cond.Message != tenantMovingMessage {
		t.Fatalf("a lost target must not end the drain: %+v", cond)
	}
	if !cond.LastTransitionTime.Time.Equal(fixedNow) {
		t.Errorf("LastTransitionTime = %v, want %v", cond.LastTransitionTime.Time, fixedNow)
	}
}

func TestPlace_VoluntaryMove_HoldReportsCellNotReady(t *testing.T) {
	cellA := smallPoolCell("cell-a")
	cellA.CellReady = false
	in := capPoolInputs(cellA)
	tenant := testTenant()
	tenant.Spec.Pipelines.CellPool = "big"
	status := placedStatus(3)

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	cond := condition(status)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.TenantReasonCellNotReady {
		t.Fatalf("expected False/CellNotReady, got %+v", cond)
	}
	if status.Placement.Cell != "cell-a" || status.Placement.Generation != 3 {
		t.Fatalf("placement moved: %+v", status.Placement)
	}
}

func TestPlace_VoluntaryMove_HoldAnnouncesAPodRoll(t *testing.T) {
	cellA := smallPoolCell("cell-a")
	cellA.Instance = "uid-new"
	in := capPoolInputs(cellA)
	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)
	in.LastSync = &SyncCache{
		Cell: "cell-a", CellNamespace: "cells-ns", Generation: 3,
		CellInstance: "uid-old", At: fixedNow,
	}
	tenant := testTenant()
	tenant.Spec.Pipelines.CellPool = "big"
	status := placedStatus(3)

	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("expected one announce for the pod roll, got %d", len(seen))
	}
	if seen[0].Cell != "cell-a" || seen[0].Generation != 3 || seen[0].CellInstance != "uid-new" {
		t.Errorf("announce = %+v, want cell-a gen 3 uid-new", seen[0])
	}
	if out.SyncCacheUpdate == nil {
		t.Error("a sent announce must return its SyncCache update")
	}
}

func TestPlace_VoluntaryMove_HoldAnnouncesTheBoundCellsPoolCap(t *testing.T) {
	cellA := smallPoolCell("cell-a")
	cellA.Instance = "uid-new"
	in := capPoolInputs(cellA)
	in.Pools[0].MaxRunsPerTenant = 4
	in.Pools[1].MaxRunsPerTenant = 9
	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)
	in.LastSync = &SyncCache{
		Cell: "cell-a", CellNamespace: "cells-ns", Generation: 3,
		CellInstance: "uid-old", At: fixedNow,
	}
	tenant := testTenant()
	tenant.Spec.Pipelines.CellPool = "big"
	status := placedStatus(3)

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("expected one announce for the pod roll, got %d", len(seen))
	}
	if got := seen[0].Policy.CellMaxRuns; got != 4 {
		t.Errorf("held announce carries cap %d, want the bound cell's pool cap 4", got)
	}
}

func TestStaleMembersMessageNamesThePoolCap(t *testing.T) {
	tenant := testTenant()
	tenant.Spec.Pipelines.CellPool = "big"
	zero := int32(0)
	members := []v1alpha1.TenantMemberStatus{{
		Cell: "cell-a", CellNamespace: "cells-ns",
		Role: v1alpha1.TenantMemberRoleActive, Slot: &zero, Stale: true,
	}}

	capped := Inputs{Pools: []CellPoolSummary{{Name: "big", CapReached: true}}}
	if got := staleMembersMessage(capped, tenant, members); !strings.Contains(got, `the CellPool "big" holds its maxCells cells`) {
		t.Errorf("message %q lacks the pool cap", got)
	}

	open := Inputs{Pools: []CellPoolSummary{{Name: "big"}}}
	if got := staleMembersMessage(open, tenant, members); strings.Contains(got, "maxCells") {
		t.Errorf("message %q names a cap the pool does not report", got)
	}
}

// applyDrainRule acts as each cell's apply does. A cell that lists the
// tenant drops its folder while TenantDrainsFromCell names that cell, and
// applies the folder again otherwise.
func applyDrainRule(status *v1alpha1.TenantStatus, in *Inputs) {
	for i := range in.Cells {
		c := &in.Cells[i]
		if !containsString(c.Tenants, "payments") {
			continue
		}
		if TenantDrainsFromCell(status, c.Namespace, c.Name) {
			c.AppliedTenants = removeString(c.AppliedTenants, "payments")
			continue
		}
		c.AppliedTenants = appendUnique(c.AppliedTenants, "payments")
	}
}

// relabeledSlotZeroInputs builds a tenant whose slot 0 cell cell-a moved to
// the pool "small". The tenant asks for pool "big", which holds its
// maxCells cells, and each cell of it holds one tenant.
func relabeledSlotZeroInputs(cells ...CellCandidate) Inputs {
	in := testInputs()
	in.Pools = []CellPoolSummary{
		{Name: "small", ProfileRef: "jenkins-lts", MaxTenantsPerCell: 10},
		{Name: "big", ProfileRef: "jenkins-lts", MaxTenantsPerCell: 1, CapReached: true},
	}
	in.Cells = cells
	return in
}

// pooledAppliedCell is appliedCell with the varroa.dev/cell-pool label.
func pooledAppliedCell(name, pool string, tenants ...string) CellCandidate {
	c := appliedCell(name, tenants...)
	c.Pool = pool
	return c
}

func TestPlace_VoluntaryMove_PromotesAFullStandbyWithoutALoop(t *testing.T) {
	slotZero := pooledAppliedCell("cell-a", "small", "payments")
	slotZero.PendingDeletionPaths = []string{"payments/api"}
	in := relabeledSlotZeroInputs(slotZero,
		pooledAppliedCell("cell-b", "big", "payments"), pooledAppliedCell("cell-c", "big", "payments"))
	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)
	tenant := slotTenant(2, 1)
	tenant.Spec.Pipelines.CellPool = "big"
	status := threeMemberStatus("cell-a")
	status.Conditions = []v1alpha1.TenantCondition{{
		Type: v1alpha1.TenantConditionPlaced, Status: metav1.ConditionTrue,
		Reason:             v1alpha1.TenantReasonMembersApplied,
		LastTransitionTime: metav1.NewTime(fixedNow.Add(-time.Hour)),
	}}

	// A build on cell-a holds the drain for three ticks.
	for tick := 0; tick < 3; tick++ {
		in.Now = fixedClock(fixedNow.Add(time.Duration(tick) * time.Minute))
		out, err := Place(context.Background(), in, tenant, status)
		if err != nil {
			t.Fatalf("tick %d: Place returned error: %v", tick, err)
		}
		cond := condition(status)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.TenantReasonTenantMoving {
			t.Fatalf("tick %d: want False/TenantMoving, got %+v", tick, cond)
		}
		if !cond.LastTransitionTime.Time.Equal(fixedNow) {
			t.Errorf("tick %d: LastTransitionTime = %v, want %v", tick, cond.LastTransitionTime.Time, fixedNow)
		}
		if status.Placement.Cell != "cell-a" || status.Placement.Generation != 3 {
			t.Fatalf("tick %d: placement moved during the drain: %+v", tick, status.Placement)
		}
		if len(out.CellPatches) != 0 {
			t.Errorf("tick %d: a drain tick must patch no cell: %+v", tick, out.CellPatches)
		}
		applyDrainRule(status, &in)
		if containsString(in.Cells[0].AppliedTenants, "payments") {
			t.Errorf("tick %d: slot 0's old cell kept the folder", tick)
		}
		for _, kept := range in.Cells[1:] {
			if !containsString(kept.AppliedTenants, "payments") {
				t.Errorf("tick %d: %s dropped the folder of a current member", tick, kept.Name)
			}
		}
	}

	// The build ends, so cell-a has drained.
	in.Cells[0].PendingDeletionPaths = nil
	in.Now = fixedClock(fixedNow.Add(3 * time.Minute))
	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("final tick: Place returned error: %v", err)
	}
	if status.Placement.Cell != "cell-c" || status.Placement.Generation != 4 {
		t.Fatalf("want slot 0 on the standby cell-c at gen 4, got %+v", status.Placement)
	}
	if m := storedMemberOf(status, "cell-c"); m.Slot == nil || *m.Slot != 0 {
		t.Errorf("cell-c should hold slot 0, got %+v", m)
	}
	if m := storedMemberOf(status, "cell-b"); m.Slot == nil || *m.Slot != 1 {
		t.Errorf("cell-b should keep slot 1, got %+v", m)
	}
	if !dropsTenant(patchFor(out, "cell-a")) {
		t.Errorf("the old slot 0 cell should drop the tenant, got %+v", out.CellPatches)
	}
	if p := patchFor(out, "cell-c"); p != nil && len(p.Tenants) != 1 {
		t.Errorf("the promotion must add no tenant to the full cell-c: %+v", p)
	}
	if cond := condition(status); cond == nil || cond.Reason != v1alpha1.TenantReasonStandbyPending {
		t.Errorf("want StandbyPending while the full pool has no standby cell, got %+v", cond)
	}
	if len(seen) == 0 || seen[len(seen)-1].Cell != "cell-c" || seen[len(seen)-1].Generation != 4 {
		t.Errorf("want a last announce for cell-c at gen 4, got %+v", seen)
	}
}

func TestPlace_VoluntaryMove_KeepsACurrentMemberBuilding(t *testing.T) {
	in := testInputs()
	in.Pools = []CellPoolSummary{
		{Name: "small", ProfileRef: "jenkins-lts", MaxTenantsPerCell: 10},
		{Name: "big", ProfileRef: "jenkins-lts", MaxTenantsPerCell: 10},
	}
	in.Cells = []CellCandidate{
		pooledAppliedCell("cell-a", "small", "payments"),
		pooledAppliedCell("cell-b", "big", "payments"),
		poolCell("cell-d", "big"),
	}
	in.Cells[0].PendingDeletionPaths = []string{"payments/api"}
	tenant := slotTenant(2, 0)
	tenant.Spec.Pipelines.CellPool = "big"
	status := placedWithMembers([]v1alpha1.TenantMemberStatus{
		memberStatus("cell-a", v1alpha1.TenantMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TenantMemberRoleActive, intPtr(1)),
	})

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("tick 1: Place returned error: %v", err)
	}
	if cond := condition(status); cond == nil || cond.Reason != v1alpha1.TenantReasonTenantMoving {
		t.Fatalf("tick 1: want TenantMoving, got %+v", cond)
	}
	applyDrainRule(status, &in)
	if !containsString(in.Cells[1].AppliedTenants, "payments") {
		t.Errorf("slot 1's current cell-b dropped the folder during the move")
	}

	in.Cells[0].PendingDeletionPaths = nil
	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("tick 2: Place returned error: %v", err)
	}
	if status.Placement.Cell != "cell-d" || status.Placement.Generation != 4 {
		t.Fatalf("want slot 0 on cell-d at gen 4, got %+v", status.Placement)
	}
	if m := storedMemberOf(status, "cell-b"); m.Slot == nil || *m.Slot != 1 {
		t.Errorf("cell-b should keep slot 1 after the move, got %+v", m)
	}
	if patchFor(out, "cell-b") != nil {
		t.Errorf("the move must not patch slot 1's cell, got %+v", patchFor(out, "cell-b"))
	}
}
