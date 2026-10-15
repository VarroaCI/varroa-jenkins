package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
)

// fanoutBrood stubs the brood for list_controllers. It returns fixed rows and
// fan-out statuses and records the cluster filter that ListAll received.
type fanoutBrood struct {
	api.Brood
	rows      []api.ClusterController
	statuses  []api.ClusterFanoutStatus
	err       error
	gotFilter string
	listed    bool
}

func (f *fanoutBrood) LocalCluster() string { return "core" }

func (f *fanoutBrood) IsKnown(_ context.Context, c string) bool {
	return c == "core" || c == "member-b"
}

func (f *fanoutBrood) ListAll(_ context.Context, _ string, filter string) ([]api.ClusterController, []api.ClusterFanoutStatus, error) {
	f.listed = true
	f.gotFilter = filter
	return f.rows, f.statuses, f.err
}

func fanoutCoreRow() api.ClusterController {
	return api.ClusterController{
		Cluster: "core",
		CR: &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"},
		},
	}
}

func fanoutDeps(b *fanoutBrood) *api.Dependencies {
	deps := adminDeps()
	deps.Brood = b
	return deps
}

// callListControllers runs list_controllers and returns the raw tool result.
func callListControllers(t *testing.T, deps *api.Dependencies, args map[string]interface{}) toolResult {
	t.Helper()
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_controllers",
		"arguments": args,
	}, mcpAdminClaims)
	return parseToolResult(t, resp.Result)
}

// decodeStructured returns the structuredContent of a tool result.
func decodeStructured(t *testing.T, tr toolResult) map[string]any {
	t.Helper()
	var sc map[string]any
	if err := json.Unmarshal(tr.StructuredContent, &sc); err != nil {
		t.Fatalf("decode structuredContent: %v (%s)", err, tr.StructuredContent)
	}
	if sc == nil {
		t.Fatalf("no structuredContent in result")
	}
	return sc
}

func clusterRow(t *testing.T, sc map[string]any, name string) map[string]any {
	t.Helper()
	rows, ok := sc["clusters"].([]any)
	if !ok {
		t.Fatalf("clusters missing or not an array: %v", sc)
	}
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if ok && row["name"] == name {
			return row
		}
	}
	t.Fatalf("no clusters row named %q in %v", name, rows)
	return nil
}

func TestListControllers_ReportsFailedMemberCluster(t *testing.T) {
	b := &fanoutBrood{
		rows: []api.ClusterController{fanoutCoreRow()},
		statuses: []api.ClusterFanoutStatus{
			{Name: "core", OK: true},
			{Name: "member-b", OK: false, Error: "cluster unreachable"},
		},
	}
	tr := callListControllers(t, fanoutDeps(b), map[string]interface{}{"namespace": "ns"})
	if tr.IsError {
		t.Fatalf("unexpected tool error: %v", tr.Content)
	}
	sc := decodeStructured(t, tr)
	if got := sc["count"]; got != float64(1) {
		t.Errorf("count = %v, want 1", got)
	}
	if rows := sc["clusters"].([]any); len(rows) != 2 {
		t.Fatalf("clusters has %d rows, want 2: %v", len(rows), rows)
	}
	row := clusterRow(t, sc, "member-b")
	if row["ok"] != false || row["error"] != "cluster unreachable" {
		t.Errorf("member-b row = %v, want ok false with error", row)
	}
}

func TestListControllers_VerboseKeepsClusters(t *testing.T) {
	b := &fanoutBrood{
		rows: []api.ClusterController{fanoutCoreRow()},
		statuses: []api.ClusterFanoutStatus{
			{Name: "core", OK: true},
			{Name: "member-b", OK: false, Error: "cluster unreachable"},
		},
	}
	tr := callListControllers(t, fanoutDeps(b), map[string]interface{}{"namespace": "ns", "verbose": true})
	if tr.IsError {
		t.Fatalf("unexpected tool error: %v", tr.Content)
	}
	sc := decodeStructured(t, tr)
	if rows := sc["clusters"].([]any); len(rows) != 2 {
		t.Errorf("verbose clusters has %d rows, want 2", len(rows))
	}
	clusterRow(t, sc, "member-b")
}

func TestListControllers_FilteredMemberFailureIsNotToolError(t *testing.T) {
	b := &fanoutBrood{
		statuses: []api.ClusterFanoutStatus{{Name: "member-b", OK: false, Error: "cluster unreachable"}},
		err:      errors.New("list remote member-b: no responders"),
	}
	tr := callListControllers(t, fanoutDeps(b), map[string]interface{}{"cluster": "member-b"})
	if tr.IsError {
		t.Fatalf("filtered member failure must not be a tool error: %v", tr.Content)
	}
	sc := decodeStructured(t, tr)
	items, ok := sc["items"].([]any)
	if !ok || len(items) != 0 {
		t.Errorf("items = %v, want empty array (not null)", sc["items"])
	}
	if got := sc["count"]; got != float64(0) {
		t.Errorf("count = %v, want 0", got)
	}
	row := clusterRow(t, sc, "member-b")
	if row["ok"] != false {
		t.Errorf("member-b row = %v, want ok false", row)
	}
}

func TestListControllers_UnknownClusterIsToolError(t *testing.T) {
	b := &fanoutBrood{}
	tr := callListControllers(t, fanoutDeps(b), map[string]interface{}{"cluster": "nowhere"})
	if !tr.IsError {
		t.Fatalf("unknown cluster must be a tool error, got %v", tr.Content)
	}
	if len(tr.Content) == 0 || tr.Content[0].Text != "unknown cluster: nowhere" {
		t.Errorf("error content = %v, want %q", tr.Content, "unknown cluster: nowhere")
	}
	if b.listed {
		t.Errorf("ListAll ran for an unknown cluster (filter %q)", b.gotFilter)
	}
}

func TestListControllers_ListAllErrorWithoutStatusIsToolError(t *testing.T) {
	b := &fanoutBrood{err: errors.New("boom")}
	tr := callListControllers(t, fanoutDeps(b), map[string]interface{}{})
	if !tr.IsError {
		t.Fatalf("expected tool error, got %v", tr.Content)
	}
	if len(tr.Content) == 0 || !strings.HasPrefix(tr.Content[0].Text, "failed to list controllers") {
		t.Errorf("error content = %v, want prefix %q", tr.Content, "failed to list controllers")
	}
}

func TestListControllers_NoBroodReportsEmptyClusters(t *testing.T) {
	tr := callListControllers(t, adminDeps(), map[string]interface{}{"namespace": "ns"})
	if tr.IsError {
		t.Fatalf("unexpected tool error: %v", tr.Content)
	}
	sc := decodeStructured(t, tr)
	rows, ok := sc["clusters"].([]any)
	if !ok || len(rows) != 0 {
		t.Errorf("clusters = %v, want empty array", sc["clusters"])
	}
}

// A failed read of the local cluster carries its own status row. That row must
// not turn the failure into a successful empty result.
func TestListControllers_FilteredLocalFailureIsToolError(t *testing.T) {
	b := &fanoutBrood{
		statuses: []api.ClusterFanoutStatus{{Name: "core", OK: false, Error: "store unavailable"}},
		err:      errors.New("store unavailable"),
	}
	tr := callListControllers(t, fanoutDeps(b), map[string]interface{}{"cluster": "core"})
	if !tr.IsError {
		t.Fatalf("failed local read must be a tool error, got %v", tr.Content)
	}
	if len(tr.Content) == 0 || !strings.HasPrefix(tr.Content[0].Text, "failed to list controllers") {
		t.Errorf("error content = %v, want prefix %q", tr.Content, "failed to list controllers")
	}
}
