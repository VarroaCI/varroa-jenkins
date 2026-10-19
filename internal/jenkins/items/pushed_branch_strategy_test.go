package items

import (
	"strings"
	"testing"
)

const (
	skipInitialBuildElement = "<jenkins.branch.buildstrategies.basic.SkipInitialBuildOnFirstBranchIndexing plugin=\"basic-branch-build-strategies\"></jenkins.branch.buildstrategies.basic.SkipInitialBuildOnFirstBranchIndexing>"
	pushedBranchElement     = "<org.jenkinsci.plugins.varroamiteauth.VarroaPushedBranchBuildStrategy plugin=\"varroa-mite-auth\"></org.jenkinsci.plugins.varroamiteauth.VarroaPushedBranchBuildStrategy>"
)

// TestBranchBuildStrategiesCarryPushedBranchStrategy checks that every
// branch-indexing item keeps the skip strategy and adds the Varroa strategy
// that builds a branch a push creates.
func TestBranchBuildStrategiesCarryPushedBranchStrategy(t *testing.T) {
	tests := []struct {
		name string
		item Item
		want bool
	}{
		{"multibranch git source", Item{Kind: "multibranch", Name: "branches",
			SourcesList: []BranchSourceEntry{{Git: &GitSCMSource{Remote: "https://example.com/r.git"}}}}, true},
		{"organization folder", orgFolderWithGitea(), true},
		{"pipeline has no branch strategies", Item{Kind: "pipeline", Name: "p"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			xml := renderOrgFolder(t, tt.item)
			hasSkip := strings.Contains(xml, skipInitialBuildElement)
			hasPushed := strings.Contains(xml, pushedBranchElement)
			if hasSkip != tt.want {
				t.Errorf("skip strategy present = %v, want %v:\n%s", hasSkip, tt.want, xml)
			}
			if hasPushed != tt.want {
				t.Errorf("pushed-branch strategy present = %v, want %v:\n%s", hasPushed, tt.want, xml)
			}
		})
	}
}
