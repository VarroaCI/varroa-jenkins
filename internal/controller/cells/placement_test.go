package cells

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/tenancy"
)

var fixedNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func testTeam() *v1alpha1.Team {
	return &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"payments-ns"},
			Pipelines: &v1alpha1.TeamPipelinesSpec{
				Profile:        "jenkins-lts",
				AgentNamespace: "payments-ns",
			},
		},
	}
}

func succeedingPurge() PurgeFunc {
	return func(context.Context, PurgeRequest) (bool, error) { return true, nil }
}

func failingPurge() PurgeFunc {
	return func(context.Context, PurgeRequest) (bool, error) { return false, nil }
}

func succeedingAnnounce() Announcer {
	return func(context.Context, AnnounceRequest) (AnnounceResult, error) { return AnnounceResult{OK: true}, nil }
}

func failingAnnounce() Announcer {
	return func(context.Context, AnnounceRequest) (AnnounceResult, error) { return AnnounceResult{}, nil }
}

func testInputs() Inputs {
	return Inputs{
		Now:                fixedClock(fixedNow),
		RecordStoreEnabled: true,
		BlobsModeS3:        true,
		ProfileExists:      func(string) bool { return true },
		ManagedNamespaces:  tenancy.NewManagedSet("", ""),
		MaxTenantsDefault:  10,
		ClusterIdentity:    "core",
		RequestPurge:       succeedingPurge(),
		Announce:           succeedingAnnounce(),
	}
}

func readyCell(name string, tenants ...string) CellCandidate {
	return CellCandidate{
		Name:            name,
		Namespace:       "cells-ns",
		Enabled:         true,
		CellReady:       true,
		ResolvedProfile: "jenkins-lts",
		Tenants:         tenants,
	}
}

// poolCell is readyCell plus the varroa.dev/cell-pool label a pool stamps.
func poolCell(name, pool string, tenants ...string) CellCandidate {
	c := readyCell(name, tenants...)
	c.Pool = pool
	return c
}

func placedStatus(generation int64) *v1alpha1.TeamStatus {
	return &v1alpha1.TeamStatus{
		Placement: &v1alpha1.TeamPlacementStatus{
			Cluster:       "core",
			Cell:          "cell-a",
			CellNamespace: "cells-ns",
			Generation:    generation,
		},
		Pipelines: &v1alpha1.TeamPipelinesStatus{WebhookToken: "tok", WebhookSecret: "sec"},
	}
}

func condition(status *v1alpha1.TeamStatus) *v1alpha1.TeamCondition {
	for i := range status.Conditions {
		if status.Conditions[i].Type == v1alpha1.TeamConditionPlaced {
			return &status.Conditions[i]
		}
	}
	return nil
}

// --- refusal gates (task 6.2) ---

func TestPlace_RefusalGates(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*Inputs, *v1alpha1.Team)
		wantReason string
	}{
		{"recordStoreDisabled", func(in *Inputs, _ *v1alpha1.Team) { in.RecordStoreEnabled = false }, v1alpha1.TeamReasonRecordStoreUnsupported},
		{"blobTierNotS3", func(in *Inputs, _ *v1alpha1.Team) { in.BlobsModeS3 = false }, v1alpha1.TeamReasonBlobTierUnsupported},
		{"profileMissing", func(in *Inputs, _ *v1alpha1.Team) { in.ProfileExists = func(string) bool { return false } }, v1alpha1.TeamReasonProfileNotFound},
		{"agentNamespaceNotInSpec", func(_ *Inputs, team *v1alpha1.Team) { team.Spec.Pipelines.AgentNamespace = "other-ns" }, v1alpha1.TeamReasonAgentNamespaceUnmanaged},
		{"agentNamespaceUnmanaged", func(in *Inputs, _ *v1alpha1.Team) {
			in.ManagedNamespaces = tenancy.NewManagedSet("some-other-ns", "op-ns")
		}, v1alpha1.TeamReasonAgentNamespaceUnmanaged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := testInputs()
			team := testTeam()
			tc.mutate(&in, team)
			status := &v1alpha1.TeamStatus{}
			out, err := Place(context.Background(), in, team, status)
			if err != nil {
				t.Fatalf("Place returned error: %v", err)
			}
			cond := condition(status)
			if cond == nil {
				t.Fatal("expected a Placed condition")
			}
			if cond.Status != metav1.ConditionFalse || cond.Reason != tc.wantReason {
				t.Errorf("got status=%s reason=%s, want False/%s", cond.Status, cond.Reason, tc.wantReason)
			}
			if status.Placement != nil {
				t.Error("a refusal must leave placement nil")
			}
			if len(out.CellPatches) != 0 {
				t.Error("a refusal must patch no cell")
			}
		})
	}
}

func TestPlace_Refusal_LeavesExistingPlacementUntouched(t *testing.T) {
	in := testInputs()
	in.Cells = []CellCandidate{readyCell("cell-a")}
	in.RecordStoreEnabled = false
	team := testTeam()
	status := placedStatus(1)

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement == nil || status.Placement.Cell != "cell-a" || status.Placement.Generation != 1 {
		t.Errorf("existing placement changed: %+v", status.Placement)
	}
	if len(out.CellPatches) != 0 {
		t.Error("a refusal must patch no cell")
	}
}

// --- keep rule (task 6.3) ---

func TestPlace_Keep_TransientNotReadyDoesNotMove(t *testing.T) {
	in := testInputs()
	team := testTeam()
	notReady := readyCell("cell-a", team.Name)
	notReady.CellReady = false
	in.Cells = []CellCandidate{notReady}
	status := placedStatus(3)

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement.Generation != 3 || status.Placement.Cell != "cell-a" {
		t.Errorf("a transient CellReady=False must not move a tenant: %+v", status.Placement)
	}
	if len(out.CellPatches) != 0 {
		t.Error("a transient CellReady=False must not patch a cell")
	}
	cond := condition(status)
	if cond == nil || cond.Reason != v1alpha1.TeamReasonCellNotReady {
		t.Errorf("expected reason CellNotReady, got %+v", cond)
	}
}

// TestPlace_Keep_MissingTenantEntryPatchesAndClaims asserts that a kept
// placement whose cell does not yet list the tenant sends the tenant patch
// again and names the claim, so a refused patch is retried every tick.
func TestPlace_Keep_MissingTenantEntryPatchesAndClaims(t *testing.T) {
	in := testInputs()
	in.Cells = []CellCandidate{readyCell("cell-a")}
	team := testTeam()
	status := placedStatus(3)

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement.Generation != 3 || status.Placement.Cell != "cell-a" {
		t.Errorf("a missing tenant entry must not move a tenant: %+v", status.Placement)
	}
	if len(out.CellClaims) != 1 || out.CellClaims[0].Name != "cell-a" || out.CellClaims[0].Tenant != team.Name {
		t.Fatalf("expected a claim on cell-a for %s, got %+v", team.Name, out.CellClaims)
	}
	if len(out.CellPatches) != 1 || out.CellPatches[0].Name != "cell-a" {
		t.Fatalf("expected one tenant patch on cell-a, got %+v", out.CellPatches)
	}
	if !containsString(out.CellPatches[0].Tenants, team.Name) {
		t.Errorf("the tenant patch must add %s: %+v", team.Name, out.CellPatches[0].Tenants)
	}

	// A cell that already lists the tenant needs no patch and no claim.
	in.Cells = []CellCandidate{readyCell("cell-a", team.Name)}
	out, err = Place(context.Background(), in, team, placedStatus(3))
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if len(out.CellClaims) != 0 || len(out.CellPatches) != 0 {
		t.Errorf("a listed tenant must patch nothing: claims=%+v patches=%+v", out.CellClaims, out.CellPatches)
	}
}

func TestPlace_Keep_EvacuatingCellMoves(t *testing.T) {
	in := testInputs()
	evacuating := readyCell("cell-a")
	evacuating.Evacuate = true
	fresh := readyCell("cell-b")
	in.Cells = []CellCandidate{evacuating, fresh}
	team := testTeam()
	status := placedStatus(3)

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement.Generation != 4 || status.Placement.Cell != "cell-b" {
		t.Errorf("an evacuating cell must move the tenant: %+v", status.Placement)
	}
	if len(out.CellPatches) != 2 {
		t.Fatalf("expected two cell patches (old and new), got %d", len(out.CellPatches))
	}
}

// --- selection (task 6.4) ---

// TestPlace_Selection_SingleTenantPool covers the restated scenario "A
// dedicated tenant never shares a cell": a pool whose maxTenantsPerCell is 1
// admits no second tenant, because capacity is a property of the cell.
func TestPlace_Selection_SingleTenantPool(t *testing.T) {
	in := testInputs()
	in.Pools = []CellPoolSummary{{Name: "solo", ProfileRef: "jenkins-lts", MaxTenantsPerCell: 1}}
	occupied := poolCell("cell-a", "solo", "other-team")
	empty := poolCell("cell-b", "solo")
	in.Cells = []CellCandidate{occupied, empty}
	team := testTeam()
	team.Spec.Pipelines.CellPool = "solo"
	status := &v1alpha1.TeamStatus{}

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement == nil || status.Placement.Cell != "cell-b" {
		t.Fatalf("a single-tenant pool must pick the empty cell, got %+v", status.Placement)
	}
	if len(out.CellPatches) != 1 || out.CellPatches[0].Name != "cell-b" {
		t.Errorf("unexpected cell patches: %+v", out.CellPatches)
	}
}

// TestPlace_Selection_CellOfAnotherPoolNeverChosen covers "A cell of another
// pool is never chosen".
func TestPlace_Selection_CellOfAnotherPoolNeverChosen(t *testing.T) {
	in := testInputs()
	in.Pools = []CellPoolSummary{
		{Name: "small", ProfileRef: "jenkins-lts", MaxTenantsPerCell: 1},
		{Name: "big", ProfileRef: "jenkins-lts", MaxTenantsPerCell: 10},
	}
	in.Cells = []CellCandidate{poolCell("cell-a", "big")}
	team := testTeam()
	team.Spec.Pipelines.CellPool = "small"
	status := &v1alpha1.TeamStatus{}

	_, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement != nil {
		t.Fatalf("a cell of another pool must never be chosen, got %+v", status.Placement)
	}
	cond := condition(status)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected Placed=False, got %+v", cond)
	}
}

// TestPlace_Selection_UnownedCellServesUnpooledTenant covers "An unowned cell
// serves a tenant that names no pool".
func TestPlace_Selection_UnownedCellServesUnpooledTenant(t *testing.T) {
	in := testInputs()
	in.Pools = []CellPoolSummary{{Name: "jenkins-lts", ProfileRef: "jenkins-lts", MaxTenantsPerCell: 10}}
	in.Cells = []CellCandidate{readyCell("cell-hand")}
	team := testTeam()
	status := &v1alpha1.TeamStatus{}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement == nil || status.Placement.Cell != "cell-hand" {
		t.Fatalf("an unowned cell must serve a tenant that names no pool, got %+v", status.Placement)
	}
}

// TestPlace_Refusal_CellPoolNotFound covers "A named pool that does not exist
// is refused", including the rule that a refusal leaves an existing
// placement untouched.
func TestPlace_Refusal_CellPoolNotFound(t *testing.T) {
	in := testInputs()
	in.Cells = []CellCandidate{readyCell("cell-a", "payments")}
	team := testTeam()
	team.Spec.Pipelines.CellPool = "nope"
	status := placedStatus(3)

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	cond := condition(status)
	if cond == nil || cond.Reason != v1alpha1.TeamReasonCellPoolNotFound {
		t.Fatalf("expected CellPoolNotFound, got %+v", cond)
	}
	if status.Placement == nil || status.Placement.Generation != 3 {
		t.Fatalf("a refusal must leave the placement untouched, got %+v", status.Placement)
	}
}

// TestPlace_Refusal_CellPoolServesAnotherProfile covers "A pool serving
// another profile is refused": the message names the profile the pool
// actually serves.
func TestPlace_Refusal_CellPoolServesAnotherProfile(t *testing.T) {
	in := testInputs()
	in.Pools = []CellPoolSummary{{Name: "big", ProfileRef: "jenkins-lts-old", MaxTenantsPerCell: 10}}
	team := testTeam()
	team.Spec.Pipelines.CellPool = "big"
	status := &v1alpha1.TeamStatus{}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	cond := condition(status)
	if cond == nil || cond.Reason != v1alpha1.TeamReasonCellPoolNotFound {
		t.Fatalf("expected CellPoolNotFound, got %+v", cond)
	}
	if !strings.Contains(cond.Message, "jenkins-lts-old") {
		t.Errorf("message must name the profile the pool serves, got %q", cond.Message)
	}
}

// TestPlace_AwaitingCell_DefaultPoolNotCreatedYet asserts a default pool the
// operator has not created yet is not a refusal: the pool reconciler creates
// it within one tick, so the tenant reports AwaitingCell.
func TestPlace_AwaitingCell_DefaultPoolNotCreatedYet(t *testing.T) {
	in := testInputs()
	team := testTeam()
	status := &v1alpha1.TeamStatus{}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	cond := condition(status)
	if cond == nil || cond.Reason != v1alpha1.TeamReasonAwaitingCell {
		t.Fatalf("expected AwaitingCell, got %+v", cond)
	}
}

// TestPlace_CellCapReached covers "A capped pool reports the cap".
func TestPlace_CellCapReached(t *testing.T) {
	in := testInputs()
	in.Pools = []CellPoolSummary{{
		Name: "jenkins-lts", ProfileRef: "jenkins-lts", MaxTenantsPerCell: 1, CapReached: true,
	}}
	in.Cells = []CellCandidate{poolCell("cell-a", "jenkins-lts", "other-team")}
	team := testTeam()
	status := &v1alpha1.TeamStatus{}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	cond := condition(status)
	if cond == nil || cond.Reason != v1alpha1.TeamReasonCellCapReached {
		t.Fatalf("expected CellCapReached, got %+v", cond)
	}
	if status.Placement != nil {
		t.Errorf("a capped pool must place nothing, got %+v", status.Placement)
	}
}

// TestPlace_DeletedPoolKeepsPlacement covers "Deleting a pool does not move
// its tenants": the bound cell loses its effective owner and the keep rule
// still holds.
func TestPlace_DeletedPoolKeepsPlacement(t *testing.T) {
	in := testInputs()
	bound := poolCell("cell-a", "gone", "payments")
	in.Cells = []CellCandidate{bound}
	team := testTeam()
	status := placedStatus(3)

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement == nil || status.Placement.Generation != 3 {
		t.Fatalf("a deleted pool must move no tenant, got %+v", status.Placement)
	}
	cond := condition(status)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected the placement kept, got %+v", cond)
	}
}

// TestPlace_ChangingCellPoolIsVoluntary covers "Moving a tenant to another
// pool is a voluntary move": the pass waits for the drain signal.
func TestPlace_ChangingCellPoolIsVoluntary(t *testing.T) {
	in := testInputs()
	in.Pools = []CellPoolSummary{
		{Name: "small", ProfileRef: "jenkins-lts", MaxTenantsPerCell: 1},
		{Name: "big", ProfileRef: "jenkins-lts", MaxTenantsPerCell: 10},
	}
	bound := poolCell("cell-a", "small", "payments")
	bound.AppliedTenants = []string{"payments"}
	in.Cells = []CellCandidate{bound, poolCell("cell-b", "big")}
	team := testTeam()
	team.Spec.Pipelines.CellPool = "big"
	status := placedStatus(3)

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	cond := condition(status)
	if cond == nil || cond.Reason != v1alpha1.TeamReasonTenantMoving {
		t.Fatalf("expected TenantMoving, got %+v", cond)
	}
	if status.Placement.Generation != 3 {
		t.Errorf("a voluntary move must not bump the generation before the drain, got %d", status.Placement.Generation)
	}
}

// TestPlace_Capacity_PoolCapAppliesToZeroMaxTenants covers task 4.8: a pool
// cell declaring maxTenants 0 resolves its capacity through its pool.
func TestPlace_Capacity_PoolCapAppliesToZeroMaxTenants(t *testing.T) {
	in := testInputs()
	in.Pools = []CellPoolSummary{{Name: "small", ProfileRef: "jenkins-lts", MaxTenantsPerCell: 2}}
	full := poolCell("cell-a", "small", "t1", "t2")
	in.Cells = []CellCandidate{full}
	team := testTeam()
	team.Spec.Pipelines.CellPool = "small"
	status := &v1alpha1.TeamStatus{}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement != nil {
		t.Fatalf("the pool cap of 2 must exclude a cell holding two tenants, got %+v", status.Placement)
	}
}

// TestPlace_Capacity_DeletedPoolFallsBackToChartValue covers task 4.8's
// second case: a cell whose pool was deleted uses the chart value.
func TestPlace_Capacity_DeletedPoolFallsBackToChartValue(t *testing.T) {
	in := testInputs()
	in.MaxTenantsDefault = 3
	orphan := poolCell("cell-a", "gone", "t1", "t2")
	in.Cells = []CellCandidate{orphan}
	team := testTeam()
	status := &v1alpha1.TeamStatus{}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement == nil || status.Placement.Cell != "cell-a" {
		t.Fatalf("the chart value of 3 must admit a third tenant, got %+v", status.Placement)
	}
}

func TestPlace_Selection_FullCellNotChosen(t *testing.T) {
	in := testInputs()
	full := readyCell("cell-a", "t1", "t2")
	full.MaxTenants = 2
	in.Cells = []CellCandidate{full}
	team := testTeam()
	status := &v1alpha1.TeamStatus{}

	_, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	cond := condition(status)
	if cond == nil || cond.Reason != v1alpha1.TeamReasonAwaitingCell {
		t.Errorf("expected AwaitingCell, got %+v", cond)
	}
	if status.Placement != nil {
		t.Error("no candidate must leave placement nil")
	}
}

func TestPlace_Selection_DeterministicTie(t *testing.T) {
	in := testInputs()
	b := readyCell("cell-b")
	a := readyCell("cell-a")
	in.Cells = []CellCandidate{b, a}
	team := testTeam()
	status := &v1alpha1.TeamStatus{}

	_, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement == nil || status.Placement.Cell != "cell-a" {
		t.Fatalf("a tie must break toward the name that sorts first, got %+v", status.Placement)
	}
}

// --- involuntary move write order (task 6.5) ---

func TestPlace_InvoluntaryMove_PlacementBeforeCellPatches(t *testing.T) {
	in := testInputs()
	evacuating := readyCell("cell-a")
	evacuating.Evacuate = true
	fresh := readyCell("cell-b")
	in.Cells = []CellCandidate{evacuating, fresh}
	team := testTeam()
	status := placedStatus(1)

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	// The fence: status.Placement already names the new cell and the new
	// generation by the time the cell patches are computed — Place is
	// synchronous and mutates status before appending to Outcome, so this
	// also proves the caller's patchStatus (which the placement pass never
	// performs itself) would carry the new placement before any cell patch
	// is applied.
	if status.Placement.Cell != "cell-b" || status.Placement.Generation != 2 {
		t.Fatalf("placement not updated: %+v", status.Placement)
	}
	var sawNew, sawOld bool
	for _, cp := range out.CellPatches {
		if cp.Name == "cell-b" {
			sawNew = true
		}
		if cp.Name == "cell-a" {
			sawOld = true
		}
	}
	if !sawNew || !sawOld {
		t.Fatalf("expected both cells patched: %+v", out.CellPatches)
	}
}

// --- voluntary move (task 6.5a) ---

func TestPlace_VoluntaryMove_BusyFolderWaits(t *testing.T) {
	in := testInputs()
	// The bound cell no longer serves the requested profile: a voluntary
	// move, not an evacuation.
	mismatched := readyCell("cell-a")
	mismatched.ResolvedProfile = "other-profile"
	mismatched.AppliedTenants = []string{"payments"} // still applying the old state
	other := readyCell("cell-b")
	in.Cells = []CellCandidate{mismatched, other}
	team := testTeam()
	status := placedStatus(1)

	// Tick 1: enter TenantMoving.
	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement.Cell != "cell-a" {
		t.Fatalf("the placement must not change on the tick that starts draining: %+v", status.Placement)
	}
	cond := condition(status)
	if cond == nil || cond.Reason != v1alpha1.TeamReasonTenantMoving {
		t.Fatalf("expected TenantMoving, got %+v", cond)
	}
	if len(out.CellPatches) != 0 {
		t.Error("starting a drain must patch no cell")
	}

	// Tick 2: still applying the old state (busy folder) — keep waiting.
	out, err = Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement.Cell != "cell-a" {
		t.Fatalf("a busy folder must not complete the move: %+v", status.Placement)
	}
	if len(out.CellPatches) != 0 {
		t.Error("a busy folder must patch no cell")
	}
}

func TestPlace_VoluntaryMove_DrainedCompletes(t *testing.T) {
	in := testInputs()
	mismatched := readyCell("cell-a")
	mismatched.ResolvedProfile = "other-profile"
	// AppliedTenants no longer names the tenant, and there is no pending
	// deletion for its folder: the drain has completed.
	other := readyCell("cell-b")
	in.Cells = []CellCandidate{mismatched, other}
	team := testTeam()
	status := placedStatus(1)
	// Simulate tick 1 having already set TenantMoving.
	status.Conditions = []v1alpha1.TeamCondition{{
		Type: v1alpha1.TeamConditionPlaced, Status: metav1.ConditionFalse,
		Reason: v1alpha1.TeamReasonTenantMoving, Message: tenantMovingMessage,
	}}

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement.Cell != "cell-b" || status.Placement.Generation != 2 {
		t.Fatalf("a drained folder must complete the move: %+v", status.Placement)
	}
	if len(out.CellPatches) != 2 {
		t.Fatalf("expected both cells patched, got %+v", out.CellPatches)
	}
}

func TestPlace_VoluntaryMove_PendingDeletionWaits(t *testing.T) {
	in := testInputs()
	mismatched := readyCell("cell-a")
	mismatched.ResolvedProfile = "other-profile"
	mismatched.PendingDeletionPaths = []string{"payments/api"}
	other := readyCell("cell-b")
	in.Cells = []CellCandidate{mismatched, other}
	team := testTeam()
	status := placedStatus(1)
	status.Conditions = []v1alpha1.TeamCondition{{
		Type: v1alpha1.TeamConditionPlaced, Status: metav1.ConditionFalse,
		Reason: v1alpha1.TeamReasonTenantMoving, Message: tenantMovingMessage,
	}}

	_, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement.Cell != "cell-a" {
		t.Fatalf("a pending deletion under the tenant folder must delay the move: %+v", status.Placement)
	}
}

// --- teardown (tasks 6.9, 6.10, 6.11) ---

func TestPlace_Teardown_HeldBeforeApply(t *testing.T) {
	in := testInputs()
	cell := readyCell("cell-a", "payments")
	cell.AppliedTenants = []string{"payments"}
	in.Cells = []CellCandidate{cell}
	team := testTeam()
	team.Spec.Pipelines = nil
	team.Finalizers = []string{v1alpha1.TeamFinalizerTenantPlacement}
	status := placedStatus(1)

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement == nil {
		t.Fatal("the finalizer holds until the apply confirms, so placement stays set")
	}
	if out.FinalizerRelease {
		t.Error("the finalizer must not release before the cell applies")
	}
	cond := condition(status)
	if cond == nil || cond.Reason != v1alpha1.TeamReasonTenantTeardownPending {
		t.Errorf("expected TenantTeardownPending, got %+v", cond)
	}
	if len(out.CellPatches) != 1 || len(out.CellPatches[0].Tenants) != 0 {
		t.Errorf("stage one must drop the tenant from the cell's tenant list: %+v", out.CellPatches)
	}
}

func TestPlace_Teardown_DeferredDeletionHolds(t *testing.T) {
	in := testInputs()
	cell := readyCell("cell-a", "payments")
	cell.PendingDeletionPaths = []string{"payments"}
	in.Cells = []CellCandidate{cell}
	team := testTeam()
	team.Spec.Pipelines = nil
	team.Finalizers = []string{v1alpha1.TeamFinalizerTenantPlacement}
	status := placedStatus(1)

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if out.FinalizerRelease {
		t.Error("a deferred deletion must hold the finalizer")
	}
	if status.Placement == nil {
		t.Fatal("placement must stay set while the finalizer holds")
	}
}

func TestPlace_Teardown_ReleasesOnceDrainedAndPurged(t *testing.T) {
	in := testInputs()
	in.Cells = []CellCandidate{readyCell("cell-a")} // no longer applying the tenant
	team := testTeam()
	team.Spec.Pipelines = nil
	team.Finalizers = []string{v1alpha1.TeamFinalizerTenantPlacement}
	status := placedStatus(1)

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement != nil {
		t.Errorf("placement must clear once drained and purged, got %+v", status.Placement)
	}
	if status.Pipelines != nil {
		t.Errorf("pipelines status must clear on teardown, got %+v", status.Pipelines)
	}
	if !out.FinalizerRelease {
		t.Error("expected the finalizer to release")
	}
	if condition(status) != nil {
		t.Error("the Placed condition must be removed once teardown completes")
	}
}

func TestPlace_Teardown_AbsentCellCompletesAtOnce(t *testing.T) {
	in := testInputs()
	in.Cells = nil // the owning Controller no longer exists
	team := testTeam()
	team.Spec.Pipelines = nil
	team.Finalizers = []string{v1alpha1.TeamFinalizerTenantPlacement}
	status := placedStatus(1)

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement != nil {
		t.Errorf("an absent cell must complete teardown at once, got %+v", status.Placement)
	}
	if !out.FinalizerRelease {
		t.Error("expected the finalizer to release when the cell is absent")
	}
}

func TestPlace_Teardown_NoFinalizerReleaseWhenAbsent(t *testing.T) {
	// A never-placed Team with no finalizer and no placement: teardown is a
	// no-op, and Place must not ask the caller to release a finalizer that
	// was never added.
	in := testInputs()
	team := testTeam()
	team.Spec.Pipelines = nil
	status := &v1alpha1.TeamStatus{}

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if out.FinalizerRelease {
		t.Error("must not release a finalizer that was never added")
	}
}

func TestPlace_Teardown_BoundedWaitCompletes(t *testing.T) {
	cell := readyCell("cell-a", "payments")
	cell.AppliedTenants = []string{"payments"} // never confirms the drain
	team := testTeam()
	team.Spec.Pipelines = nil
	team.Finalizers = []string{v1alpha1.TeamFinalizerTenantPlacement}
	status := placedStatus(1)

	in := testInputs()
	in.Cells = []CellCandidate{cell}
	in.Now = fixedClock(fixedNow)
	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement == nil {
		t.Fatal("expected the finalizer to hold before the bound expires")
	}

	in.Now = fixedClock(fixedNow.Add(11 * time.Minute))
	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement != nil {
		t.Errorf("the bounded wait must complete teardown, got %+v", status.Placement)
	}
	if !out.FinalizerRelease {
		t.Error("expected the finalizer to release once the bound expires")
	}
}

func TestPlace_Teardown_AgentPodsHoldStageTwo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cells []CellCandidate
	}{
		{"drained cell", []CellCandidate{readyCell("cell-a")}},
		{"absent cell", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			team := testTeam()
			team.Spec.Pipelines = nil
			team.Finalizers = []string{v1alpha1.TeamFinalizerTenantPlacement}
			status := placedStatus(1)

			in := testInputs()
			in.Cells = tc.cells
			in.AgentPodsPending = true
			out, err := Place(context.Background(), in, team, status)
			if err != nil {
				t.Fatalf("Place returned error: %v", err)
			}
			if out.FinalizerRelease || status.Placement == nil || status.Pipelines == nil {
				t.Fatalf("a remaining agent pod must hold stage two, got release=%v status=%+v", out.FinalizerRelease, status)
			}
			if cond := condition(status); cond == nil || cond.Reason != v1alpha1.TeamReasonTenantTeardownPending {
				t.Fatalf("Placed = %+v, want TenantTeardownPending", cond)
			}

			in.AgentPodsPending = false
			out, err = Place(context.Background(), in, team, status)
			if err != nil {
				t.Fatalf("Place returned error: %v", err)
			}
			if !out.FinalizerRelease || status.Placement != nil {
				t.Errorf("teardown must complete once no agent pod remains, got release=%v", out.FinalizerRelease)
			}
		})
	}
}

func TestPlace_Teardown_BoundedWaitEscapesAgentPods(t *testing.T) {
	team := testTeam()
	team.Spec.Pipelines = nil
	team.Finalizers = []string{v1alpha1.TeamFinalizerTenantPlacement}
	status := placedStatus(1)

	in := testInputs()
	in.Cells = nil
	in.AgentPodsPending = true
	in.Now = fixedClock(fixedNow)
	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement == nil {
		t.Fatal("expected the hold before the bound expires")
	}

	in.Now = fixedClock(fixedNow.Add(11 * time.Minute))
	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if !out.FinalizerRelease || status.Placement != nil {
		t.Errorf("the bounded wait must complete teardown over a remaining agent pod, got release=%v", out.FinalizerRelease)
	}
}

func TestPlace_Teardown_PurgeFailureHoldsThenGivesUp(t *testing.T) {
	team := testTeam()
	team.Spec.Pipelines = nil
	team.Finalizers = []string{v1alpha1.TeamFinalizerTenantPlacement}
	status := placedStatus(1)

	in := testInputs()
	in.Cells = []CellCandidate{readyCell("cell-a")}
	in.RequestPurge = failingPurge()

	var out Outcome
	var err error
	for i := 0; i < 9; i++ {
		out, err = Place(context.Background(), in, team, status)
		if err != nil {
			t.Fatalf("Place returned error on attempt %d: %v", i+1, err)
		}
		if out.FinalizerRelease {
			t.Fatalf("must not release before ten failed attempts (attempt %d)", i+1)
		}
		if status.Pipelines == nil || status.Pipelines.PurgeAttempts != int32(i+1) {
			t.Fatalf("attempt %d: purgeAttempts = %+v, want %d", i+1, status.Pipelines, i+1)
		}
		cond := condition(status)
		if cond == nil || cond.Reason != v1alpha1.TeamReasonTenantTeardownPending {
			t.Fatalf("attempt %d: expected TenantTeardownPending, got %+v", i+1, cond)
		}
	}

	out, err = Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error on the tenth attempt: %v", err)
	}
	if !out.FinalizerRelease {
		t.Error("expected teardown to complete after the tenth failed attempt")
	}
	if status.Placement != nil {
		t.Errorf("expected placement cleared after giving up, got %+v", status.Placement)
	}
	var sawPurgeFailed bool
	for _, ev := range out.ActivityEvents {
		if ev.Type == "tenant.purge_failed" {
			sawPurgeFailed = true
		}
	}
	if !sawPurgeFailed {
		t.Error("expected a tenant.purge_failed activity event")
	}
}

func TestPlace_Teardown_DoesNotHoldWhenControllerAbsent(t *testing.T) {
	team := testTeam()
	team.Spec.Pipelines = nil
	team.Finalizers = []string{v1alpha1.TeamFinalizerTenantPlacement}
	status := placedStatus(1)

	in := testInputs()
	in.Cells = nil // the owning Controller is gone
	in.RequestPurge = failingPurge()

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if !out.FinalizerRelease {
		t.Error("an absent Controller must never hold the finalizer, even when the purge fails")
	}
}

// TestPlace_TeardownPurgeCarriesTheIncarnation asserts the teardown purge
// names the Team object it tears down. The recorder compares that value
// with the one its placement row holds, so a purge that arrives after a
// tenant of the same name is created again keeps the new row.
func TestPlace_TeardownPurgeCarriesTheIncarnation(t *testing.T) {
	team := testTeam()
	team.UID = "uid-old"
	team.Spec.Pipelines = nil
	team.Finalizers = []string{v1alpha1.TeamFinalizerTenantPlacement}
	status := placedStatus(1)

	var captured PurgeRequest
	in := testInputs()
	in.Cells = nil
	in.RequestPurge = func(_ context.Context, req PurgeRequest) (bool, error) {
		captured = req
		return true, nil
	}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if captured.Incarnation != "uid-old" {
		t.Errorf("teardown purge incarnation = %q, want uid-old", captured.Incarnation)
	}
}

// TestPlace_AnnounceCarriesTheIncarnation asserts the placement
// announcement names the Team object it places. The recorder stores that
// value, and the teardown purge reads it.
func TestPlace_AnnounceCarriesTheIncarnation(t *testing.T) {
	team := testTeam()
	team.UID = "uid-new"

	var captured AnnounceRequest
	in := testInputs()
	in.Cells = []CellCandidate{readyCell("cell-a")}
	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		captured = req
		return AnnounceResult{OK: true}, nil
	}

	status := &v1alpha1.TeamStatus{}
	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if captured.Incarnation != "uid-new" {
		t.Errorf("announce incarnation = %q, want uid-new", captured.Incarnation)
	}
}

// --- schedule purge on a move (task 6.11) ---

func TestPlace_Move_PurgeRequestCarriesOldGeneration(t *testing.T) {
	var captured PurgeRequest
	var called bool
	in := testInputs()
	in.RequestPurge = func(_ context.Context, req PurgeRequest) (bool, error) {
		captured = req
		called = true
		return true, nil
	}
	evacuating := readyCell("cell-a")
	evacuating.Evacuate = true
	other := readyCell("cell-b")
	in.Cells = []CellCandidate{evacuating, other}
	team := testTeam()
	status := placedStatus(5)

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if !called {
		t.Fatal("expected a purge request on a move")
	}
	if captured.OldGeneration != 5 || captured.Tenant != "payments" {
		t.Errorf("unexpected purge request: %+v", captured)
	}
	if captured.Cell != "cell-a" || captured.CellNamespace != "cells-ns" {
		t.Errorf("the purge must name the old cell and its namespace: %+v", captured)
	}
}

func TestPlace_Move_PurgeFailureDoesNotBlock(t *testing.T) {
	in := testInputs()
	in.RequestPurge = failingPurge()
	evacuating := readyCell("cell-a")
	evacuating.Evacuate = true
	other := readyCell("cell-b")
	in.Cells = []CellCandidate{evacuating, other}
	team := testTeam()
	status := placedStatus(1)

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement.Cell != "cell-b" || status.Placement.Generation != 2 {
		t.Fatalf("a failed purge must not block the move: %+v", status.Placement)
	}
	if len(out.CellPatches) != 2 {
		t.Errorf("expected both cells patched despite the purge failure: %+v", out.CellPatches)
	}
}

// TestPlace_Move_PurgeRetrySurvivesTransientCellNotReadyTick proves the
// move-purge retry is durable: a transient CellNotReady tick between a
// failed purge and its retry must not lose the retry (design D4's "retries
// on the next tick with the same old generation"). Retrying off the stored
// TenantMoved reason would fail this, because a CellNotReady tick
// overwrites that reason; PendingPurgeGeneration must not.
func TestPlace_Move_PurgeRetrySurvivesTransientCellNotReadyTick(t *testing.T) {
	var requests []PurgeRequest
	purgeOK := false
	in := testInputs()
	in.RequestPurge = func(_ context.Context, req PurgeRequest) (bool, error) {
		requests = append(requests, req)
		return purgeOK, nil
	}
	evacuating := readyCell("cell-a")
	evacuating.Evacuate = true
	cellB := readyCell("cell-b")
	in.Cells = []CellCandidate{evacuating, cellB}
	team := testTeam()
	status := placedStatus(1)

	// Tick 1: the move fires and its purge fails.
	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement.Cell != "cell-b" || status.Placement.Generation != 2 {
		t.Fatalf("expected the move to land on cell-b at generation 2: %+v", status.Placement)
	}
	if status.Pipelines == nil || status.Pipelines.PendingPurgeGeneration == nil || *status.Pipelines.PendingPurgeGeneration != 1 {
		t.Fatalf("expected PendingPurgeGeneration=1 after the failed purge: %+v", status.Pipelines)
	}

	// Tick 2: a transient CellNotReady tick on the new cell. The purge
	// still fails, and the Placed reason is overwritten away from
	// TenantMoved.
	notReadyB := readyCell("cell-b")
	notReadyB.CellReady = false
	in.Cells = []CellCandidate{notReadyB}
	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if cond := condition(status); cond == nil || cond.Reason != v1alpha1.TeamReasonCellNotReady {
		t.Fatalf("expected the transient tick to overwrite the reason to CellNotReady, got %+v", cond)
	}
	if len(requests) != 2 {
		t.Fatalf("expected the transient tick to still retry the purge, got %d requests", len(requests))
	}
	if status.Pipelines.PendingPurgeGeneration == nil || *status.Pipelines.PendingPurgeGeneration != 1 {
		t.Fatalf("expected the retry to remain pending after the transient tick: %+v", status.Pipelines)
	}
	for _, req := range requests {
		// Every retry names the cell the move left, and its namespace.
		if req.Cell != "cell-a" || req.CellNamespace != "cells-ns" {
			t.Fatalf("a retry lost the old cell reference: %+v", req)
		}
	}

	// Tick 3: cell-b is ready again and the purge now succeeds.
	purgeOK = true
	in.Cells = []CellCandidate{cellB}
	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if len(requests) != 3 {
		t.Fatalf("expected a third retry, got %d requests", len(requests))
	}
	for _, req := range requests {
		if req.OldGeneration != 1 || req.Tenant != "payments" {
			t.Errorf("unexpected purge request: %+v", req)
		}
	}
	if status.Pipelines.PendingPurgeGeneration != nil {
		t.Errorf("expected PendingPurgeGeneration cleared once the purge succeeds, got %v", *status.Pipelines.PendingPurgeGeneration)
	}
	if status.Pipelines.PendingPurgeCell != "" || status.Pipelines.PendingPurgeCellNamespace != "" {
		t.Errorf("expected the pending purge cell cleared with it: %+v", status.Pipelines)
	}

	// Tick 4: the pending purge is gone, so the tick asks for nothing. A
	// clear the status patch never carries would repeat the completed
	// purge on every tick that follows.
	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if len(requests) != 3 {
		t.Fatalf("a tick after the successful purge asked again: %d requests", len(requests))
	}
}

// --- door credentials (task 6.7) ---

func TestPlace_CredentialsMintedOnceAndStable(t *testing.T) {
	in := testInputs()
	in.Cells = []CellCandidate{readyCell("cell-a")}
	team := testTeam()
	status := &v1alpha1.TeamStatus{}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Pipelines == nil || len(status.Pipelines.WebhookToken) != 32 || len(status.Pipelines.WebhookSecret) != 64 {
		t.Fatalf("expected 16-byte token and 32-byte secret, hex-encoded: %+v", status.Pipelines)
	}
	token, secret := status.Pipelines.WebhookToken, status.Pipelines.WebhookSecret

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Pipelines.WebhookToken != token || status.Pipelines.WebhookSecret != secret {
		t.Error("a stored credential must never be regenerated")
	}
}

// --- placement.cluster (task 6.6a) ---

func TestPlace_PlacementClusterWrittenTogether(t *testing.T) {
	in := testInputs()
	in.Cells = []CellCandidate{readyCell("cell-a")}
	in.ClusterIdentity = "core"
	team := testTeam()
	status := &v1alpha1.TeamStatus{}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement == nil {
		t.Fatal("expected a placement")
	}
	if status.Placement.Cluster != "core" || status.Placement.Cell != "cell-a" || status.Placement.CellNamespace != "cells-ns" {
		t.Errorf("expected cluster, cell, and cellNamespace written together: %+v", status.Placement)
	}
}

// --- finalizer (spec: added on first placement) ---

func TestPlace_FinalizerAddOnFirstPlacement(t *testing.T) {
	in := testInputs()
	in.Cells = []CellCandidate{readyCell("cell-a")}
	team := testTeam()
	status := &v1alpha1.TeamStatus{}

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if !out.FinalizerAdd {
		t.Error("expected the finalizer to be added on the first placement")
	}
}

func TestPlace_NoFinalizerAddWithoutPlacement(t *testing.T) {
	in := testInputs()
	team := testTeam()
	status := &v1alpha1.TeamStatus{}

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if out.FinalizerAdd {
		t.Error("must not add the finalizer without a placement")
	}
}

// --- unaffected Team (tenant-model spec scenario) ---

func TestPlace_NoPipelinesIsUnaffected(t *testing.T) {
	in := testInputs()
	team := &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "plain"}}
	status := &v1alpha1.TeamStatus{}

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement != nil || status.Pipelines != nil {
		t.Errorf("a Team with no pipelines must stay untouched: %+v", status)
	}
	if condition(status) != nil {
		t.Error("a Team with no pipelines must get no Placed condition")
	}
	if out.FinalizerAdd || out.FinalizerRelease {
		t.Error("a Team with no pipelines must never touch the finalizer")
	}
}

func TestPlace_Move_PurgeRetrySurvivesARefusalGate(t *testing.T) {
	var requests []PurgeRequest
	purgeOK := false
	in := testInputs()
	in.RequestPurge = func(_ context.Context, req PurgeRequest) (bool, error) {
		requests = append(requests, req)
		return purgeOK, nil
	}
	evacuating := readyCell("cell-a")
	evacuating.Evacuate = true
	in.Cells = []CellCandidate{evacuating, readyCell("cell-b")}
	team := testTeam()
	status := placedStatus(1)

	// Tick 1: the move fires and its purge fails.
	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Pipelines == nil || status.Pipelines.PendingPurgeGeneration == nil {
		t.Fatalf("expected a pending purge after the failed request: %+v", status.Pipelines)
	}

	// Tick 2: the profile is gone, so a refusal gate fires. The purge
	// retry must still run.
	in.ProfileExists = func(string) bool { return false }
	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if cond := condition(status); cond == nil || cond.Reason != v1alpha1.TeamReasonProfileNotFound {
		t.Fatalf("expected ProfileNotFound, got %+v", cond)
	}
	if len(requests) != 2 || requests[1].OldGeneration != 1 {
		t.Fatalf("expected a second purge request for generation 1 despite the refusal, got %+v", requests)
	}

	// Tick 3: the purge succeeds and the pending generation clears.
	purgeOK = true
	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Pipelines.PendingPurgeGeneration != nil {
		t.Errorf("expected the pending purge to clear after success: %+v", status.Pipelines)
	}
}

// --- the confirmed-gone stamp on a dead instance ---

// TestPlace_Reannounce_StampsConfirmation asserts the reannounce path
// stamps ConfirmedGoneAt with the tick's clock. That path stands behind
// the reconciler's own pod read, so the entry carries proof.
func TestPlace_Reannounce_StampsConfirmation(t *testing.T) {
	lastSeen := fixedNow.Add(-2 * time.Minute)
	in := testInputs()
	in.LastSync = &SyncCache{
		Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1, CellInstance: "uid-1",
		CellLastSeenAt: &lastSeen, At: lastSeen,
	}
	rolled := readyCell("cell-a", "payments")
	rolled.Instance = "uid-2"
	rolled.MiteConnected = true
	in.Cells = []CellCandidate{rolled}

	var got AnnounceRequest
	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		got = req
		return AnnounceResult{OK: true}, nil
	}

	team := testTeam()
	status := placedStatus(1)
	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if len(got.DeadInstances) != 1 {
		t.Fatalf("dead instances = %+v, want exactly one", got.DeadInstances)
	}
	entry := got.DeadInstances[0]
	if entry.Instance != "uid-1" {
		t.Fatalf("dead instance = %q, want uid-1", entry.Instance)
	}
	if entry.ConfirmedGoneAt == nil || !entry.ConfirmedGoneAt.Equal(fixedNow) {
		t.Fatalf("ConfirmedGoneAt = %v, want the tick's clock %v", entry.ConfirmedGoneAt, fixedNow)
	}
}

// TestPlace_Move_StampsNoConfirmation asserts the move path stamps no
// confirmation. That path reads no pod: it proves only that the old cell
// object left the candidate set, and a deleting cell pod can still run
// its whole termination grace.
func TestPlace_Move_StampsNoConfirmation(t *testing.T) {
	lastSeen := fixedNow.Add(-2 * time.Minute)
	in := testInputs()
	in.LastSync = &SyncCache{
		Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1, CellInstance: "uid-1",
		CellLastSeenAt: &lastSeen, At: lastSeen,
	}
	// The bound cell object is gone from the candidate set, so the move
	// path reports it as dead without any pod read behind the entry.
	in.Cells = []CellCandidate{readyCell("cell-b")}

	var got AnnounceRequest
	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		got = req
		return AnnounceResult{OK: true}, nil
	}

	team := testTeam()
	status := placedStatus(1)
	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if len(got.DeadInstances) != 1 {
		t.Fatalf("dead instances = %+v, want exactly one", got.DeadInstances)
	}
	entry := got.DeadInstances[0]
	if entry.Instance != "uid-1" {
		t.Fatalf("dead instance = %q, want uid-1", entry.Instance)
	}
	if entry.ConfirmedGoneAt != nil {
		t.Fatalf("ConfirmedGoneAt = %v, want nil: the move path reads no pod", entry.ConfirmedGoneAt)
	}
}
