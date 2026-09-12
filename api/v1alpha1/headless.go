package v1alpha1

// IsHeadless reports whether the Jenkins web UI is blocked for non-admins.
// A cell is always headless; a one-tenant controller opts in.
//
// This is the single answer to that question. Reading spec.headless or
// spec.cell.enabled directly anywhere else reintroduces the drift that the
// EffectiveBundleRef rule already forbids for the bundle question.
func IsHeadless(cr *Controller) bool {
	if cr == nil {
		return false
	}
	if cr.Spec.Cell != nil && cr.Spec.Cell.Enabled {
		return true
	}
	return cr.Spec.Headless
}
