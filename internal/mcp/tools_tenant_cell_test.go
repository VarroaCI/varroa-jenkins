package mcp

import (
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// seedWarningTenant stores an unplaced pipelines Tenant whose TenantWarnings
// condition carries an ItemsApplyFailed warning.
func seedWarningTenant(t *testing.T, store *crdstore.Fake, tenant string) {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "Tenant",
		"metadata":   map[string]any{"name": tenant},
		"spec":       map[string]any{"pipelines": map[string]any{}},
		"status": map[string]any{"conditions": []any{
			map[string]any{
				"type":    v1alpha1.TenantConditionTenantWarnings,
				"status":  "True",
				"reason":  v1alpha1.TenantReasonItemsApplyFailed,
				"message": "create " + tenant + "/repos: HTTP 500: Jenkins internal error",
			},
		}},
	}}
	gvr, err := crdstore.GVRFor[v1alpha1.Tenant]()
	if err != nil {
		t.Fatalf("GVRFor[Tenant]: %v", err)
	}
	if err := store.SeedObject(gvr, obj); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
}

// TestGetTenantCellToolReturnsWarning proves the tool carries the Tenant's
// TenantWarnings condition, so an MCP caller sees a refused item.
func TestGetTenantCellToolReturnsWarning(t *testing.T) {
	store := crdstore.NewFake()
	seedWarningTenant(t, store, "team-a")
	deps := &api.Dependencies{
		Store:       store,
		RecordStore: newFakePipelineReader(),
		Authorizer:  pipelinesTestAuthorizer("read"),
	}

	tr := callPipelineTool(t, deps, "get_tenant_cell", map[string]interface{}{"tenant": "team-a"})
	if tr.IsError {
		t.Fatalf("get_tenant_cell returned error: %v", tr.Content)
	}
	var payload struct {
		State   string `json:"state"`
		Warning *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"warning"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &payload); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	if payload.State != "unplaced" {
		t.Errorf("state = %q, want unplaced", payload.State)
	}
	if payload.Warning == nil || payload.Warning.Reason != v1alpha1.TenantReasonItemsApplyFailed {
		t.Fatalf("warning = %+v, want an ItemsApplyFailed warning", payload.Warning)
	}
}

// TestGetTenantCellToolRefusesWithoutTenantRead proves the tool denies a
// caller whose grant reads no pipeline, and names no tenant.
func TestGetTenantCellToolRefusesWithoutTenantRead(t *testing.T) {
	store := crdstore.NewFake()
	seedWarningTenant(t, store, "team-b")
	deps := &api.Dependencies{
		Store:       store,
		RecordStore: newFakePipelineReader(),
		Authorizer:  pipelinesTestAuthorizer("write"),
	}

	tr := callPipelineTool(t, deps, "get_tenant_cell", map[string]interface{}{"tenant": "team-b"})
	if !tr.IsError {
		t.Fatal("expected get_tenant_cell to deny a caller without the tenant read grant")
	}
	if got := toolText(tr); got != "tenant not found" {
		t.Errorf("error text = %q, want %q", got, "tenant not found")
	}
}
