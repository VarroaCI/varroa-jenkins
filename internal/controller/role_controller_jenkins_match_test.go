package controller

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestBuiltinRoles_JenkinsPermissionsMatchJenkinsRole(t *testing.T) {
	jenkinsRoles := make(map[string]*v1alpha1.JenkinsRole)
	for _, jr := range builtinJenkinsRoles() {
		jenkinsRoles[jr.Name] = jr
	}

	for _, role := range builtinRoles() {
		ref := role.Spec.JenkinsRoleRef
		if ref == "" {
			t.Fatalf("built-in VarroaRole %s has no JenkinsRoleRef", role.Name)
		}
		jr, ok := jenkinsRoles[ref]
		if !ok {
			t.Fatalf("built-in VarroaRole %s references missing JenkinsRole %s", role.Name, ref)
		}
		if !stringSlicesEqual(role.Spec.JenkinsPermissions, jr.Spec.Permissions) {
			t.Errorf("VarroaRole %s JenkinsPermissions drifted from JenkinsRole %s:\n varroa:  %v\n jenkins: %v",
				role.Name, ref, role.Spec.JenkinsPermissions, jr.Spec.Permissions)
		}
	}
}
