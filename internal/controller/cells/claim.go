package cells

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// CellClaimTTL bounds how long one placement claim holds a cell against a
// drain. The placement pass converts a claim into a tenant inside the same
// tick, so the window only covers a pass that stops between the two writes.
// After it, the drain treats the cell as free again.
const CellClaimTTL = 10 * time.Minute

// WriteCellClaim records tenant's claim on a cell. The placement pass calls
// it before it writes the Team fence.
//
// The claim closes this interleaving: the pool drain lists a cell as free,
// reads no fence, and re-reads the cell; the placement pass then writes the
// fence and patches the tenant list; the drain, holding the earlier read,
// deletes the cell the tenant now runs on. The fence alone cannot close it,
// because the drain reads the fence set before it re-reads the cell. The
// claim lands on the cell itself, so it either moves the resourceVersion the
// drain's precondition carries, or the drain's own fresh read observes it.
func WriteCellClaim(ctx context.Context, store crdstore.Backend, name, namespace, tenant string, now time.Time) error {
	return setCellClaim(ctx, store, name, namespace, tenant, true, now)
}

// ReleaseCellClaim removes tenant's claim from a cell. The placement pass
// calls it after the tenant patch, which is the durable record the claim
// stands in for.
func ReleaseCellClaim(ctx context.Context, store crdstore.Backend, name, namespace, tenant string, now time.Time) error {
	return setCellClaim(ctx, store, name, namespace, tenant, false, now)
}

// setCellClaim reads the cell, rebuilds its claim list, and writes back a
// narrow merge patch of status.cell.pendingTenants. It drops every expired
// claim on the way, so a cell cannot collect claim garbage.
func setCellClaim(ctx context.Context, store crdstore.Backend, name, namespace, tenant string, hold bool, now time.Time) error {
	live, err := crdstore.Get[v1alpha1.Controller](ctx, store, name, namespace)
	if err != nil {
		return err
	}
	next := make([]v1alpha1.CellPendingTenant, 0, 4)
	if live.Status.Cell != nil {
		for _, claim := range live.Status.Cell.PendingTenants {
			if claim.Tenant == tenant || !claimIsLive(claim, now) {
				continue
			}
			next = append(next, claim)
		}
	}
	if hold {
		next = append(next, v1alpha1.CellPendingTenant{
			Tenant:    tenant,
			ClaimedAt: metav1.NewTime(now),
		})
	}
	// The patch names status.cell.pendingTenants alone. A wider status body
	// would clobber the keys the controller reconciler owns.
	return crdstore.PatchStatus[v1alpha1.Controller](ctx, store, name, namespace,
		map[string]any{"cell": map[string]any{"pendingTenants": next}})
}

// HasLiveCellClaim reports whether a cell carries a claim that has not
// expired.
func HasLiveCellClaim(cell *v1alpha1.Controller, now time.Time) bool {
	if cell == nil || cell.Status.Cell == nil {
		return false
	}
	for _, claim := range cell.Status.Cell.PendingTenants {
		if claimIsLive(claim, now) {
			return true
		}
	}
	return false
}

// claimIsLive reports whether one claim is inside the claim window.
func claimIsLive(claim v1alpha1.CellPendingTenant, now time.Time) bool {
	if claim.ClaimedAt.IsZero() {
		return false
	}
	return now.Sub(claim.ClaimedAt.Time) < CellClaimTTL
}
