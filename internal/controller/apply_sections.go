package controller

import "github.com/varroaci/varroa-jenkins/api/v1alpha1"

// applySectionRBAC is the name of the rbac section of an ApplyResult. The
// operator generates that section from the cluster RBAC objects, not from
// the ComposedBundle.
const applySectionRBAC = "rbac"

// bundleSectionsApplied reports whether ar applied every section that the
// bundle feeds. The rbac section is not a bundle section, so a refused
// rbac.yaml does not count against the bundle. A result with no failed
// section and Succeeded false is not applied.
func bundleSectionsApplied(ar *v1alpha1.ApplyResult) bool {
	if ar == nil {
		return false
	}
	if ar.Succeeded {
		return true
	}
	rbacFailed := false
	for _, s := range ar.Sections {
		if s.OK {
			continue
		}
		if s.Name != applySectionRBAC {
			return false
		}
		rbacFailed = true
	}
	return rbacFailed
}

// rbacRefusedFor reports whether ar is the verdict for desiredHash and Jenkins
// refused its rbac.yaml. A refusal carries error text. A failed section with
// no error text means that Jenkins was not ready, which is not a verdict.
func rbacRefusedFor(ar *v1alpha1.ApplyResult, desiredHash string) bool {
	if ar == nil || desiredHash == "" || ar.Hash != desiredHash {
		return false
	}
	for _, s := range ar.Sections {
		if s.Name == applySectionRBAC {
			return !s.OK && s.Error != ""
		}
	}
	return false
}
