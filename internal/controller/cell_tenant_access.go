package controller

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// listPlacedTenants reads each Tenant named in cr.Status.Cell.Tenants, once per
// tick (design D7). A missing Tenant is skipped and logged: it does not fail
// the tick, and it leaves every other tenant unaffected. The controller
// reconciler needs each Tenant's spec.pipelines for the bundle reference and
// the agent namespace, and each Tenant's Placed condition for the drain
// filter (design D4).
func (r *Reconciler) listPlacedTenants(ctx context.Context, cr *v1alpha1.Controller) []*v1alpha1.Tenant {
	if cr.Status.Cell == nil {
		return nil
	}
	tenants := make([]*v1alpha1.Tenant, 0, len(cr.Status.Cell.Tenants))
	for _, name := range cr.Status.Cell.Tenants {
		tenant, err := crdstore.Get[v1alpha1.Tenant](ctx, r.store, name, "")
		if err != nil {
			if r.Logger != nil {
				r.Logger.Warn("cell content: tenant Tenant not found", "tenant", name,
					"controller", cr.Namespace+"/"+cr.Name, "error", err)
			}
			continue
		}
		tenants = append(tenants, tenant)
	}
	return tenants
}

// findTenantCondition returns the named condition, or nil when absent.
func findTenantCondition(conds []v1alpha1.TenantCondition, condType string) *v1alpha1.TenantCondition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

// tenantIsDrainingFromCell reports whether tenant is draining away from cr
// (design D4, tenant-isolation spec). A voluntary move drains every member
// cell: its Placed condition is False with reason TenantMoving, and its
// placement still names cr. A member the pass releases drains its own cell
// alone, through the member's draining flag. A draining tenant's folder is
// omitted from the merged manifest, which is what makes the voluntary
// move's idle test and a member's release converge.
func tenantIsDrainingFromCell(tenant *v1alpha1.Tenant, cr *v1alpha1.Controller) bool {
	p := tenant.Status.Placement
	if p == nil {
		return false
	}
	cond := findTenantCondition(tenant.Status.Conditions, v1alpha1.TenantConditionPlaced)
	moving := cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == v1alpha1.TenantReasonTenantMoving
	for _, m := range p.Members {
		if m.Cell == cr.Name && m.CellNamespace == cr.Namespace {
			return moving || m.Draining
		}
	}
	return moving && p.Cell == cr.Name && p.CellNamespace == cr.Namespace
}
