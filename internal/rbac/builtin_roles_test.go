package rbac

import (
	"slices"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestBuiltinJenkinsRoles_CancelMatrix(t *testing.T) {
	byName := make(map[string]*v1alpha1.JenkinsRole)
	for _, r := range BuiltinJenkinsRoles() {
		byName[r.Name] = r
	}

	for _, name := range []string{"varroa-operator", "varroa-developer"} {
		role, ok := byName[name]
		if !ok {
			t.Fatalf("BuiltinJenkinsRoles() missing %s", name)
		}
		if !slices.Contains(role.Spec.Permissions, "hudson.model.Item.Cancel") {
			t.Errorf("%s must grant hudson.model.Item.Cancel, got %v", name, role.Spec.Permissions)
		}
	}

	viewer, ok := byName["varroa-viewer"]
	if !ok {
		t.Fatal("BuiltinJenkinsRoles() missing varroa-viewer")
	}
	if slices.Contains(viewer.Spec.Permissions, "hudson.model.Item.Cancel") {
		t.Errorf("varroa-viewer must not grant hudson.model.Item.Cancel, got %v", viewer.Spec.Permissions)
	}

	admin, ok := byName["varroa-admin"]
	if !ok {
		t.Fatal("BuiltinJenkinsRoles() missing varroa-admin")
	}
	if !slices.Contains(admin.Spec.Permissions, "hudson.model.Hudson.Administer") {
		t.Errorf("varroa-admin must grant hudson.model.Hudson.Administer, got %v", admin.Spec.Permissions)
	}

	for _, role := range BuiltinJenkinsRoles() {
		if err := v1alpha1.ValidateJenkinsRole(role); err != nil {
			t.Errorf("built-in role %s fails validation: %v", role.Name, err)
		}
	}
}
