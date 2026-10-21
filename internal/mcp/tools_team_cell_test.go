package mcp

import (
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// seedWarningTeam stores an unplaced pipelines Team whose TenantWarnings
// condition carries an ItemsApplyFailed warning.
func seedWarningTeam(t *testing.T, store *crdstore.Fake, team string) {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "Team",
		"metadata":   map[string]any{"name": team},
		"spec":       map[string]any{"pipelines": map[string]any{}},
		"status": map[string]any{"conditions": []any{
			map[string]any{
				"type":    v1alpha1.TeamConditionTenantWarnings,
				"status":  "True",
				"reason":  v1alpha1.TeamReasonItemsApplyFailed,
				"message": "create " + team + "/repos: HTTP 500: Jenkins internal error",
			},
		}},
	}}
	gvr, err := crdstore.GVRFor[v1alpha1.Team]()
	if err != nil {
		t.Fatalf("GVRFor[Team]: %v", err)
	}
	if err := store.SeedObject(gvr, obj); err != nil {
		t.Fatalf("seed team: %v", err)
	}
}

// TestGetTeamCellToolReturnsWarning proves the tool carries the Team's
// TenantWarnings condition, so an MCP caller sees a refused item.
func TestGetTeamCellToolReturnsWarning(t *testing.T) {
	store := crdstore.NewFake()
	seedWarningTeam(t, store, "team-a")
	deps := &api.Dependencies{
		Store:       store,
		RecordStore: newFakePipelineReader(),
		Authorizer:  pipelinesTestAuthorizer("read"),
	}

	tr := callPipelineTool(t, deps, "get_team_cell", map[string]interface{}{"team": "team-a"})
	if tr.IsError {
		t.Fatalf("get_team_cell returned error: %v", tr.Content)
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
	if payload.Warning == nil || payload.Warning.Reason != v1alpha1.TeamReasonItemsApplyFailed {
		t.Fatalf("warning = %+v, want an ItemsApplyFailed warning", payload.Warning)
	}
}

// TestGetTeamCellToolRefusesWithoutTenantRead proves the tool denies a
// caller whose grant reads no pipeline, and names no team.
func TestGetTeamCellToolRefusesWithoutTenantRead(t *testing.T) {
	store := crdstore.NewFake()
	seedWarningTeam(t, store, "team-b")
	deps := &api.Dependencies{
		Store:       store,
		RecordStore: newFakePipelineReader(),
		Authorizer:  pipelinesTestAuthorizer("write"),
	}

	tr := callPipelineTool(t, deps, "get_team_cell", map[string]interface{}{"team": "team-b"})
	if !tr.IsError {
		t.Fatal("expected get_team_cell to deny a caller without the tenant read grant")
	}
	if got := toolText(tr); got != "team not found" {
		t.Errorf("error text = %q, want %q", got, "team not found")
	}
}
