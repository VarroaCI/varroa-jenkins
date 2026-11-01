package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// fleetReader serves the fleet reads over a fixed set of runs. Every other
// method comes from the embedded fake.
type fleetReader struct {
	*fakePipelineReader
	runs []recordstore.Run
}

func (f *fleetReader) ListAllTenantPlacements(context.Context) ([]recordstore.TenantPlacement, error) {
	return nil, nil
}

func (f *fleetReader) ListAllFleetLocks(context.Context) ([]recordstore.FleetLock, error) {
	return nil, nil
}

// ListRunsAcross answers the runs of the requested scopes, newest first.
func (f *fleetReader) ListRunsAcross(_ context.Context, q recordstore.RunsAcrossQuery) (recordstore.RunPage, error) {
	var out []recordstore.Run
	for _, run := range f.runs {
		for _, scope := range q.Scopes {
			if run.Cluster == scope.Cluster && run.Tenant == scope.Tenant {
				out = append(out, run)
				break
			}
		}
	}
	return recordstore.RunPage{Runs: out}, nil
}

// RunsOverview answers zero counts for each requested scope.
func (f *fleetReader) RunsOverview(_ context.Context, q recordstore.RunsOverviewQuery) (recordstore.RunsOverview, error) {
	out := recordstore.RunsOverview{WindowStart: q.Now.Add(-24 * time.Hour)}
	for _, scope := range q.Scopes {
		out.Scopes = append(out.Scopes, recordstore.ScopeOverview{Cluster: scope.Cluster, Tenant: scope.Tenant})
	}
	return out, nil
}

// fleetPipelineTenant builds a placed pipelines Tenant on the core cluster.
func fleetPipelineTenant(name string) *v1alpha1.Tenant {
	return &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.TenantSpec{Pipelines: &v1alpha1.TenantPipelinesSpec{}},
		Status: v1alpha1.TenantStatus{Placement: &v1alpha1.TenantPlacementStatus{
			Cluster: "core", Cell: "cell-" + name, CellNamespace: "ns", Generation: 1,
		}},
	}
}

// fleetDeps builds the dependencies of the fleet tools: two placed tenants,
// pay and ops, each with one run, and the caller reads both.
func fleetDeps(t *testing.T) *api.Dependencies {
	t.Helper()
	store := crdstore.NewFake()
	crdstore.MustSeed(store, fleetPipelineTenant("pay"))
	crdstore.MustSeed(store, fleetPipelineTenant("ops"))
	success := "SUCCESS"
	reader := &fleetReader{
		fakePipelineReader: newFakePipelineReader(),
		runs: []recordstore.Run{
			{RunID: "0192f000-0000-7000-8000-000000000001", Cluster: "core", Tenant: "pay", Namespace: "ns",
				Cell: "cell-pay", JobPath: "pay/api", Status: "COMPLETED", Result: &success, CreatedAt: time.Now().UTC()},
			{RunID: "0192f000-0000-7000-8000-000000000002", Cluster: "core", Tenant: "ops", Namespace: "ns",
				Cell: "cell-ops", JobPath: "ops/deploy", Status: "COMPLETED", Result: &success, CreatedAt: time.Now().UTC()},
		},
	}
	deps := &api.Dependencies{
		Store:       store,
		Authorizer:  fleetAuthorizer(),
		RecordStore: reader,
	}
	return deps
}

// fleetAuthorizer grants pipelines read on the tenants pay and ops.
func fleetAuthorizer() *api.Authorizer {
	roles := []*v1alpha1.VarroaRole{{
		ObjectMeta: metav1.ObjectMeta{Name: "reader"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"pipelines"}, Verbs: []string{"read"}},
		}},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "reader",
			Scope: &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{
				{Name: "pay"}, {Name: "ops"},
			}},
		},
	}}
	return api.NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false)
}

func TestListRunsRowsCarryNoTitle(t *testing.T) {
	deps := fleetDeps(t)
	tr := callPipelineTool(t, deps, "list_runs", map[string]interface{}{})
	if tr.IsError {
		t.Fatalf("list_runs returned error: %v", tr.Content)
	}
	var payload struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &payload); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("items = %d; want 2", len(payload.Items))
	}
	for i, item := range payload.Items {
		if _, ok := item["title"]; ok {
			t.Errorf("item %d carries a title field", i)
		}
	}
}

func TestRunListOutputSchemaHasNoTitle(t *testing.T) {
	var schema struct {
		Properties struct {
			Items struct {
				Items struct {
					AnyOf []struct {
						Properties map[string]json.RawMessage `json:"properties"`
					} `json:"anyOf"`
				} `json:"items"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(runListOutputSchema, &schema); err != nil {
		t.Fatalf("unmarshal runListOutputSchema: %v", err)
	}
	if len(schema.Properties.Items.Items.AnyOf) == 0 {
		t.Fatal("runListOutputSchema has no anyOf branches")
	}
	props := schema.Properties.Items.Items.AnyOf[0].Properties
	if _, ok := props["title"]; ok {
		t.Error("runListOutputSchema declares a title property")
	}
	if _, ok := props["changeSummary"]; !ok {
		t.Error("runListOutputSchema lacks the changeSummary property")
	}
}

func TestListRunsWithoutTenantReadsFleet(t *testing.T) {
	deps := fleetDeps(t)
	tr := callPipelineTool(t, deps, "list_runs", map[string]interface{}{})
	if tr.IsError {
		t.Fatalf("list_runs returned error: %v", tr.Content)
	}
	var payload struct {
		Count int `json:"count"`
		Items []struct {
			Tenant  string `json:"tenant"`
			Trigger struct {
				Kind string `json:"kind"`
			} `json:"trigger"`
		} `json:"items"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &payload); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	if payload.Count != 2 || len(payload.Items) != 2 {
		t.Fatalf("count = %d, items = %d; want 2", payload.Count, len(payload.Items))
	}
	seen := map[string]bool{}
	for _, item := range payload.Items {
		seen[item.Tenant] = true
		if item.Trigger.Kind == "" {
			t.Errorf("item of tenant %q carries no trigger kind", item.Tenant)
		}
	}
	if !seen["pay"] || !seen["ops"] {
		t.Fatalf("tenants = %v; want pay and ops", seen)
	}
}

func TestListRunsTenantWithoutJobRefused(t *testing.T) {
	deps := fleetDeps(t)
	tr := callPipelineTool(t, deps, "list_runs", map[string]interface{}{"tenant": "pay"})
	if !tr.IsError || !strings.Contains(fmtContent(tr.Content), "job is required when tenant is set") {
		t.Fatalf("list_runs with tenant and no job: error %v, content %v; want the job error", tr.IsError, tr.Content)
	}
}

func TestGetPipelinesOverviewTool(t *testing.T) {
	deps := fleetDeps(t)
	tr := callPipelineTool(t, deps, "get_pipelines_overview", map[string]interface{}{})
	if tr.IsError {
		t.Fatalf("get_pipelines_overview returned error: %v", tr.Content)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(tr.StructuredContent, &payload); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	if _, ok := payload["tenants"]; !ok {
		t.Error("overview carries no tenants field")
	}
	if _, ok := payload["totals"]; !ok {
		t.Error("overview carries no totals field")
	}
}

// fmtContent renders a tool result's content for a substring check.
func fmtContent(content interface{}) string {
	b, _ := json.Marshal(content)
	return string(b)
}
