package controller

import (
	"sort"
	"strings"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// itemsApplyFailureWarnings answers the ItemsApplyFailed entries for one
// desired-state result. The mite sets ItemFailures only when its items apply
// walked every item, so the list covers every tenant of the cell. Each tenant
// gets one warning that names only its own refused items. A failure whose path
// no tenant of the cell owns answers no entry.
func itemsApplyFailureWarnings(cell *v1alpha1.CellStatus, d *mitev1.DesiredStateResult) []v1alpha1.TenantWarning {
	if cell == nil || d == nil || d.ItemsSuccess || len(d.ItemFailures) == 0 {
		return nil
	}
	errorsByTenant := make(map[string][]string)
	for _, f := range d.ItemFailures {
		for tenant := range partitionJobPathsByTenant([]string{f.Path}, cell.Tenants) {
			errorsByTenant[tenant] = append(errorsByTenant[tenant], f.Error)
		}
	}
	tenants := make([]string, 0, len(errorsByTenant))
	for tenant := range errorsByTenant {
		tenants = append(tenants, tenant)
	}
	sort.Strings(tenants)
	var out []v1alpha1.TenantWarning
	for _, tenant := range tenants {
		out = append(out, v1alpha1.TenantWarning{
			Tenant:  tenant,
			Reason:  v1alpha1.TenantReasonItemsApplyFailed,
			Message: truncate(strings.Join(errorsByTenant[tenant], "; "), 1024),
		})
	}
	return out
}

// itemFailurePaths answers the item path of each refused item.
func itemFailurePaths(d *mitev1.DesiredStateResult) []string {
	if d == nil {
		return nil
	}
	paths := make([]string, 0, len(d.ItemFailures))
	for _, f := range d.ItemFailures {
		paths = append(paths, f.Path)
	}
	return paths
}

// itemsApplyFailedAfter returns the TenantWarnings list after one
// desired-state result. A successful items section clears every
// ItemsApplyFailed entry. A result with refused items replaces every
// ItemsApplyFailed entry with the fresh per-tenant set, because the walk
// covered every tenant. The fresh set also clears a tenant whose items now
// apply. Any other result keeps the list unchanged.
func itemsApplyFailedAfter(existing []v1alpha1.TenantWarning, cell *v1alpha1.CellStatus, d *mitev1.DesiredStateResult) []v1alpha1.TenantWarning {
	if d == nil {
		return existing
	}
	if d.ItemsSuccess {
		return replaceTenantWarnings(existing, v1alpha1.TenantReasonItemsApplyFailed, nil)
	}
	if len(d.ItemFailures) == 0 {
		return existing
	}
	return replaceTenantWarnings(existing, v1alpha1.TenantReasonItemsApplyFailed, itemsApplyFailureWarnings(cell, d))
}
