package mcp

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// cellPoolDeps seeds two pools and an Authorizer holding the operator's own
// cellpools verbs through a cluster-wide binding.
func cellPoolDeps(t *testing.T, verbs ...string) (*api.Dependencies, *crdstore.Fake) {
	t.Helper()
	store := crdstore.NewFake()
	crdstore.MustSeed(store,
		&v1alpha1.CellPool{
			ObjectMeta: metav1.ObjectMeta{Name: "lts-2-555"},
			Spec: v1alpha1.CellPoolSpec{
				ProfileRef:        "lts-2-555",
				Warm:              1,
				MaxCells:          6,
				MaxTenantsPerCell: 10,
				MaxRunsPerTenant:  4,
				CellTemplate: &v1alpha1.CellTemplate{
					PodOverrides: &v1alpha1.PodOverrides{NodeSelector: map[string]string{"pool": "cells"}},
				},
			},
			Status: v1alpha1.CellPoolStatus{WarmCells: 1},
		},
		&v1alpha1.CellPool{
			ObjectMeta: metav1.ObjectMeta{Name: "big"},
			Spec:       v1alpha1.CellPoolSpec{ProfileRef: "lts-2-555", Warm: 2, MaxTenantsPerCell: 10},
		},
	)
	roles := []*v1alpha1.VarroaRole{{
		ObjectMeta: metav1.ObjectMeta{Name: "operator"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"cellpools"}, Verbs: verbs},
		}},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{{
		ObjectMeta: metav1.ObjectMeta{Name: "operator-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "admin-user"}},
			RoleRef:  "operator",
		},
	}}
	return &api.Dependencies{
		Client:     &stubClient{},
		Store:      store,
		Authorizer: api.NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false),
	}, store
}

// TestListCellPools covers "A reader lists pools over MCP".
func TestListCellPools(t *testing.T) {
	deps, _ := cellPoolDeps(t, "read")
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_cell_pools",
		"arguments": map[string]interface{}{},
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list_cell_pools returned an error: %v", tr.Content)
	}
	var payload struct {
		StructuredContent struct {
			Items []cellPoolSummary `json:"items"`
			Count int               `json:"count"`
		} `json:"structuredContent"`
	}
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if payload.StructuredContent.Count != 2 {
		t.Fatalf("count = %d, want 2", payload.StructuredContent.Count)
	}
	if payload.StructuredContent.Items[0].Name != "big" {
		t.Errorf("items must sort by name, got %q first", payload.StructuredContent.Items[0].Name)
	}
	got := payload.StructuredContent.Items[1]
	if got.ProfileRef != "lts-2-555" || got.Warm != 1 || got.MaxCells != 6 || got.MaxTenantsPerCell != 10 {
		t.Errorf("the summary must carry every spec dial, got %+v", got)
	}
	if got.MaxRunsPerTenant != 4 {
		t.Errorf("the summary must carry maxRunsPerTenant, got %+v", got)
	}
	if got.WarmCells != 1 {
		t.Errorf("the summary must carry every status count, got %+v", got)
	}
}

// TestListCellPools_Verbose proves the verbose flag returns full resources,
// so the cell template and the condition list stay reachable.
func TestListCellPools_Verbose(t *testing.T) {
	deps, _ := cellPoolDeps(t, "read")
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_cell_pools",
		"arguments": map[string]interface{}{"verbose": true},
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list_cell_pools returned an error: %v", tr.Content)
	}
	var payload struct {
		StructuredContent struct {
			Items []struct {
				Spec v1alpha1.CellPoolSpec `json:"spec"`
			} `json:"items"`
		} `json:"structuredContent"`
	}
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(payload.StructuredContent.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(payload.StructuredContent.Items))
	}
	if payload.StructuredContent.Items[1].Spec.CellTemplate == nil {
		t.Error("a verbose listing must carry the cell template")
	}
}

// TestGetCellPool proves the detail tool returns the named pool.
func TestGetCellPool(t *testing.T) {
	deps, _ := cellPoolDeps(t, "read")
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "get_cell_pool",
		"arguments": map[string]interface{}{"name": "lts-2-555"},
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("get_cell_pool returned an error: %v", tr.Content)
	}
}

// TestGetCellPool_DeniedWithoutVerb covers "A caller without the verb is
// refused".
func TestGetCellPool_DeniedWithoutVerb(t *testing.T) {
	deps, _ := cellPoolDeps(t, "update")
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "get_cell_pool",
		"arguments": map[string]interface{}{"name": "lts-2-555"},
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatal("a caller with no cellpools:read grant must be refused")
	}
}

// TestUpdateCellPool_WritesOnlyTheDials covers "The update tool writes only
// the dials": profileRef and cellTemplate stay unchanged.
func TestUpdateCellPool_WritesOnlyTheDials(t *testing.T) {
	deps, store := cellPoolDeps(t, "read", "update")
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "update_cell_pool",
		"arguments": map[string]interface{}{"name": "lts-2-555", "warm": 4},
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("update_cell_pool returned an error: %v", tr.Content)
	}
	stored, err := crdstore.Get[v1alpha1.CellPool](context.Background(), store, "lts-2-555", "")
	if err != nil {
		t.Fatalf("get stored pool: %v", err)
	}
	if stored.Spec.Warm != 4 {
		t.Errorf("warm = %d, want 4", stored.Spec.Warm)
	}
	if stored.Spec.ProfileRef != "lts-2-555" {
		t.Errorf("profileRef changed to %q", stored.Spec.ProfileRef)
	}
	if stored.Spec.CellTemplate == nil || stored.Spec.CellTemplate.PodOverrides == nil {
		t.Error("cellTemplate must be unchanged")
	}
	if stored.Spec.MaxCells != 6 || stored.Spec.MaxTenantsPerCell != 10 {
		t.Errorf("an unnamed dial changed: %+v", stored.Spec)
	}
}

// TestUpdateCellPool_DeniedWithoutVerb proves the update gate.
func TestUpdateCellPool_DeniedWithoutVerb(t *testing.T) {
	deps, store := cellPoolDeps(t, "read")
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "update_cell_pool",
		"arguments": map[string]interface{}{"name": "lts-2-555", "warm": 4},
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatal("a caller with no cellpools:update grant must be refused")
	}
	stored, err := crdstore.Get[v1alpha1.CellPool](context.Background(), store, "lts-2-555", "")
	if err != nil {
		t.Fatalf("get stored pool: %v", err)
	}
	if stored.Spec.Warm != 1 {
		t.Errorf("a refused call must write nothing, got warm %d", stored.Spec.Warm)
	}
}

// TestUpdateCellPool_RefusesABadDial proves the numeric bounds are repeated
// before the write.
func TestUpdateCellPool_RefusesABadDial(t *testing.T) {
	tests := []struct {
		name string
		args map[string]interface{}
	}{
		{name: "negative warm", args: map[string]interface{}{"name": "lts-2-555", "warm": -1}},
		{name: "zero maxTenantsPerCell", args: map[string]interface{}{"name": "lts-2-555", "maxTenantsPerCell": 0}},
		{name: "cap below warm", args: map[string]interface{}{"name": "big", "maxCells": 1}},
		{name: "unparsable duration", args: map[string]interface{}{"name": "lts-2-555", "unreadyGracePeriod": "soon"}},
		{name: "no dial", args: map[string]interface{}{"name": "lts-2-555"}},
		// A fractional dial must be refused, never truncated: warm 1.9 is
		// not a request for 1.
		{name: "fractional warm", args: map[string]interface{}{"name": "lts-2-555", "warm": 1.9}},
		{name: "fractional maxCells", args: map[string]interface{}{"name": "lts-2-555", "maxCells": 2.5}},
		// A value outside int32 must be refused before the conversion, which
		// would otherwise wrap a large positive into a negative and pass the
		// bounds check it already cleared.
		{name: "int32 overflow", args: map[string]interface{}{"name": "lts-2-555", "warm": 2147483648.0}},
		{name: "far beyond int32", args: map[string]interface{}{"name": "lts-2-555", "maxCells": 1e18}},
		{name: "negative int32 underflow", args: map[string]interface{}{"name": "lts-2-555", "warm": -2147483649.0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, _ := cellPoolDeps(t, "read", "update")
			handler := NewHandler(deps)
			resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
				"name":      "update_cell_pool",
				"arguments": tt.args,
			}, mcpAdminClaims)
			if tr := parseToolResult(t, resp.Result); !tr.IsError {
				t.Fatalf("expected a tool error, got success: %v", tr.Content)
			}
		})
	}
}

// TestUpdateCellPool_RefusedDialWritesNothing proves a refused numeric dial
// is caught before the write, not after a silent truncation.
func TestUpdateCellPool_RefusedDialWritesNothing(t *testing.T) {
	deps, store := cellPoolDeps(t, "read", "update")
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "update_cell_pool",
		"arguments": map[string]interface{}{"name": "lts-2-555", "warm": 1.9},
	}, mcpAdminClaims)

	if tr := parseToolResult(t, resp.Result); !tr.IsError {
		t.Fatalf("a fractional dial must be refused, got success: %v", tr.Content)
	}
	stored, err := crdstore.Get[v1alpha1.CellPool](context.Background(), store, "lts-2-555", "")
	if err != nil {
		t.Fatalf("get stored pool: %v", err)
	}
	if stored.Spec.Warm != 1 {
		t.Errorf("warm = %d, want the stored 1 untouched", stored.Spec.Warm)
	}
}

// TestUpdateCellPool_AcceptsAnIntegralFloat proves the guard refuses only a
// fractional value: JSON carries every number as a float, so 3 arrives as 3.0
// and must still be accepted.
func TestUpdateCellPool_AcceptsAnIntegralFloat(t *testing.T) {
	deps, store := cellPoolDeps(t, "read", "update")
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "update_cell_pool",
		"arguments": map[string]interface{}{"name": "lts-2-555", "warm": 3.0},
	}, mcpAdminClaims)

	if tr := parseToolResult(t, resp.Result); tr.IsError {
		t.Fatalf("an integral float must be accepted: %v", tr.Content)
	}
	stored, err := crdstore.Get[v1alpha1.CellPool](context.Background(), store, "lts-2-555", "")
	if err != nil {
		t.Fatalf("get stored pool: %v", err)
	}
	if stored.Spec.Warm != 3 {
		t.Errorf("warm = %d, want 3", stored.Spec.Warm)
	}
}

// TestUpdateCellPool_Annotations covers "The update tool is not annotated
// destructive".
func TestUpdateCellPool_Annotations(t *testing.T) {
	for _, tool := range liveTools(t) {
		if tool.Name != "update_cell_pool" {
			continue
		}
		if tool.Annotations.ReadOnlyHint == nil || *tool.Annotations.ReadOnlyHint {
			t.Error("readOnlyHint must be false: the tool writes")
		}
		if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Error("destructiveHint must be false: the tool removes nothing")
		}
		if tool.Annotations.IdempotentHint == nil || !*tool.Annotations.IdempotentHint {
			t.Error("idempotentHint must be true")
		}
		if tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Error("openWorldHint must be false")
		}
		return
	}
	t.Fatal("update_cell_pool is not registered")
}

// TestCellPoolTools_RefuseAnAbsentAuthorizer proves each tool refuses rather
// than dereferencing a nil Authorizer.
func TestCellPoolTools_RefuseAnAbsentAuthorizer(t *testing.T) {
	handler := NewHandler(newTestDeps())
	for _, name := range []string{"list_cell_pools", "get_cell_pool", "update_cell_pool"} {
		t.Run(name, func(t *testing.T) {
			resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
				"name":      name,
				"arguments": map[string]interface{}{"name": "lts-2-555", "warm": 2},
			}, mcpAdminClaims)
			if tr := parseToolResult(t, resp.Result); !tr.IsError {
				t.Fatalf("%s must refuse an absent authorizer", name)
			}
		})
	}
}
