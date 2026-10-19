package forgeops

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// TestCanaryToken_NeverInReplyOrLog runs check and read through each error
// path with a credential value that is easy to find. The value must reach
// neither a reply nor a log line.
func TestCanaryToken_NeverInReplyOrLog(t *testing.T) {
	const canary = "canary-tok"
	errs := []error{
		forge.ErrAuth,
		forge.ErrForbidden,
		forge.ErrNotFound,
		forge.ErrRedirect,
		&forge.StatusError{Status: 500},
		errors.New("dial tcp 192.0.2.1:443: i/o timeout"),
	}
	for _, e := range errs {
		fx := newFixture(t, "agents-payments")
		fx.secrets.addCred("sa", "a", canary)
		fx.clients[forge.NewToken([]byte(canary))] = &fakeClient{repoErr: e}
		out := make([]byte, 0, 256)
		b, _ := json.Marshal(fx.ops.Check(context.Background(), checkReq()))
		out = append(out, b...)
		b, _ = json.Marshal(fx.ops.Read(context.Background(), readReq("a.txt")))
		out = append(out, b...)
		b, _ = json.Marshal(fx.ops.PullRequest(context.Background(), prReq()))
		out = append(out, b...)
		if strings.Contains(string(out), canary) || strings.Contains(fx.logBuf.String(), canary) {
			t.Fatalf("token value reached a reply or a log for error %v", e)
		}
	}
}

// TestCanaryToken_ReplyCodes confirms that the forge error maps to the reply
// code that the requester sees, not to the text of the error.
func TestCanaryToken_ReplyCodes(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	fx.secrets.addCred("sa", "a", "canary-tok")
	fx.clients[forge.NewToken([]byte("canary-tok"))] = &fakeClient{repoErr: &forge.StatusError{Status: 502}}
	rep := fx.ops.Check(context.Background(), checkReq())
	if rep.Code != forgeapi.CodeForgeUnreachable || strings.Contains(rep.Error, "canary") {
		t.Fatalf("reply %+v", rep.Status)
	}
}
