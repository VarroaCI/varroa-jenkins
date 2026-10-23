package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// searchFanoutMCPBrood reports one controller row per cluster and the
// read status the test sets. It backs the MCP search tests below.
type searchFanoutMCPBrood struct {
	api.Brood
	controllers []api.ClusterController
	statuses    []api.ClusterFanoutStatus
}

func (b *searchFanoutMCPBrood) LocalCluster() string { return "core" }

func (b *searchFanoutMCPBrood) ListAll(_ context.Context, _, _ string) ([]api.ClusterController, []api.ClusterFanoutStatus, error) {
	return b.controllers, b.statuses, nil
}

// searchFanoutMCPOutput decodes the search tool result. Clusters stays a
// raw slice so a test can tell null from an empty array.
type searchFanoutMCPOutput struct {
	Items    []api.SearchResult        `json:"items"`
	Count    int                       `json:"count"`
	Clusters []api.ClusterFanoutStatus `json:"clusters"`
}

func callSearchFanoutMCP(t *testing.T, deps *api.Dependencies, claims *auth.Claims, query string) searchFanoutMCPOutput {
	t.Helper()
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "search",
		"arguments": map[string]interface{}{"query": query},
	}, claims)
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("search returned error: %v", tr.Content)
	}
	var out searchFanoutMCPOutput
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &out); err != nil {
		t.Fatalf("unmarshal search result: %v (%s)", err, tr.Content[0].Text)
	}
	return out
}

func TestMCPSearch_ReportsFailedMemberCluster(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "viewer-binding"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "viewer",
		}},
	}
	deps := newTestDeps()
	deps.Logger = slog.Default()
	deps.Authorizer = api.NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false)
	deps.Brood = &searchFanoutMCPBrood{
		controllers: []api.ClusterController{{Cluster: "core", CR: &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "tenant-core", Namespace: "ns-a"}}}},
		statuses: []api.ClusterFanoutStatus{
			{Name: "core", OK: true},
			{Name: "member-b", OK: false, Error: "cluster unreachable"},
		},
	}

	out := callSearchFanoutMCP(t, deps, &auth.Claims{Subject: "u"}, "tenant")

	var row *api.ClusterFanoutStatus
	for i := range out.Clusters {
		if out.Clusters[i].Name == "member-b" {
			row = &out.Clusters[i]
		}
	}
	if row == nil || row.OK || row.Error != "cluster unreachable" {
		t.Fatalf("clusters = %+v, want a member-b row with ok false", out.Clusters)
	}
	if out.Count != len(out.Items) || out.Count == 0 {
		t.Fatalf("count = %d items = %d, want the core match counted", out.Count, len(out.Items))
	}
}

func TestMCPSearch_NoAuthorizerReturnsEmptyClusters(t *testing.T) {
	deps := newTestDeps()
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "search",
		"arguments": map[string]interface{}{"query": "tenant"},
	}, nil)
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("search returned error: %v", tr.Content)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(raw["clusters"]) != "[]" {
		t.Fatalf("clusters = %s, want []", raw["clusters"])
	}
	if string(raw["items"]) != "[]" {
		t.Fatalf("items = %s, want []", raw["items"])
	}
}

func TestSearchOutputSchema_DeclaresClusters(t *testing.T) {
	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(searchListOutputSchema, &doc); err != nil {
		t.Fatalf("unmarshal search output schema: %v", err)
	}
	if _, ok := doc.Properties["clusters"]; !ok {
		t.Fatalf("search output schema has no clusters property: %s", searchListOutputSchema)
	}
}
