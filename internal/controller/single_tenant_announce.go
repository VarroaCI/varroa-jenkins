package controller

import (
	"context"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
)

// announceSingleTenantPlacement sends the C6 tenant.sync "announce"
// request for a controller that sets no cell role (design D6, placement-
// fencing spec): the fence needs a row for every claim, with no exemption,
// and a single-tenant controller is its own tenant and its own cell. It
// runs on every reconcile tick, before this tick's own desired-state
// build, so the fence never blocks a legitimate claim on a missing row.
// The upsert is idempotent, so the repeat costs one request.
//
// The tenant name matches the gateway resolver's own stamp for a
// non-cell claim (internal/mite/tenant_resolver.go): "<namespace>/<name>".
// The generation stays 0, so the recorder's generation-zero branch seeds
// rehydrated_instance from the announced cell instance at once — a
// single-tenant controller's own pod is its own seed.
func (r *Reconciler) announceSingleTenantPlacement(ctx context.Context, cr *v1alpha1.Controller) {
	if isCellController(cr) || r.announce == nil {
		return
	}

	now := time.Now()
	instance := ""
	var dead []cells.DeadInstance
	if cr.Status.Cell != nil {
		instance = cr.Status.Cell.Instance
		// A replaced pod of a single-tenant controller is always dead:
		// the same StatefulSet never runs two instances. Report it on
		// every tick; the recorder's merge is idempotent.
		//
		// The entry stands behind updateCellInstance's own pod read, so
		// it carries this tick's clock as the confirmation time. The
		// recorder keeps the earliest such value.
		if prev := cr.Status.Cell.PreviousInstance; prev != "" && prev != instance {
			seen := now
			if cr.Status.Cell.PreviousInstanceLastSeen != nil {
				seen = cr.Status.Cell.PreviousInstanceLastSeen.Time
			}
			confirmed := now
			dead = []cells.DeadInstance{{Instance: prev, LastSeenAt: seen, ConfirmedGoneAt: &confirmed}}
		}
	}
	connected := false
	var lastSeen *time.Time
	if cr.Status.MiteStatus != nil {
		connected = cr.Status.MiteStatus.Connected
		lastSeen = timeFromMetaV1(cr.Status.MiteStatus.LastSeen)
	}

	_, err := r.announce(ctx, cells.AnnounceRequest{
		Cluster:            r.clusterName,
		Tenant:             cr.Namespace + "/" + cr.Name,
		Incarnation:        string(cr.UID),
		Namespace:          cr.Namespace,
		Cell:               cr.Name,
		Generation:         0,
		CellInstance:       instance,
		RehydratedInstance: instance,
		CellAlive:          connected,
		CellAliveAt:        &now,
		CellLastSeenAt:     lastSeen,
		DeadInstances:      dead,
	})
	if err != nil {
		r.Logger.Warn("tenant sync announce failed", "controller", cr.Namespace+"/"+cr.Name, "error", err)
	}
}
