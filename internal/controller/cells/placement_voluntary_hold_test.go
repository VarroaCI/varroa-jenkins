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
// cell drops the tenant's folder while the tenant is TenantMoving, and it
// applies the folder again otherwise.
func simulateDrainFilter(status *v1alpha1.TenantStatus, cell *CellCandidate) {
	if c := condition(status); c != nil && c.Reason == v1alpha1.TenantReasonTenantMoving {
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
