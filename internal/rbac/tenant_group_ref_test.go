package rbac

import (
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestGenerate_TenantManagedGroupReferenceRendered asserts the role-strategy
// output names the managed Group tenant-<name> and no legacy team- name.
func TestGenerate_TenantManagedGroupReferenceRendered(t *testing.T) {
	controller := memberRoleTestController()
	tenant := memberRoleTestTenant("developer", v1alpha1.SubjectRef{Kind: "Group", Name: "sbsoftware:Owners"})
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, nil, []*v1alpha1.Tenant{tenant}, []*v1alpha1.Controller{controller})

	yamlOut, err := NewGenerator(r).Generate(controller)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(yamlOut, "tenant-sbsoftware") {
		t.Fatalf("expected managed group reference tenant-sbsoftware in YAML, got:\n%s", yamlOut)
	}
	if strings.Contains(yamlOut, "team-") {
		t.Fatalf("expected no team- name in YAML, got:\n%s", yamlOut)
	}
}
