package v1alpha1

// IsCell reports whether cr runs as a cell: one disposable Jenkins JVM that
// hosts several pipelines tenants as top-level folders.
//
// This is the single answer to that question. A second copy of the
// spec.cell.enabled test drifts from this one, the same way a direct read of
// spec.headless drifts from IsHeadless.
func IsCell(cr *Controller) bool {
	if cr == nil {
		return false
	}
	return cr.Spec.Cell != nil && cr.Spec.Cell.Enabled
}

// CellHoldsTenant reports whether the cell cr holds tenant: its
// status.cell.tenantWarnings carries an ItemsHeld entry for tenant.
// A held tenant's declared jobs are not the cell's jobs, so a read
// of the tenant's definition declares no job while this is true.
func CellHoldsTenant(cr *Controller, tenant string) bool {
	if cr == nil || cr.Status.Cell == nil || tenant == "" {
		return false
	}
	for _, w := range cr.Status.Cell.TenantWarnings {
		if w.Tenant == tenant && w.Reason == TeamReasonItemsHeld {
			return true
		}
	}
	return false
}
