package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
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
func (r *Reconciler) updateCellInstance(ctx context.Context, cr *v1alpha1.Controller) {
	pod, err := r.client.GetControllerPod(ctx, cr.Namespace, cr.Name)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("get controller pod for cell instance",
				"controller", cr.Namespace+"/"+cr.Name, "error", err)
		}
		return
	}
	instance := ""
	if pod != nil && pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded {
		instance = string(pod.UID)
	}
	if cr.Status.Cell == nil {
		if instance == "" {
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
	cr.Status.Cell.Instance = instance
}
