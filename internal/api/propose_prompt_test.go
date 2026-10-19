package api

import (
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent/prompts"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// TestProposePromptBranchIsProposalBranch checks that the prompt names the
// branch that the operator writes, so the model and the operator agree.
func TestProposePromptBranchIsProposalBranch(t *testing.T) {
	p, ok := prompts.Get(prompts.ProposePipeline)
	if !ok || !strings.Contains(p.Text, forgeapi.ProposalBranch) {
		t.Fatalf("prompt does not name %q", forgeapi.ProposalBranch)
	}
}
