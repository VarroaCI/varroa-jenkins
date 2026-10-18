// Package prompts holds the fixed prompts that a person may start a session
// from. The text is fixed, so a prompt id always means the same request.
package prompts

import "sort"

// Prompt is one fixed prompt.
type Prompt struct {
	ID        string
	Text      string
	Startable bool
}

// The prompt ids.
const (
	ExplainFailure  = "explain-failure"
	ProposePipeline = "propose-pipeline"
)

var all = []Prompt{
	{
		ID:        ExplainFailure,
		Text:      "Explain why this run failed. Name the stage and the error. Tell me the next step to fix it.",
		Startable: true,
	},
	{
		ID:        ProposePipeline,
		Text:      "Read the repository in the session context. Propose a Jenkinsfile for it.",
		Startable: false,
	},
}

// Get returns the prompt with the id, and false when no prompt has it.
func Get(id string) (Prompt, bool) {
	for _, p := range all {
		if p.ID == id {
			return p, true
		}
	}
	return Prompt{}, false
}

// All returns every prompt, sorted by id.
func All() []Prompt {
	out := make([]Prompt, len(all))
	copy(out, all)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
