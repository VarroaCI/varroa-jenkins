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
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
)

// poolBrood knows the local cluster core and the member member-c.
type poolBrood struct{ api.Brood }

func (poolBrood) LocalCluster() string { return "core" }

func (poolBrood) Clusters(context.Context) ([]api.ClusterInfo, error) {
	return []api.ClusterInfo{
		{ClusterInfo: bus.ClusterInfo{Name: "core"}, Core: true, Healthy: true},
		{ClusterInfo: bus.ClusterInfo{Name: "member-c"}, Healthy: true},
	}, nil
}

func (poolBrood) IsKnown(_ context.Context, cluster string) bool {
	return cluster == "core" || cluster == "member-c"
}

// memberPools serves the member-c pool member-2-570 from memory.
type memberPools struct {
	api.ConfigBrood
	pool        *v1alpha1.CellPool
	unreachable bool
	scaled      []string
}

func (d *memberPools) ListCellPools(context.Context, string) ([]v1alpha1.CellPool, error) {
	if d.unreachable {
		return nil, &api.ErrClusterUnreachable{Cluster: "member-c", Err: errors.New("no responders")}
	}
	return []v1alpha1.CellPool{*d.pool}, nil
}

func (d *memberPools) GetCellPool(_ context.Context, _, name string) (*v1alpha1.CellPool, error) {
	if d.unreachable {
		return nil, &api.ErrClusterUnreachable{Cluster: "member-c", Err: errors.New("no responders")}
	}
	if name != d.pool.Name {
		return nil, &api.BroodError{Code: bus.CodeNotFound, Msg: "not found"}
	}
	return d.pool, nil
}

func (d *memberPools) ScaleCellPool(_ context.Context, cluster string, req bus.CellPoolScaleRequest) (*v1alpha1.CellPool, error) {
	if d.unreachable {
		return nil, &api.ErrClusterUnreachable{Cluster: "member-c", Err: errors.New("no responders")}
	}
	d.scaled = append(d.scaled, cluster+"/"+req.Name)
	updated := d.pool.DeepCopy()
	if err := cells.ApplyCellPoolDials(&updated.Spec, cells.CellPoolDials{Warm: req.Warm, MaxCells: req.MaxCells}); err != nil {
		return nil, &api.BroodError{Code: bus.CodeInvalid, Msg: err.Error()}
	}
	d.pool = updated
	return updated, nil
}

func clusterPoolDeps(t *testing.T) (*api.Dependencies, *memberPools, *recordingSink) {
	t.Helper()
	deps, _ := cellPoolDeps(t, "read", "update")
	members := &memberPools{pool: &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "member-2-570"},
		Spec:       v1alpha1.CellPoolSpec{ProfileRef: "lts-2-570", Warm: 1, MaxCells: 4, MaxTenantsPerCell: 4},
	}}
	sink := &recordingSink{}
	deps.Brood = poolBrood{}
	deps.ConfigBrood = members
	deps.ActivityPublisher = sink
	return deps, members, sink
}

func TestListCellPools_NamesEachCluster(t *testing.T) {
	deps, _, _ := clusterPoolDeps(t)
	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
		"name": "list_cell_pools", "arguments": map[string]interface{}{},
	}, mcpAdminClaims)
	var payload struct {
		StructuredContent struct {
			Items    []cellPoolSummary         `json:"items"`
			Clusters []api.ClusterFanoutStatus `json:"clusters"`
		} `json:"structuredContent"`
	}
	b, _ := json.Marshal(resp.Result)
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, it := range payload.StructuredContent.Items {
		got[it.Cluster+"/"+it.Name] = true
	}
	for _, want := range []string{"core/lts-2-555", "core/big", "member-c/member-2-570"} {
		if !got[want] {
			t.Errorf("list missing %s: %v", want, got)
		}
	}
	if len(payload.StructuredContent.Clusters) != 2 {
		t.Errorf("clusters = %+v, want core and member-c", payload.StructuredContent.Clusters)
	}
}

func TestListCellPools_UnreachableMemberAddsNoItem(t *testing.T) {
	deps, members, _ := clusterPoolDeps(t)
	members.unreachable = true
	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
		"name": "list_cell_pools", "arguments": map[string]interface{}{},
	}, mcpAdminClaims)
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list must not fail on an unreachable member: %v", tr.Content)
	}
	b, _ := json.Marshal(resp.Result)
	if strings.Contains(string(b), "member-2-570") {
		t.Error("an unreachable member must add no item")
	}
	if !strings.Contains(string(b), `"ok":false`) {
		t.Errorf("the clusters status must report member-c with ok false: %s", b)
	}
}

func TestGetCellPool_ReadsTheNamedCluster(t *testing.T) {
	deps, _, _ := clusterPoolDeps(t)
	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
		"name":      "get_cell_pool",
		"arguments": map[string]interface{}{"name": "member-2-570", "cluster": "member-c"},
	}, mcpAdminClaims)
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("get_cell_pool returned an error: %v", tr.Content)
	}
	var payload struct {
		StructuredContent struct {
			Cluster  string            `json:"cluster"`
			Metadata metav1.ObjectMeta `json:"metadata"`
		} `json:"structuredContent"`
	}
	b, _ := json.Marshal(resp.Result)
	_ = json.Unmarshal(b, &payload)
	if payload.StructuredContent.Cluster != "member-c" || payload.StructuredContent.Metadata.Name != "member-2-570" {
		t.Errorf("result = %+v, want member-c/member-2-570", payload.StructuredContent)
	}
}

func TestCellPoolTools_RefuseAnUnknownOrUnreachableCluster(t *testing.T) {
	for _, tc := range []struct {
		name        string
		tool        string
		args        map[string]interface{}
		unreachable bool
	}{
		{"get unknown", "get_cell_pool", map[string]interface{}{"name": "p", "cluster": "nowhere"}, false},
		{"update unknown", "update_cell_pool", map[string]interface{}{"name": "p", "cluster": "nowhere", "warm": 1}, false},
		{"list unknown", "list_cell_pools", map[string]interface{}{"cluster": "nowhere"}, false},
		{"get unreachable", "get_cell_pool", map[string]interface{}{"name": "member-2-570", "cluster": "member-c"}, true},
		{"update unreachable", "update_cell_pool", map[string]interface{}{"name": "member-2-570", "cluster": "member-c", "warm": 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, members, sink := clusterPoolDeps(t)
			members.unreachable = tc.unreachable
			resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
				"name": tc.tool, "arguments": tc.args,
			}, mcpAdminClaims)
			if tr := parseToolResult(t, resp.Result); !tr.IsError {
				t.Fatal("the call must be refused")
			}
			if len(sink.events) != 0 {
				t.Errorf("a refused call must publish nothing, got %+v", sink.events)
			}
		})
	}
}

func TestUpdateCellPool_ScalesTheMemberAndNamesItInTheEvent(t *testing.T) {
	deps, members, sink := clusterPoolDeps(t)
	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
		"name":      "update_cell_pool",
		"arguments": map[string]interface{}{"name": "member-2-570", "cluster": "member-c", "warm": 2},
	}, mcpAdminClaims)
	if tr := parseToolResult(t, resp.Result); tr.IsError {
		t.Fatalf("update_cell_pool returned an error: %v", tr.Content)
	}
	if len(members.scaled) != 1 || members.scaled[0] != "member-c/member-2-570" {
		t.Errorf("scaled = %v, want member-c/member-2-570", members.scaled)
	}
	if members.pool.Spec.Warm != 2 {
		t.Errorf("warm = %d, want 2", members.pool.Spec.Warm)
	}
	if len(sink.events) != 1 || sink.events[0].Cluster != "member-c" || sink.events[0].Source != "mcp" {
		t.Errorf("events = %+v, want one mcp event naming member-c", sink.events)
	}
}
