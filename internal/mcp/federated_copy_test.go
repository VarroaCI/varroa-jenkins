package mcp

import (
	"context"
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
