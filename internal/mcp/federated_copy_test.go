package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// TestUpdateJenkinsRole_RefusesFederatedCopy pins that the MCP update tool
// refuses a federated copy and writes nothing.
func TestUpdateJenkinsRole_RefusesFederatedCopy(t *testing.T) {
	deps := adminDeps()
	withLocalOperator(deps)
	store := deps.Store.(*crdstore.Fake)
	crdstore.MustSeed(store, &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{
		Name:        "r1",
		Labels:      map[string]string{rbac.LabelFederatedFrom: "alpha"},
		Annotations: map[string]string{rbac.AnnotationFederatedRoles: "alpha"},
	}})

	tr := callToolAs(t, deps, mcpAdminClaims, "update_jenkins_role", map[string]interface{}{"name": "r1", "description": "edited"})
	if !tr.IsError {
		t.Fatalf("update_jenkins_role wrote a federated copy: %v", tr.Content)
	}
	if !strings.Contains(toolText(tr), "is a federated copy of") {
		t.Fatalf("error %q, want the federated copy text", toolText(tr))
	}
	got, err := crdstore.Get[v1alpha1.JenkinsRole](context.Background(), store, "r1", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Description != "" {
		t.Errorf("description = %q, want the stored value unchanged", got.Spec.Description)
	}
}

// TestUpdateJenkinsRoleBinding_RefusesFederatedCopy pins that the MCP binding
// update tool refuses a federated copy and writes nothing.
func TestUpdateJenkinsRoleBinding_RefusesFederatedCopy(t *testing.T) {
	deps := adminDeps()
	withLocalOperator(deps)
	store := deps.Store.(*crdstore.Fake)
	crdstore.MustSeed(store, &v1alpha1.JenkinsRoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name:        "b1",
		Labels:      map[string]string{rbac.LabelFederatedFrom: "writer"},
		Annotations: map[string]string{rbac.AnnotationFederatedBindings: "bind-a"},
	}, Spec: v1alpha1.JenkinsRoleBindingSpec{RoleRef: "viewer"}})

	tr := callToolAs(t, deps, mcpAdminClaims, "update_jenkins_role_binding", map[string]interface{}{"name": "b1", "roleRef": "admin"})
	if !tr.IsError {
		t.Fatalf("update_jenkins_role_binding wrote a federated copy: %v", tr.Content)
	}
	if !strings.Contains(toolText(tr), "is a federated copy of VarroaRoleBinding bind-a") {
		t.Fatalf("error %q, want the federated copy text", toolText(tr))
	}
	got, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](context.Background(), store, "b1", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.RoleRef != "viewer" {
		t.Errorf("roleRef = %q, want viewer", got.Spec.RoleRef)
	}
}

// TestJenkinsRBACTools_DeclareNoMetadataArgs pins that the Jenkins RBAC tools
// declare no metadata argument. addTool refuses an argument that the schema
// does not declare, so the MCP path cannot set the federation label. A new
// metadata argument needs the BFF label guard first.
func TestJenkinsRBACTools_DeclareNoMetadataArgs(t *testing.T) {
	want := map[string]bool{
		"create_jenkins_role":         false,
		"update_jenkins_role":         false,
		"create_jenkins_role_binding": false,
		"update_jenkins_role_binding": false,
	}
	for _, tool := range liveTools(t) {
		if _, ok := want[tool.Name]; !ok {
			continue
		}
		want[tool.Name] = true
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("%s: decode input schema: %v", tool.Name, err)
		}
		for _, arg := range []string{"labels", "annotations", "metadata"} {
			if _, ok := schema.Properties[arg]; ok {
				t.Errorf("%s declares argument %q, want none", tool.Name, arg)
			}
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tool %s not listed", name)
		}
	}
}
