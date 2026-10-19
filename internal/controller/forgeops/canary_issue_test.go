package forgeops

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/forge"
)

// TestIssueCanaryTokenNeverLeaves runs the issue path through each forge
// error with a credential value that is easy to find. The value must reach
// neither the reply nor a log line.
func TestIssueCanaryTokenNeverLeaves(t *testing.T) {
	const canary = "canary-issue-tok"
	errs := []error{
		forge.ErrAuth,
		forge.ErrForbidden,
		forge.ErrNotFound,
		&forge.StatusError{Status: 500},
		errors.New("post https://git.example.org: canary-issue-tok rejected"),
	}
	for _, e := range errs {
		fx := newFixture(t, "agents-payments")
		seedAgent(t, fx, nil)
		fx.secrets.addCred("sa", "a", canary)
		c := pushable()
		c.issueErr = e
		fx.clients[forge.NewToken([]byte(canary))] = c
		rep := fx.ops.Issue(context.Background(), issueReq("ops"), "varroa-system")
		b, err := json.Marshal(rep)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), canary) || strings.Contains(rep.Error, canary) || strings.Contains(fx.logBuf.String(), canary) {
			t.Fatalf("token value reached a reply or a log for error %v", e)
		}
	}
}
