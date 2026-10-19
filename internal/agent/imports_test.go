package agent_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestAgentImportsNoMCPOrAPI proves the agent packages stay below the MCP and
// HTTP layers. The loop reaches a tool only through its ToolHost, so an import
// of mcp or api would let the loop bypass the host and the grant check.
func TestAgentImportsNoMCPOrAPI(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", `{{.ImportPath}} {{join .Imports " "}}`,
		"github.com/varroaci/varroa-jenkins/internal/agent/...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	banned := []string{
		"github.com/varroaci/varroa-jenkins/internal/mcp",
		"github.com/varroaci/varroa-jenkins/internal/api",
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		for _, imp := range fields[1:] {
			for _, b := range banned {
				if strings.HasPrefix(imp, b) {
					t.Errorf("%s imports %s", fields[0], imp)
				}
			}
		}
	}
}
