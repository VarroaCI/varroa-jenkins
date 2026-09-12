package mcp

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// fakeSearchRecordStore is a minimal recordstore.Reader that returns a
// fixed SearchRecords result. It exists so the MCP "search" tool test below
// exercises only the per-row tenant authorization the handler applies, not
// a real SQL query.
type fakeSearchRecordStore struct {
	recordstore.Reader
	results recordstore.SearchResults
}

func (f *fakeSearchRecordStore) SearchRecords(_ context.Context, _ recordstore.SearchQuery) (recordstore.SearchResults, error) {
	return f.results, nil
}

// fakeSearchBrood is a minimal api.Brood whose controller fan-out is
// always empty, so the MCP "search" tool test below isolates the
// record-store tenant check from the controller/namespace search kinds.
type fakeSearchBrood struct {
	api.Brood
}

func (f *fakeSearchBrood) LocalCluster() string { return "local" }

func (f *fakeSearchBrood) ListAll(_ context.Context, _, _ string) ([]api.ClusterController, []api.ClusterFanoutStatus, error) {
	return nil, nil, nil
}

// TestMCPSearch_RunRowsFilteredByTenantScope proves the MCP "search" tool's
// "run" kind applies CanReadRun per row (D8's replacement for the seam
// move-activity-to-recordstore shipped): a caller holding only a
// tenant-scoped pipelines:read grant on "team-a" sees the run tenanted
// "team-a" and not the run tenanted "team-b".
func TestMCPSearch_RunRowsFilteredByTenantScope(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "reader"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "team-a-binding"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "reader",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Tenants: []v1alpha1.TenantScope{{Name: "team-a"}},
			},
		}},
	}
	resolver := rbac.NewTestResolverWithRoles(roles, bindings)
	authorizer := api.NewAuthorizer(resolver, false)

	deps := newTestDeps()
	deps.Authorizer = authorizer
	deps.Brood = &fakeSearchBrood{}
	deps.RecordStore = &fakeSearchRecordStore{
		results: recordstore.SearchResults{
			Runs: []recordstore.RunSearchResult{
				{RunID: "run-a", Cluster: "local", Namespace: "team-a", Tenant: "team-a", JobPath: "team-a/main"},
				{RunID: "run-b", Cluster: "local", Namespace: "team-b", Tenant: "team-b", JobPath: "team-b/main"},
			},
		},
	}

	handler := NewHandler(deps)
	claims := &auth.Claims{Subject: "u"}
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "search",
		"arguments": map[string]interface{}{
			"query": "main",
		},
	}, claims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("search returned error: %v", tr.Content)
	}
	if len(tr.Content) == 0 {
		t.Fatalf("search returned no content")
	}
	var out struct {
		Count int                `json:"count"`
		Items []api.SearchResult `json:"items"`
	}
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &out); err != nil {
		t.Fatalf("unmarshal search results: %v", err)
	}

	var runs []api.SearchResult
	for _, item := range out.Items {
		if item.Type == "run" {
			runs = append(runs, item)
		}
	}
	if len(runs) != 1 {
		t.Fatalf("expected exactly one visible run, got %d: %+v", len(runs), runs)
	}
	if runs[0].Namespace != "team-a" {
		t.Errorf("expected the visible run to belong to team-a, got %+v", runs[0])
	}
}
