package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	miteTransport "github.com/varroaci/varroa-jenkins/internal/transport"
)

// newDrainingCellFixture seeds a cell with a stay tenant and a mover tenant
// that is draining away from the cell. The cell already applied both.
func newDrainingCellFixture(t *testing.T) (*testClient, *Reconciler, *captureTransport, *v1alpha1.Controller) {
	t.Helper()
	client := newTestClientWithBundle()
	rec := newTestReconcilerWithTokenSigner(client)
	transport := &captureTransport{}
	rec.miteTransport = transport
	cr := newItemsFailureCell("")
	cr.Status.Cell = &v1alpha1.CellStatus{
		Tenants:        []string{"mover", "stay"},
		AppliedTenants: []string{"mover", "stay"},
	}
	stay := pipelinesTeam("stay")
	seedTenantBundle(client, stay, "stay-bundle", cleanPipelineItemsYAML)
	mover := pipelinesTeam("mover")
	seedTenantBundle(client, mover, "mover-bundle", cleanPipelineItemsYAML)
	markDraining(mover, cr)
	crdstore.MustSeed(client.store, stay)
	crdstore.MustSeed(client.store, mover)
	return client, rec, transport, cr
}

// TestCellDrainingTenantLeavesAppliedTenants pins that a draining tenant
// leaves status.cell.appliedTenants once the cell applies the pushed desired
// state, even though the tenant is still in cell.Tenants.
func TestCellDrainingTenantLeavesAppliedTenants(t *testing.T) {
	_, rec, transport, cr := newDrainingCellFixture(t)

	_ = rec.reconcileController(context.Background(), cr)
	if cr.Status.DesiredStateHash == "" {
		t.Fatal("first tick did not push a desired state")
	}

	transport.drainResults = []*mitev1.CommandResult{
		mkDSR(true, true, true, true, "", "", "", "", cr.Status.DesiredStateHash),
	}
	_ = rec.reconcileController(context.Background(), cr)

	if want := []string{"stay"}; !slices.Equal(cr.Status.Cell.AppliedTenants, want) {
		t.Fatalf("AppliedTenants = %v, want %v", cr.Status.Cell.AppliedTenants, want)
	}
}

// TestCellDrainingTenantsFollowDeliveredPush pins that the draining set
// changes only with a delivered push. A push that fails leaves the set and
// the hash as they were, even when the tenant has stopped draining.
func TestCellDrainingTenantsFollowDeliveredPush(t *testing.T) {
	client, rec, transport, cr := newDrainingCellFixture(t)

	_ = rec.reconcileController(context.Background(), cr)
	if want := []string{"mover"}; !slices.Equal(cr.Status.Cell.DrainingTenants, want) {
		t.Fatalf("DrainingTenants = %v after a delivered push, want %v", cr.Status.Cell.DrainingTenants, want)
	}
	hash := cr.Status.DesiredStateHash

	// The mover no longer drains, so the next push would carry its folder.
	// That push is not delivered.
	transport.sendErr = miteTransport.ErrMiteUnreachable
	mover := pipelinesTeam("mover")
	seedTenantBundle(client, mover, "mover-bundle", cleanPipelineItemsYAML)
	crdstore.MustSeed(client.store, mover)
	_ = rec.reconcileController(context.Background(), cr)

	if want := []string{"mover"}; !slices.Equal(cr.Status.Cell.DrainingTenants, want) {
		t.Fatalf("DrainingTenants = %v after an undelivered push, want %v", cr.Status.Cell.DrainingTenants, want)
	}
	if cr.Status.DesiredStateHash != hash {
		t.Fatalf("DesiredStateHash = %q after an undelivered push, want %q", cr.Status.DesiredStateHash, hash)
	}
}

// TestAppliedTenantsAfterItemsApply_DrainingTenantNeverApplied pins that a
// draining tenant never enters the applied set, whatever its earlier state.
func TestAppliedTenantsAfterItemsApply_DrainingTenantNeverApplied(t *testing.T) {
	cases := []struct {
		name         string
		cell         v1alpha1.CellStatus
		refusedPaths []string
		want         []string
	}{
		{
			name: "draining tenant applied before is dropped",
			cell: v1alpha1.CellStatus{
				Tenants:         []string{"a", "b"},
				AppliedTenants:  []string{"a", "b"},
				DrainingTenants: []string{"a"},
			},
			want: []string{"b"},
		},
		{
			name: "draining tenant with a refused path is dropped",
			cell: v1alpha1.CellStatus{
				Tenants:         []string{"a", "b"},
				AppliedTenants:  []string{"a", "b"},
				DrainingTenants: []string{"a"},
			},
			refusedPaths: []string{"a/repos"},
			want:         []string{"b"},
		},
		{
			name: "draining tenant never applied stays out",
			cell: v1alpha1.CellStatus{
				Tenants:         []string{"a", "b"},
				AppliedTenants:  []string{"b"},
				DrainingTenants: []string{"a"},
			},
			want: []string{"b"},
		},
		{
			name: "non-draining tenant still enters",
			cell: v1alpha1.CellStatus{
				Tenants: []string{"a", "b"},
			},
			want: []string{"a", "b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cell := tc.cell
			got := appliedTenantsAfterItemsApply(&cell, tc.refusedPaths)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("appliedTenantsAfterItemsApply = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAssembleCellContent_ReportsDrainingTenants pins that the assembly names
// each draining tenant, so the reconciler can record the set with the push.
func TestAssembleCellContent_ReportsDrainingTenants(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	clean := pipelinesTeam("clean")
	seedTenantBundle(client, clean, "clean-bundle", cleanPipelineItemsYAML)
	draining := pipelinesTeam("draining")
	markDraining(draining, cr)
	seedTenantBundle(client, draining, "draining-bundle", cleanPipelineItemsYAML)

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Team{clean, draining}, nil, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	if want := []string{"draining"}; !slices.Equal(state.Draining, want) {
		t.Fatalf("state.Draining = %v, want %v", state.Draining, want)
	}
}

// TestDrainingTenantsStayASubsetOfTenants pins that a cell at the 64-item
// limit with every tenant draining reports a draining set that is a subset of
// status.cell.tenants and fits the same limit, with no truncation.
func TestDrainingTenantsStayASubsetOfTenants(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	names := make([]string, 0, 64)
	teams := make([]*v1alpha1.Team, 0, 64)
	for i := range 64 {
		name := fmt.Sprintf("tenant-%02d", i)
		names = append(names, name)
		team := pipelinesTeam(name)
		markDraining(team, cr)
		seedTenantBundle(client, team, name+"-bundle", cleanPipelineItemsYAML)
		teams = append(teams, team)
	}
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: names}

	state, err := rec.assembleCellContent(context.Background(), cr, teams, nil, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	if len(state.Draining) != len(names) {
		t.Fatalf("len(Draining) = %d, want %d: the set must not truncate", len(state.Draining), len(names))
	}
	for _, d := range state.Draining {
		if !slices.Contains(names, d) {
			t.Fatalf("draining tenant %q is not in Tenants", d)
		}
	}
}

// withCellInstance stamps the cell instance of a desired-state result, the
// way the gateway stamps it from the stream certificate.
func withCellInstance(res *mitev1.CommandResult, uid string) *mitev1.CommandResult {
	res.GetDesiredState().CellInstance = uid
	return res
}

// TestCellItemsApplyResultFromReplacedInstanceIgnored pins that a clean items
// result from a pod that is no longer the cell instance changes neither the
// applied set nor the refused tenant warning.
func TestCellItemsApplyResultFromReplacedInstanceIgnored(t *testing.T) {
	newCell := func() *v1alpha1.Controller {
		cr := newItemsFailureCell("hash-1")
		cr.Status.Cell.Instance = "pod-new"
		cr.Status.Cell.AppliedTenants = []string{"search"}
		cr.Status.Cell.TenantWarnings = []v1alpha1.TenantWarning{{
			Tenant: "payments", Reason: v1alpha1.TeamReasonItemsApplyFailed, Message: "create payments/repos: boom",
		}}
		return cr
	}
	setup := func() (*Reconciler, *captureTransport) {
		client := newTestClientWithBundle()
		client.controllerPod = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{UID: types.UID("pod-new")},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
		rec := newTestReconcilerWithTokenSigner(client)
		transport := &captureTransport{}
		rec.miteTransport = transport
		return rec, transport
	}

	t.Run("replaced pod result", func(t *testing.T) {
		rec, transport := setup()
		cr := newCell()
		transport.drainResults = []*mitev1.CommandResult{
			withCellInstance(mkDSR(true, true, true, true, "", "", "", "", "hash-1"), "pod-old"),
		}
		_ = rec.reconcileController(context.Background(), cr)

		if want := []string{"search"}; !slices.Equal(cr.Status.Cell.AppliedTenants, want) {
			t.Fatalf("AppliedTenants = %v, want %v", cr.Status.Cell.AppliedTenants, want)
		}
		if n := itemsFailureCount(cr, "payments"); n != 1 {
			t.Fatalf("payments ItemsApplyFailed warnings = %d, want 1", n)
		}
	})

	t.Run("current pod result", func(t *testing.T) {
		rec, transport := setup()
		cr := newCell()
		transport.drainResults = []*mitev1.CommandResult{
			withCellInstance(mkDSR(true, true, true, true, "", "", "", "", "hash-1"), "pod-new"),
		}
		_ = rec.reconcileController(context.Background(), cr)

		if want := []string{"payments", "search"}; !slices.Equal(cr.Status.Cell.AppliedTenants, want) {
			t.Fatalf("AppliedTenants = %v, want %v", cr.Status.Cell.AppliedTenants, want)
		}
		if n := itemsFailureCount(cr, "payments"); n != 0 {
			t.Fatalf("payments ItemsApplyFailed warnings = %d, want 0", n)
		}
	})
}

// TestCellBufferedResultFromReplacedPodDoesNotReaddDroppedTenant pins that a
// result which the old pod buffered does not re-add a tenant that the pod
// change dropped.
func TestCellBufferedResultFromReplacedPodDoesNotReaddDroppedTenant(t *testing.T) {
	client := newTestClientWithBundle()
	client.controllerPod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("pod-new")},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	rec := newTestReconcilerWithTokenSigner(client)
	transport := &captureTransport{}
	rec.miteTransport = transport

	cr := newItemsFailureCell("hash-1")
	cr.Status.Cell.Instance = "pod-old"
	cr.Status.Cell.AppliedTenants = []string{"payments", "search"}
	cr.Status.Cell.TenantWarnings = []v1alpha1.TenantWarning{{
		Tenant: "payments", Reason: v1alpha1.TeamReasonItemsApplyFailed, Message: "create payments/repos: boom",
	}}

	transport.drainResults = []*mitev1.CommandResult{
		withCellInstance(mkDSR(true, true, true, true, "", "", "", "", "hash-1"), "pod-old"),
	}
	_ = rec.reconcileController(context.Background(), cr)

	if want := []string{"search"}; !slices.Equal(cr.Status.Cell.AppliedTenants, want) {
		t.Fatalf("AppliedTenants = %v, want %v: a buffered result from the replaced pod re-added a dropped tenant", cr.Status.Cell.AppliedTenants, want)
	}
}

// parkedCellResultCount reports how many results the reconciler holds for a
// controller key.
func parkedCellResultCount(rec *Reconciler, key string) int {
	rec.cellResultMu.Lock()
	defer rec.cellResultMu.Unlock()
	if _, ok := rec.parkedCellResults[key]; ok {
		return 1
	}
	return 0
}

// newParkingCellFixture returns a cell on pod-old with payments refused and
// search applied, and a reconciler whose pod read the test controls.
func newParkingCellFixture(t *testing.T) (*testClient, *Reconciler, *captureTransport, *v1alpha1.Controller) {
	t.Helper()
	client := newTestClientWithBundle()
	client.controllerPod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("pod-old")},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	rec := newTestReconcilerWithTokenSigner(client)
	transport := &captureTransport{}
	rec.miteTransport = transport

	cr := newItemsFailureCell("")
	// A warm-up tick pushes the desired state, so the hash is stable before
	// the test drains a result for it.
	_ = rec.reconcileController(context.Background(), cr)
	cr.Status.Cell.Instance = "pod-old"
	cr.Status.Cell.AppliedTenants = []string{"search"}
	cr.Status.Cell.TenantWarnings = []v1alpha1.TenantWarning{{
		Tenant: "payments", Reason: v1alpha1.TeamReasonItemsApplyFailed, Message: "create payments/repos: boom",
	}}
	return client, rec, transport, cr
}

// TestCellResultDuringFailedPodReadAppliesAfterNextRead pins that a result
// from the new pod, drained while the pod read fails, applies on the next
// successful read instead of being lost.
func TestCellResultDuringFailedPodReadAppliesAfterNextRead(t *testing.T) {
	client, rec, transport, cr := newParkingCellFixture(t)
	key := cr.Namespace + "/" + cr.Name

	client.controllerPodErr = errors.New("apiserver unavailable")
	transport.drainResults = []*mitev1.CommandResult{
		withCellInstance(mkDSR(true, true, true, true, "", "", "", "", cr.Status.DesiredStateHash), "pod-new"),
	}
	_ = rec.reconcileController(context.Background(), cr)

	if want := []string{"search"}; !slices.Equal(cr.Status.Cell.AppliedTenants, want) {
		t.Fatalf("tick 1 AppliedTenants = %v, want %v", cr.Status.Cell.AppliedTenants, want)
	}
	if n := itemsFailureCount(cr, "payments"); n != 1 {
		t.Fatalf("tick 1 payments ItemsApplyFailed warnings = %d, want 1", n)
	}
	if n := parkedCellResultCount(rec, key); n != 1 {
		t.Fatalf("tick 1 parked results = %d, want 1", n)
	}

	client.controllerPodErr = nil
	client.controllerPod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("pod-new")},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	transport.drainResults = nil
	_ = rec.reconcileController(context.Background(), cr)

	if want := []string{"payments", "search"}; !slices.Equal(cr.Status.Cell.AppliedTenants, want) {
		t.Fatalf("tick 2 AppliedTenants = %v, want %v", cr.Status.Cell.AppliedTenants, want)
	}
	if n := itemsFailureCount(cr, "payments"); n != 0 {
		t.Fatalf("tick 2 payments ItemsApplyFailed warnings = %d, want 0", n)
	}
	if n := parkedCellResultCount(rec, key); n != 0 {
		t.Fatalf("tick 2 parked results = %d, want 0", n)
	}
}

// TestCellParkedResultDroppedWhenHashMoves pins that a parked result for an
// older desired-state hash is dropped on replay and never applied.
func TestCellParkedResultDroppedWhenHashMoves(t *testing.T) {
	client, rec, transport, cr := newParkingCellFixture(t)
	key := cr.Namespace + "/" + cr.Name

	client.controllerPodErr = errors.New("apiserver unavailable")
	transport.drainResults = []*mitev1.CommandResult{
		withCellInstance(mkDSR(true, true, true, true, "", "", "", "", cr.Status.DesiredStateHash), "pod-new"),
	}
	_ = rec.reconcileController(context.Background(), cr)

	client.controllerPodErr = nil
	client.controllerPod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("pod-new")},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	transport.drainResults = nil
	cr.Status.DesiredStateHash = "hash-2"
	rec.updateCellInstance(context.Background(), cr)

	if want := []string{"search"}; !slices.Equal(cr.Status.Cell.AppliedTenants, want) {
		t.Fatalf("AppliedTenants = %v, want %v: a result for a moved hash was applied", cr.Status.Cell.AppliedTenants, want)
	}
	if n := parkedCellResultCount(rec, key); n != 0 {
		t.Fatalf("parked results = %d, want 0", n)
	}
}

// TestCellParkedResultDroppedForReplacedPod pins that a result from a pod
// that was replaced again is dropped on the next successful read.
func TestCellParkedResultDroppedForReplacedPod(t *testing.T) {
	client, rec, transport, cr := newParkingCellFixture(t)
	key := cr.Namespace + "/" + cr.Name

	client.controllerPod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("pod-new")},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	transport.drainResults = []*mitev1.CommandResult{
		withCellInstance(mkDSR(true, true, true, true, "", "", "", "", cr.Status.DesiredStateHash), "pod-old"),
	}
	_ = rec.reconcileController(context.Background(), cr)

	transport.drainResults = nil
	_ = rec.reconcileController(context.Background(), cr)

	if want := []string{"search"}; !slices.Equal(cr.Status.Cell.AppliedTenants, want) {
		t.Fatalf("AppliedTenants = %v, want %v: a result from a replaced pod was applied", cr.Status.Cell.AppliedTenants, want)
	}
	if n := parkedCellResultCount(rec, key); n != 0 {
		t.Fatalf("parked results = %d, want 0", n)
	}
}

// TestCellParkedResultKeptWhileReadFails pins that a parked result survives
// failed pod reads and applies on the first successful read.
func TestCellParkedResultKeptWhileReadFails(t *testing.T) {
	client, rec, transport, cr := newParkingCellFixture(t)

	client.controllerPodErr = errors.New("apiserver unavailable")
	transport.drainResults = []*mitev1.CommandResult{
		withCellInstance(mkDSR(true, true, true, true, "", "", "", "", cr.Status.DesiredStateHash), "pod-new"),
	}
	_ = rec.reconcileController(context.Background(), cr)
	transport.drainResults = nil
	_ = rec.reconcileController(context.Background(), cr)

	if want := []string{"search"}; !slices.Equal(cr.Status.Cell.AppliedTenants, want) {
		t.Fatalf("while reads fail AppliedTenants = %v, want %v", cr.Status.Cell.AppliedTenants, want)
	}

	client.controllerPodErr = nil
	client.controllerPod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("pod-new")},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	_ = rec.reconcileController(context.Background(), cr)

	if want := []string{"payments", "search"}; !slices.Equal(cr.Status.Cell.AppliedTenants, want) {
		t.Fatalf("after the read succeeds AppliedTenants = %v, want %v", cr.Status.Cell.AppliedTenants, want)
	}
}

// TestCellParkedWalkResultNotReplacedByNonWalkResult pins that a result with
// no items walk, drained from the new pod later, does not overwrite the
// parked walk result that would re-add the refused tenant.
func TestCellParkedWalkResultNotReplacedByNonWalkResult(t *testing.T) {
	client, rec, transport, cr := newParkingCellFixture(t)
	key := cr.Namespace + "/" + cr.Name

	client.controllerPodErr = errors.New("apiserver unavailable")
	transport.drainResults = []*mitev1.CommandResult{
		withCellInstance(mkDSR(true, true, true, true, "", "", "", "", cr.Status.DesiredStateHash), "pod-new"),
	}
	_ = rec.reconcileController(context.Background(), cr)

	transport.drainResults = []*mitev1.CommandResult{
		withCellInstance(mkDSR(true, true, true, false, "", "", "", "deadline", cr.Status.DesiredStateHash), "pod-new"),
	}
	_ = rec.reconcileController(context.Background(), cr)

	if n := parkedCellResultCount(rec, key); n != 1 {
		t.Fatalf("parked results = %d, want 1", n)
	}

	client.controllerPodErr = nil
	client.controllerPod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("pod-new")},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	transport.drainResults = nil
	_ = rec.reconcileController(context.Background(), cr)

	if want := []string{"payments", "search"}; !slices.Equal(cr.Status.Cell.AppliedTenants, want) {
		t.Fatalf("AppliedTenants = %v, want %v: a non-walk result replaced the parked walk", cr.Status.Cell.AppliedTenants, want)
	}
}
