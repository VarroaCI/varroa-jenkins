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
