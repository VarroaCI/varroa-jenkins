package prompts

import (
	"strings"
	"testing"
)

// TestProposePipelineTextNamesTheTools checks that the proposal prompt names
// the tools and the branch that the proposal uses.
func TestProposePipelineTextNamesTheTools(t *testing.T) {
	t.Parallel()

	p, ok := Get(ProposePipeline)
	if !ok {
		t.Fatal("propose-pipeline is not registered")
	}
	for _, want := range []string{"read_repo_file", "lint_jenkinsfile", "open_pull_request", "varroa/propose-pipeline"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("prompt text does not name %q", want)
		}
	}
	if p.Startable {
		t.Error("propose-pipeline is startable on the public route")
	}
}
