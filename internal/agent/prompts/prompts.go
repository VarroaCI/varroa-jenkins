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
		ID: ProposePipeline,
		Text: `Propose a first Jenkinsfile for the repository in the session context. Do these steps in order.
1. Call read_repo_file with the team and the repo of the context and the path "." to list the repository root.
2. Read the build files that you find, such as go.mod, package.json, pom.xml, build.gradle, Makefile, or Dockerfile. Use read_repo_file and always pass the repo of the context.
3. Write one complete declarative Jenkinsfile that builds and tests the project.
4. Call open_pull_request one time. Use the team and the repo of the context, the branch "varroa/propose-pipeline", and the title "Add a Jenkinsfile proposed by Varroa". Set files to one entry with the path "Jenkinsfile" and the file text as content. Set body to a short paragraph that tells why the file fits the project, then this line: "Varroa builds this file after the team's bundle declares this repository."
Do not call any other write tool. After the tool result, give me the pull request link.`,
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
