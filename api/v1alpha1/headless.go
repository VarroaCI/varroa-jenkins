package v1alpha1

import "strings"

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
	if IsCell(cr) {
		return true
	}
	return cr.Spec.Headless
}

// HeadlessLocationURL returns the Jenkins location URL of a headless
// controller: the dashboard door under origin. It returns "" when any
// argument is empty, so the caller keeps the in-cluster endpoint.
//
// This is the single place that builds this value. Every consumer (the
// operator's location injection, break-glass, and the diff mirror) calls it.
func HeadlessLocationURL(origin, cluster, namespace, name string) string {
	if origin == "" || cluster == "" || namespace == "" || name == "" {
		return ""
	}
	return strings.TrimRight(origin, "/") + "/headless/" + cluster + "/" + namespace + "/" + name + "/"
}
