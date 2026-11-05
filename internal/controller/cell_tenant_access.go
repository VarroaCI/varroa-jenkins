package controller

import (
	"context"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
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

// tenantIsDrainingFromCell reports whether tenant drains from cr (design D4,
// tenant-isolation spec). A draining tenant's folder is omitted from the
// merged manifest. cells.TenantDrainsFromCell holds the rule, so the
// placement pass and the controller reconciler read the same one.
func tenantIsDrainingFromCell(tenant *v1alpha1.Tenant, cr *v1alpha1.Controller) bool {
	return cells.TenantDrainsFromCell(&tenant.Status, cr.Namespace, cr.Name)
}
