package sessions

import (
	"encoding/json"
	"slices"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/loop"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/accept"
)

// NewDecision returns the write decision of a Team agent session. An output
// tool that the TeamAgent turns on runs at once. A tool on the unattended list
// runs at once too; the creator already cut that list to the eligible set and
// to the global grant. Every other write waits for an approval.
func NewDecision(unattended, outputs []string) func(agent.ToolDef, json.RawMessage) loop.WriteDecision {
	outputTools := accept.OutputToolNames(outputs)
	return func(def agent.ToolDef, _ json.RawMessage) loop.WriteDecision {
		if slices.Contains(outputTools, def.Name) || slices.Contains(unattended, def.Name) {
			return loop.WriteRunNow
		}
		return loop.WriteApprove
	}
}
