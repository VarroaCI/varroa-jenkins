package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// These tests assert that update_controller surfaces an SSA field conflict
// with the conflicting field and its owning manager, and that force=true
// reaches the brood as an explicit opt-in rather than a hardcoded default.

// conflictError is the BroodError the operator returns for an SSA conflict:
// a bare message plus the actionable half in Conflicts.
func conflictError() *api.BroodError {
	return &api.BroodError{
		Code: bus.CodeConflict,
		Msg:  "field conflict",
		Conflicts: []bus.FieldConflict{{
			Field:   ".spec.powerState",
			Manager: "kubectl-patch",
			Message: `conflict with "kubectl-patch"`,
		}},
	}
}

// Without force the tool must still refuse — but the refusal has to name the
// field and its owner, which is the difference between a diagnosis and a dead
// end.
func TestMCPUpdateController_ConflictErrorNamesFieldAndManager(t *testing.T) {
	brood := &recordingBrood{updateErr: conflictError()}
	handler := NewHandler(broodHandler(brood).deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_controller",
		"arguments": map[string]interface{}{
			"namespace": "team-a", "name": "ci", "powerState": "Running",
		},
	}, guardClaims)

	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatal("expected a conflict error, got success")
	}
	text := toolText(tr)
	for _, want := range []string{".spec.powerState", "kubectl-patch", "force=true"} {
		if !strings.Contains(text, want) {
			t.Errorf("conflict error does not mention %q:\n%s", want, text)
		}
	}
	if brood.updateForce {
		t.Error("force must default to false when the caller does not ask for it")
	}
}

// The retry the error message tells the caller to make has to actually reach
// the brood as force=true; that argument is the whole escape hatch.
func TestMCPUpdateController_ForceReachesBrood(t *testing.T) {
	brood := &recordingBrood{}
	handler := NewHandler(broodHandler(brood).deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_controller",
		"arguments": map[string]interface{}{
			"namespace": "team-a", "name": "ci", "powerState": "Running", "force": true,
		},
	}, guardClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("expected success, got error: %q", toolText(tr))
	}
	if !brood.updateForce {
		t.Error("Brood.Update got force=false, want true")
	}
	// force controls how the patch is applied, never what it contains.
	if strings.Contains(string(brood.updatePatch), "force") {
		t.Errorf("force leaked into the spec patch: %s", brood.updatePatch)
	}
}

// create_controller shares the argument allowlist but goes through Brood.Create,
// which owns nothing and cannot conflict. Accepting `force` there would be the
// "accepted and then ignored" trap the allowlist exists to prevent.
func TestMCPCreateController_RejectsForce(t *testing.T) {
	brood := &recordingBrood{}
	handler := NewHandler(broodHandler(brood).deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "create_controller",
		"arguments": map[string]interface{}{
			"namespace": "team-a", "name": "newci", "force": true,
		},
	}, guardClaims)

	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatal("create_controller accepted force, want it rejected as unrecognized")
	}
	if text := toolText(tr); !strings.Contains(text, "force") {
		t.Errorf("rejection does not name the offending argument:\n%s", text)
	}
}

// update_controller must declare force, or no MCP client can send it.
func TestUpdateControllerDeclaresForce(t *testing.T) {
	handler := NewHandler(newTestDeps())
	resp := mcpRequest(t, handler, "tools/list", nil, nil)
	if resp.Error != nil {
		t.Fatalf("tools/list: %v", resp.Error)
	}
	var wrapper struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]struct {
					Type string `json:"type"`
				} `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &wrapper); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}

	var seen bool
	for _, tool := range wrapper.Tools {
		switch tool.Name {
		case "update_controller":
			seen = true
			prop, ok := tool.InputSchema.Properties["force"]
			if !ok {
				t.Fatal("update_controller does not declare a force argument")
			}
			if prop.Type != "boolean" {
				t.Errorf("force declared as %q, want boolean", prop.Type)
			}
		case "create_controller":
			if _, ok := tool.InputSchema.Properties["force"]; ok {
				t.Error("create_controller declares force, which it cannot honor")
			}
		}
	}
	if !seen {
		t.Fatal("update_controller missing from tools/list")
	}
}
