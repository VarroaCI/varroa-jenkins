// Package policy holds the tool policy of the assistant: how a tool's MCP
// hints map to a class, which tools may run unattended, and what target a
// tool call names.
package policy

import (
	"slices"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// UnattendedEligible lists the write tools that a team agent may run
// without a person's approval. A write tool outside this list needs an
// approval for every call.
var UnattendedEligible = []string{"run_pipeline", "restart_run", "scan_multibranch"}

// NeverUnattended lists the write tools that always need a person's
// approval, whatever the output of the call.
var NeverUnattended = []string{
	"replay_run", "proceed_run_input", "abort_run_input", "release_fleet_lock",
	"restart_controller", "hibernate_controller", "wake_controller", "reconcile_controller",
	"open_pull_request",
}

// ClassFromHints maps the MCP read-only and open-world hints to a tool
// class and a visibility flag. A tool is read only when readOnly is set to
// true. A tool is hidden from the model when openWorld is set to true.
func ClassFromHints(readOnly, openWorld *bool) (class agent.ToolClass, hidden bool) {
	class = agent.ClassWrite
	if readOnly != nil && *readOnly {
		class = agent.ClassRead
	}
	hidden = openWorld != nil && *openWorld
	return class, hidden
}

// Visible reports whether the model may see the tool.
func Visible(t agent.ToolDef) bool { return !t.Hidden }

// Class returns the class of the tool. A tool is read only when it is
// declared read, and write otherwise.
func Class(t agent.ToolDef) agent.ToolClass {
	if t.Class == agent.ClassRead {
		return agent.ClassRead
	}
	return agent.ClassWrite
}

// IsNeverUnattended reports whether a call of the tool needs an approval
// in every case. A tool in NeverUnattended always needs one. A read tool
// never does. An eligible tool, or a tool named in output, does not need
// one. Every other write tool needs one.
func IsNeverUnattended(t agent.ToolDef, output []string) bool {
	if slices.Contains(NeverUnattended, t.Name) {
		return true
	}
	if Class(t) == agent.ClassRead {
		return false
	}
	if slices.Contains(UnattendedEligible, t.Name) || slices.Contains(output, t.Name) {
		return false
	}
	return true
}
