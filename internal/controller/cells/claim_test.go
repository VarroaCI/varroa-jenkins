package cells

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// testClockNow is the fixed time newReconciler injects. A claim test stamps
// its claim relative to it.
var testClockNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// claimedCell returns a free cell that carries one placement claim for the
// test tenant, stamped at claimedAt.
func claimedCell(name string, claimedAt time.Time) *v1alpha1.Controller {
	const tenant = "team-a"
	cell := versionedCell(name, "uid-1", "1", nil)
	cell.Status.Cell = &v1alpha1.CellStatus{
		PendingTenants: []v1alpha1.CellPendingTenant{{
			Tenant:    tenant,
			ClaimedAt: metav1.NewTime(claimedAt),
		}},
	}
	return cell
}

// TestCellPool_DrainConflictsWithAClaimWrittenAfterItsReRead names the
// interleaving the claim closes. The drain re-reads a free cell. The
// placement pass then claims that cell and writes the Tenant fence. The
// conditional delete must fail, because the claim moves the cell's
// resourceVersion after the drain observes it.
func TestCellPool_DrainConflictsWithAClaimWrittenAfterItsReRead(t *testing.T) {
	store := crdstore.NewFake()
	cell := versionedCell("cell-lts-2-555-1", "uid-1", "1", nil)
	crdstore.MustSeed(store, newProfile(), newPool(0), cell, pipelinesTenant())

	ctrlGVR, err := crdstore.GVRFor[v1alpha1.Controller]()
	if err != nil {
		t.Fatalf("GVRFor[Controller]: %v", err)
	}
	ctx := context.Background()
	// A plain flag, not sync.Once: the hook writes through the same Fake,
	// and that write reads the cell again, so a re-entrant Once would
	// block.
	placed := false
	store.SetAfterGet(ctrlGVR, func(namespace, name string) {
		if !placed {
			placed = true
			if err := WriteCellClaim(ctx, store, name, namespace, "team-a", testClockNow); err != nil {
				t.Errorf("WriteCellClaim: %v", err)
			}
			if err := crdstore.PatchStatus[v1alpha1.Tenant](ctx, store, "team-a", "",
				map[string]any{"placement": map[string]any{
					"cell": name, "cellNamespace": namespace,
				}}); err != nil {
				t.Errorf("write the Tenant fence: %v", err)
			}
		}
	})

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	pool := getPool(t, store, testProfileName)
	if got := r.drainSurplus(ctx, pool, []*v1alpha1.Controller{cell}, 1); got != 0 {
		t.Fatalf("drainSurplus deleted %d cells, want 0", got)
	}
	if !cellExists(t, store, "cell-lts-2-555-1") {
		t.Fatal("the drain deleted a cell the placement pass claimed")
	}
}

// TestCellPool_DrainSkipsAFenceWrittenAfterTheSnapshot covers the second
// stale window. The pass lists the fences once. A tenant whose cell patch
// keeps failing holds a fence but no tenant entry, and its claim can expire
// before the patch lands. The drain must read the fences again after the
// fresh cell read, or it deletes a cell a live fence names.
func TestCellPool_DrainSkipsAFenceWrittenAfterTheSnapshot(t *testing.T) {
	store := crdstore.NewFake()
	// The claim is older than the window, so guard zero lets the cell pass.
	cell := claimedCell("cell-lts-2-555-1", testClockNow.Add(-CellClaimTTL-time.Minute))
	crdstore.MustSeed(store, newProfile(), newPool(0), cell, pipelinesTenant())

	ctrlGVR, err := crdstore.GVRFor[v1alpha1.Controller]()
	if err != nil {
		t.Fatalf("GVRFor[Controller]: %v", err)
	}
	ctx := context.Background()
	fenced := false
	store.SetAfterGet(ctrlGVR, func(namespace, name string) {
		if !fenced {
			fenced = true
			if err := crdstore.PatchStatus[v1alpha1.Tenant](ctx, store, "team-a", "",
				map[string]any{"placement": map[string]any{
					"cell": name, "cellNamespace": namespace,
				}}); err != nil {
				t.Errorf("write the Tenant fence: %v", err)
			}
		}
	})

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	pool := getPool(t, store, testProfileName)
	if got := r.drainSurplus(ctx, pool, []*v1alpha1.Controller{cell}, 1); got != 0 {
		t.Fatalf("drainSurplus deleted %d cells, want 0", got)
	}
	if !cellExists(t, store, "cell-lts-2-555-1") {
		t.Fatal("the drain deleted a cell a fresh fence names")
	}
}

// TestCellPool_DrainSkipsACellWithALiveClaim covers guard zero. A claim the
// fresh read observes keeps the cell, although the cell holds no tenant and
// no fence names it yet.
func TestCellPool_DrainSkipsACellWithALiveClaim(t *testing.T) {
	store := crdstore.NewFake()
	cell := claimedCell("cell-lts-2-555-1", testClockNow.Add(-time.Minute))
	crdstore.MustSeed(store, newProfile(), newPool(0), cell)

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	pool := getPool(t, store, testProfileName)
	if got := r.drainSurplus(context.Background(), pool, []*v1alpha1.Controller{cell}, 1); got != 0 {
		t.Fatalf("drainSurplus deleted %d cells, want 0", got)
	}
	if !cellExists(t, store, "cell-lts-2-555-1") {
		t.Fatal("the drain deleted a cell that carries a live claim")
	}
}

// TestCellPool_AnExpiredClaimDoesNotHoldACell proves the claim window is
// bounded. A placement pass that stops between the claim and the tenant
// patch cannot hold a cell for longer than CellClaimTTL.
func TestCellPool_AnExpiredClaimDoesNotHoldACell(t *testing.T) {
	store := crdstore.NewFake()
	cell := claimedCell("cell-lts-2-555-1", testClockNow.Add(-CellClaimTTL-time.Minute))
	crdstore.MustSeed(store, newProfile(), newPool(0), cell)

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	pool := getPool(t, store, testProfileName)
	if got := r.drainSurplus(context.Background(), pool, []*v1alpha1.Controller{cell}, 1); got != 1 {
		t.Fatalf("drainSurplus deleted %d cells, want 1", got)
	}
	if cellExists(t, store, "cell-lts-2-555-1") {
		t.Fatal("a cell whose claim expired must be drained")
	}
}

// TestCellPool_DeletedPoolDrainSkipsAClaimedCell asserts the delete-path
// drain applies guard zero too. It keeps the finalizer while the claimed
// cell survives.
func TestCellPool_DeletedPoolDrainSkipsAClaimedCell(t *testing.T) {
	store := crdstore.NewFake()
	deleting := newPool(0)
	deletedAt := metav1.NewTime(testClockNow)
	deleting.DeletionTimestamp = &deletedAt
	deleting.Finalizers = []string{v1alpha1.CellPoolFinalizerDrain}
	crdstore.MustSeed(store, newProfile(), deleting,
		claimedCell("cell-lts-2-555-0", testClockNow.Add(-time.Minute)))

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !cellExists(t, store, "cell-lts-2-555-0") {
		t.Fatal("the delete-path drain deleted a claimed cell")
	}
	pool := getPool(t, store, testProfileName)
	if len(pool.Finalizers) == 0 {
		t.Fatal("the pool released the finalizer while a cell survives")
	}
}

// TestWriteCellClaim_RetiresOnRelease asserts the claim round trip: the
// write adds one entry, and the release removes it.
func TestWriteCellClaim_RetiresOnRelease(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, versionedCell("cell-lts-2-555-0", "uid-0", "1", nil))
	ctx := context.Background()

	if err := WriteCellClaim(ctx, store, "cell-lts-2-555-0", testOperatorNamespace, "team-a", testClockNow); err != nil {
		t.Fatalf("WriteCellClaim: %v", err)
	}
	cell, err := crdstore.Get[v1alpha1.Controller](ctx, store, "cell-lts-2-555-0", testOperatorNamespace)
	if err != nil {
		t.Fatalf("get cell: %v", err)
	}
	if !HasLiveCellClaim(cell, testClockNow) {
		t.Fatalf("the cell carries no live claim: %+v", cell.Status.Cell)
	}

	if err := ReleaseCellClaim(ctx, store, "cell-lts-2-555-0", testOperatorNamespace, "team-a", testClockNow); err != nil {
		t.Fatalf("ReleaseCellClaim: %v", err)
	}
	cell, err = crdstore.Get[v1alpha1.Controller](ctx, store, "cell-lts-2-555-0", testOperatorNamespace)
	if err != nil {
		t.Fatalf("get cell: %v", err)
	}
	if HasLiveCellClaim(cell, testClockNow) {
		t.Fatalf("the released claim still holds the cell: %+v", cell.Status.Cell)
	}
}
