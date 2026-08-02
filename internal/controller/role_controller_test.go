package controller

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestBuiltinJenkinsRoles_IncludesSystemOperator(t *testing.T) {
	roles := builtinJenkinsRoles()

	var found *v1alpha1.JenkinsRole
	for _, r := range roles {
		if r.Name == "varroa-system-operator" {
			found = r
			break
		}
	}
	if found == nil {
		t.Fatal("expected builtinJenkinsRoles() to include varroa-system-operator")
	}
	if found.Spec.RoleType != "Global" {
		t.Errorf("expected RoleType=Global, got %s", found.Spec.RoleType)
	}
	if len(found.Spec.Permissions) != 1 || found.Spec.Permissions[0] != "hudson.model.Hudson.Administer" {
		t.Errorf("expected exactly [hudson.model.Hudson.Administer], got %v", found.Spec.Permissions)
	}
	if found.Labels[v1alpha1.LabelBuiltin] != "true" {
		t.Errorf("expected LabelBuiltin=true, got %v", found.Labels)
	}
}
