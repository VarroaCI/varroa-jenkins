package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestNoToolDeclaresATeamParameter asserts that no tool input schema names
// a property that starts with "team". Tenant is the only name for the
// pipelines unit, so a team argument is a stale contract.
func TestNoToolDeclaresATeamParameter(t *testing.T) {
	resp := mcpRequest(t, NewHandler(localUserDeps(crdstore.NewFake())), "tools/list", map[string]interface{}{}, mcpAdminClaims)
	if resp.Error != nil {
		t.Fatalf("tools/list returned error: %s", resp.Error)
	}
	var list struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &list); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	if len(list.Tools) == 0 {
		t.Fatal("tools/list returned no tools")
	}
	for _, tool := range list.Tools {
		for prop := range tool.InputSchema.Properties {
			if strings.HasPrefix(strings.ToLower(prop), "team") {
				t.Errorf("tool %s declares a team parameter %q", tool.Name, prop)
			}
		}
	}
}
