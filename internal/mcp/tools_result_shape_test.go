package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// recordingBrood captures what the MCP tools hand to the Brood so tests can
// assert on the wire arguments rather than on a live cluster.
type recordingBrood struct {
	api.Brood

	createArgs    api.ControllersCreateArgs
	createCluster string

	updatePatch  json.RawMessage
	updateActor  string
	updateForce  bool
	updateCalled bool

	// updateErr, when set, is returned instead of applying the patch.
	updateErr error
}

func (r *recordingBrood) LocalCluster() string { return "local" }

func (r *recordingBrood) Create(_ context.Context, cluster string, req api.ControllersCreateArgs) (*v1alpha1.Controller, []bus.Check, error) {
	r.createCluster = cluster
	r.createArgs = req
	var cr v1alpha1.Controller
	_ = json.Unmarshal(req.Controller, &cr)
	cr.Namespace = req.Namespace
	return &cr, nil, nil
}

func (r *recordingBrood) Update(_ context.Context, _ string, ns, name string, patch json.RawMessage, actor string, force bool) (*v1alpha1.Controller, []bus.Check, []bus.UnappliedRemoval, error) {
	r.updateCalled = true
	r.updatePatch = patch
	r.updateActor = actor
	r.updateForce = force
	if r.updateErr != nil {
		return nil, nil, nil, r.updateErr
	}
	cr := &v1alpha1.Controller{}
	cr.Namespace, cr.Name = ns, name
	return cr, nil, nil, nil
}

// Delete lets delete_controller run through the brood path in tests; without
// it the embedded nil api.Brood interface nil-panics.
func (r *recordingBrood) Delete(_ context.Context, _, _, _ string) error { return nil }

func broodHandler(b api.Brood) *stubHandlerDeps {
	stub := newGuardStub()
	stub.namespaces["team-a"] = true
	stub.namespaces["varroa-system"] = true
	return &stubHandlerDeps{
		stub: stub,
		deps: &api.Dependencies{Client: stub, Store: stub, Authorizer: guardAuthorizer(), Brood: b},
	}
}

type stubHandlerDeps struct {
	stub *guardStub
	deps *api.Dependencies
}

// The operator derives cr.Namespace from ControllersCreateArgs.Namespace, not
// from the marshalled CR. Dropping it creates the object in the empty namespace
// and the API server answers 404 ("the server could not find the requested
// resource"), which made create_controller unusable over MCP.
func TestMCPCreateController_SendsNamespaceToBrood(t *testing.T) {
	brood := &recordingBrood{}
	h := broodHandler(brood)
	handler := NewHandler(h.deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "create_controller",
		"arguments": map[string]interface{}{"namespace": "team-a", "name": "newci"},
	}, guardClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("expected success, got error: %q", toolText(tr))
	}
	if brood.createArgs.Namespace != "team-a" {
		t.Errorf("Brood.Create got namespace %q, want %q", brood.createArgs.Namespace, "team-a")
	}
}

// A merge patch carrying "version": "" blanks the pinned Jenkins version of a
// live controller, so absent optional args must not reach the patch at all.
func TestMCPUpdateController_OmitsUnsuppliedFields(t *testing.T) {
	brood := &recordingBrood{}
	h := broodHandler(brood)
	handler := NewHandler(h.deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "update_controller",
		"arguments": map[string]interface{}{"namespace": "team-a", "name": "ci", "composedBundleRef": "b"},
	}, guardClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("expected success, got error: %q", toolText(tr))
	}

	var patch struct {
		Spec map[string]interface{} `json:"spec"`
	}
	if err := json.Unmarshal(brood.updatePatch, &patch); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	if _, present := patch.Spec["version"]; present {
		t.Errorf("patch must omit version when not supplied, got %s", brood.updatePatch)
	}
	ref, ok := patch.Spec["composedBundleRef"].(map[string]interface{})
	if !ok || ref["name"] != "b" {
		t.Errorf("patch must carry composedBundleRef, got %s", brood.updatePatch)
	}
}

func TestMCPUpdateController_RejectsEmptyUpdate(t *testing.T) {
	brood := &recordingBrood{}
	h := broodHandler(brood)
	handler := NewHandler(h.deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "update_controller",
		"arguments": map[string]interface{}{"namespace": "team-a", "name": "ci"},
	}, guardClaims)

	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatalf("expected tool error for a no-op update, got success")
	}
	if brood.updateCalled {
		t.Error("Brood.Update must not be called for a no-op update")
	}
}

// MCP requires structuredContent to be a JSON object. Emitting a bare array
// makes strict clients reject the whole result, which took out every list_*
// tool ("expected record, received array").
func TestResultJSON_WrapsCollections(t *testing.T) {
	t.Run("slice is boxed", func(t *testing.T) {
		res, err := resultJSON([]string{"a", "b"})
		if err != nil {
			t.Fatalf("resultJSON: %v", err)
		}
		m, ok := res.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("structuredContent must be an object, got %T", res.StructuredContent)
		}
		if m["count"] != 2 {
			t.Errorf("count = %v, want 2", m["count"])
		}
		if _, ok := m["items"]; !ok {
			t.Error("boxed result must carry items")
		}
	})

	t.Run("nil slice becomes empty list", func(t *testing.T) {
		var controllers []*v1alpha1.Controller
		res, err := resultJSON(controllers)
		if err != nil {
			t.Fatalf("resultJSON: %v", err)
		}
		m := res.StructuredContent.(map[string]any)
		if m["count"] != 0 {
			t.Errorf("count = %v, want 0", m["count"])
		}
		blob, err := json.Marshal(m["items"])
		if err != nil {
			t.Fatalf("marshal items: %v", err)
		}
		if string(blob) != "[]" {
			t.Errorf("items = %s, want []", blob)
		}
	})

	t.Run("struct passes through", func(t *testing.T) {
		cr := &v1alpha1.Controller{}
		cr.Name = "ci"
		res, err := resultJSON(cr)
		if err != nil {
			t.Fatalf("resultJSON: %v", err)
		}
		// Sanitization renders every result as a generic map, so the Go type no
		// longer distinguishes boxed from unboxed. Assert the wire contract
		// instead: a single object carries its own fields, not an items/count
		// wrapper.
		m, ok := res.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("structuredContent must be an object, got %T", res.StructuredContent)
		}
		if _, boxed := m["items"]; boxed {
			t.Error("single objects must not be boxed in items/count")
		}
		if _, boxed := m["count"]; boxed {
			t.Error("single objects must not be boxed in items/count")
		}
		meta, ok := m["metadata"].(map[string]any)
		if !ok {
			t.Fatalf("single object must expose its own fields, got %v", m)
		}
		if meta["name"] != "ci" {
			t.Errorf("metadata.name = %v, want ci", meta["name"])
		}
	})
}
