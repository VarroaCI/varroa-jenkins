package controller

import (
	"testing"
)

// TestBuiltinRoles_JenkinsRoleRefOnly proves each built-in VarroaRole names
// its JenkinsRole and carries no inline Jenkins permissions. The JenkinsRole
// holds the permission set, so a second copy on the VarroaRole could drift.
func TestBuiltinRoles_JenkinsRoleRefOnly(t *testing.T) {
	jenkinsRoles := make(map[string]bool)
	for _, jr := range builtinJenkinsRoles() {
		jenkinsRoles[jr.Name] = true
	}

	for _, role := range builtinRoles() {
		ref := role.Spec.JenkinsRoleRef
		if ref == "" {
			t.Fatalf("built-in VarroaRole %s has no JenkinsRoleRef", role.Name)
		}
		if !jenkinsRoles[ref] {
			t.Fatalf("built-in VarroaRole %s references missing JenkinsRole %s", role.Name, ref)
		}
		if len(role.Spec.JenkinsPermissions) != 0 {
			t.Errorf("built-in VarroaRole %s carries inline JenkinsPermissions %v, want none; JenkinsRole %s holds them",
				role.Name, role.Spec.JenkinsPermissions, ref)
		}
	}
}
