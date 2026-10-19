package forgeurl

import (
	"os/exec"
	"strings"
	"testing"
)

// TestForgeurlIsLeaf checks that the rule package imports no module package,
// so the API tier can apply the rules without a dependency cycle.
func TestForgeurlIsLeaf(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", `{{join .Imports " "}}`,
		"github.com/varroaci/varroa-jenkins/internal/forgeurl").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, imp := range strings.Fields(string(out)) {
		if strings.HasPrefix(imp, "github.com/") {
			t.Fatalf("forgeurl imports %s, which breaks its leaf shape", imp)
		}
	}
}
