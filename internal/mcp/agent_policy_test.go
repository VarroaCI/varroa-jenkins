package mcp

import (
	"slices"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/datashare"
	"github.com/varroaci/varroa-jenkins/internal/agent/policy"
)

// laterChangeTools names the tools of the agent policy that a later change
// adds. A listed name in this map may be absent from the live registry.
var laterChangeTools = map[string]bool{"open_pull_request": true}

// liveByName indexes the live tool list by name.
func liveByName(t *testing.T) map[string]listedTool {
	t.Helper()
	out := map[string]listedTool{}
	for _, tool := range liveTools(t) {
		out[tool.Name] = tool
	}
	return out
}

// TestPolicyListsNameRegisteredTools fails when a policy list names a tool
// the registry no longer has. Renaming a listed tool makes this fail.
func TestPolicyListsNameRegisteredTools(t *testing.T) {
	t.Parallel()

	live := liveByName(t)
	for _, list := range [][]string{policy.UnattendedEligible, policy.NeverUnattended} {
		for _, name := range list {
			if laterChangeTools[name] {
				continue
			}
			if _, ok := live[name]; !ok {
				t.Fatalf("policy names %s, which the live tool registry does not register", name)
			}
		}
	}
}

// TestEveryUneligibleWriteIsNeverUnattended asserts that a write tool outside
// the eligible set always needs an approval.
func TestEveryUneligibleWriteIsNeverUnattended(t *testing.T) {
	t.Parallel()

	for _, tool := range liveTools(t) {
		class, _ := policy.ClassFromHints(tool.Annotations.ReadOnlyHint, tool.Annotations.OpenWorldHint)
		if class != agent.ClassWrite || slices.Contains(policy.UnattendedEligible, tool.Name) {
			continue
		}
		def := agent.ToolDef{Name: tool.Name, Class: class}
		if !policy.IsNeverUnattended(def, nil) {
			t.Fatalf("write tool %s is outside the eligible set but may run unattended", tool.Name)
		}
	}
}

// TestEveryReadToolHasDataClass asserts that every live read tool has a data
// class, so the data-sharing filter can place it.
func TestEveryReadToolHasDataClass(t *testing.T) {
	t.Parallel()

	for _, tool := range liveTools(t) {
		class, _ := policy.ClassFromHints(tool.Annotations.ReadOnlyHint, tool.Annotations.OpenWorldHint)
		if class != agent.ClassRead {
			continue
		}
		if _, ok := datashare.ClassOf(tool.Name); !ok {
			t.Fatalf("read tool %s has no data class", tool.Name)
		}
	}
}
