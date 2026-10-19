package controller

import (
	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// itemsApplyFailureWarnings answers the ItemsApplyFailed entries for one
// desired-state result. A successful items section answers none. A failure
// whose path no tenant of the cell owns answers none.
func itemsApplyFailureWarnings(cell *v1alpha1.CellStatus, d *mitev1.DesiredStateResult) []v1alpha1.TenantWarning {
	if cell == nil || d == nil || d.ItemsSuccess || d.ItemsFailedPath == "" {
		return nil
	}
	byTenant := partitionJobPathsByTenant([]string{d.ItemsFailedPath}, cell.Tenants)
	var out []v1alpha1.TenantWarning
	for tenant := range byTenant {
		out = append(out, v1alpha1.TenantWarning{
			Tenant:  tenant,
			Reason:  v1alpha1.TeamReasonItemsApplyFailed,
			Message: truncate(d.ItemsError, 1024),
		})
	}
	return out
}

// itemsApplyFailedAfter returns the TenantWarnings list after one
// desired-state result. A successful items section clears every
// ItemsApplyFailed entry. A failure that names a tenant-owned item replaces
// only that tenant's entry, because the engine stops at the first failure
// and never reached the other tenants. Any other result keeps the list
// unchanged.
func itemsApplyFailedAfter(existing []v1alpha1.TenantWarning, cell *v1alpha1.CellStatus, d *mitev1.DesiredStateResult) []v1alpha1.TenantWarning {
	if d == nil {
		return existing
	}
	if d.ItemsSuccess {
		return replaceTenantWarnings(existing, v1alpha1.TeamReasonItemsApplyFailed, nil)
	}
	fresh := itemsApplyFailureWarnings(cell, d)
	if len(fresh) == 0 {
		return existing
	}
	tenant := fresh[0].Tenant
	kept := make([]v1alpha1.TenantWarning, 0, len(existing)+len(fresh))
	for _, w := range existing {
		if w.Reason == v1alpha1.TeamReasonItemsApplyFailed && w.Tenant == tenant {
			continue
		}
		kept = append(kept, w)
	}
	return append(kept, fresh...)
}
