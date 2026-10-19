package mcp

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/policy"
)

// TestOpenPullRequestIsRegisteredWrite checks that the proposal tool is live,
// classed as a write, and never runs unattended.
func TestOpenPullRequestIsRegisteredWrite(t *testing.T) {
	tool, ok := liveByName(t)["open_pull_request"]
	if !ok {
		t.Fatal("open_pull_request is not registered")
	}
	class, _ := policy.ClassFromHints(tool.Annotations.ReadOnlyHint, tool.Annotations.OpenWorldHint)
	if class != agent.ClassWrite {
		t.Fatalf("class = %v, want write", class)
	}
	if !policy.IsNeverUnattended(agent.ToolDef{Name: tool.Name, Class: class}, nil) {
		t.Fatal("open_pull_request may run unattended")
	}
}
