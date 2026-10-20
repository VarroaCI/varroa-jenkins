package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// updateCellInstance writes Controller.status.cell.instance from the
// running Jenkins pod's metadata.uid (design D13, task 6.0). Every
// controller-reconciler tick calls it, cell or not: a controller with no
// cell role reads this same field for its own tenant.sync self-
// announcement (announceSingleTenantPlacement).
//
// It clears the field only on a confirmed absence (no error, no pod). A
// transient read error leaves the field at its last known value rather
// than clobbering it with an empty one.
//
// A pod in phase Failed or Succeeded counts as no pod. An exited Jenkins
// process serves no build, whatever its pod object still reports. A pod
// in phase Running or Pending never clears the field.
//
// When the value changes away from a non-empty instance, that instance
// moves to status.cell.previousInstance with the mite's last heartbeat
// time, so the single-tenant announcement can report the replaced pod as
// a dead instance. A change from empty keeps the earlier record.
//
// Any change also drops each held or refused tenant from
// status.cell.appliedTenants. The new pod has an empty home, so their folders
// are gone.
//
// After a successful read, the parked desired-state result for the controller
// replays when it came from the instance just read and still matches the
// tracked hash. The replay runs after the drop, so the result re-adds the
// tenants that the new pod applied. A failed read keeps the parked result for
// the next tick.
func (r *Reconciler) updateCellInstance(ctx context.Context, cr *v1alpha1.Controller) {
	key := cr.Namespace + "/" + cr.Name
	pod, err := r.client.GetControllerPod(ctx, cr.Namespace, cr.Name)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("get controller pod for cell instance",
				"controller", key, "error", err)
		}
		return
	}
	instance := ""
	if pod != nil && pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded {
		instance = string(pod.UID)
	}
	if cr.Status.Cell == nil {
		if instance == "" {
			r.takeParkedCellResult(key)
			return
		}
		cr.Status.Cell = &v1alpha1.CellStatus{}
	}
	if old := cr.Status.Cell.Instance; old != "" && old != instance {
		cr.Status.Cell.PreviousInstance = old
		lastSeen := metav1.Now()
		if cr.Status.MiteStatus != nil && cr.Status.MiteStatus.LastSeen != nil {
			lastSeen = *cr.Status.MiteStatus.LastSeen
		}
		cr.Status.Cell.PreviousInstanceLastSeen = &lastSeen
	}
	if cr.Status.Cell.Instance != instance {
		dropKeptTenants(cr.Status.Cell)
	}
	cr.Status.Cell.Instance = instance
	if parked := r.takeParkedCellResult(key); parked != nil &&
		isCellController(cr) && parked.CellInstance == instance &&
		parked.AppliedHash != "" && parked.AppliedHash == cr.Status.DesiredStateHash {
		applyCellItemsResult(cr, parked)
	}
}

// applyCellItemsResult applies the items walk of a desired-state result to
// status.cell. The caller has checked that the result is for a cell
// controller, that its hash matches the tracked desired-state hash, and that
// it came from the current instance. An items apply that walked every item
// (with or without refusals) advances appliedTenants. Every items apply
// replaces the ItemsApplyFailed warnings.
func applyCellItemsResult(cr *v1alpha1.Controller, d *mitev1.DesiredStateResult) {
	if cr.Status.Cell == nil {
		cr.Status.Cell = &v1alpha1.CellStatus{}
	}
	if d.ItemsSuccess || len(d.ItemFailures) > 0 {
		cr.Status.Cell.AppliedTenants = appliedTenantsAfterItemsApply(cr.Status.Cell, itemFailurePaths(d))
	}
	cr.Status.Cell.TenantWarnings = itemsApplyFailedAfter(cr.Status.Cell.TenantWarnings, cr.Status.Cell, d)
}

// parkCellResult stores a result that the drain could not apply because it
// came from another instance. One slot exists per controller key; a newer
// result replaces an older one. The drained result is not shared with the
// transport after the drain, so the pointer is stored as is.
func (r *Reconciler) parkCellResult(key string, d *mitev1.DesiredStateResult) {
	r.cellResultMu.Lock()
	defer r.cellResultMu.Unlock()
	if r.parkedCellResults == nil {
		r.parkedCellResults = make(map[string]*mitev1.DesiredStateResult)
	}
	r.parkedCellResults[key] = d
}

// takeParkedCellResult removes and returns the parked result for a key, or nil.
func (r *Reconciler) takeParkedCellResult(key string) *mitev1.DesiredStateResult {
	r.cellResultMu.Lock()
	defer r.cellResultMu.Unlock()
	d := r.parkedCellResults[key]
	delete(r.parkedCellResults, key)
	return d
}

// cellResultFromCurrentInstance reports whether a desired-state result came
// from the pod that status.cell.instance names. A result from a replaced pod
// must not change the applied tenant set or the refused tenant warnings.
func cellResultFromCurrentInstance(cr *v1alpha1.Controller, d *mitev1.DesiredStateResult) bool {
	current := ""
	if cr.Status.Cell != nil {
		current = cr.Status.Cell.Instance
	}
	return d.CellInstance == current
}
