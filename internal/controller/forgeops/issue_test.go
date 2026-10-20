package forgeops

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

const issueUID = "uid-agent-a"

// seedAgent seeds one TeamAgent in the namespace default, owned by Team
// payments. Its issue output names the test repository on the fixture forge.
func seedAgent(t *testing.T, fx *fixture, mutate func(*v1alpha1.TeamAgent)) {
	t.Helper()
	ta := &v1alpha1.TeamAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-a", Namespace: "default", UID: issueUID},
		Spec: v1alpha1.TeamAgentSpec{
			TeamRef: v1alpha1.TeamAgentTeamRef{Name: "payments"},
			Outputs: v1alpha1.TeamAgentOutputs{Issues: &v1alpha1.TeamAgentIssueOutput{
				Repo: testRepoURL, Forge: "gitea", Labels: []string{"ops", "bug"},
			}},
		},
	}
	if mutate != nil {
		mutate(ta)
	}
	crdstore.MustSeed(fx.store, ta)
}

func issueReq(labels ...string) forgeapi.IssueRequest {
	return forgeapi.IssueRequest{
		Team:      "payments",
		TeamAgent: forgeapi.TeamAgentRef{Namespace: "default", Name: "agent-a", UID: issueUID},
		Title:     "Build is flaky",
		Body:      "Three runs failed.",
		Labels:    labels,
		Actor:     "agent:default/payments/agent-a",
		SessionID: "s-1",
	}
}

// TestIssue_OpensOnGitea checks the happy path on a Gitea forge: the issue is
// opened with the object's labels and the footer naming the session.
func TestIssue_OpensOnGitea(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	seedAgent(t, fx, nil)
	c := pushable()
	oneWriter(fx, c)
	rep := fx.ops.Issue(context.Background(), issueReq("ops"), "varroa-system")
	if !rep.OK || rep.Number != 9 || rep.URL == "" {
		t.Fatalf("reply %+v", rep)
	}
	if len(c.issueLabels) != 1 || len(c.issueLabels[0]) != 2 {
		t.Fatalf("labels sent = %v, want the object labels", c.issueLabels)
	}
}

// TestIssue_GitHubForge checks the same path on a GitHub forge.
func TestIssue_GitHubForge(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	fx.kind = forge.KindGitHub
	seedAgent(t, fx, func(ta *v1alpha1.TeamAgent) { ta.Spec.Outputs.Issues.Forge = "github" })
	c := pushable()
	oneWriter(fx, c)
	if rep := fx.ops.Issue(context.Background(), issueReq(), "varroa-system"); !rep.OK {
		t.Fatalf("reply %+v", rep)
	}
}

func TestIssue_Refusals(t *testing.T) {
	type tc struct {
		name   string
		mutate func(*v1alpha1.TeamAgent)
		req    func(forgeapi.IssueRequest) forgeapi.IssueRequest
		setup  func(*testing.T, *fixture)
		code   string
	}
	writer := func(fx *fixture) { oneWriter(fx, pushable()) }
	cases := []tc{
		{name: "UIDMismatch", req: func(r forgeapi.IssueRequest) forgeapi.IssueRequest { r.TeamAgent.UID = "other"; return r },
			setup: func(t *testing.T, fx *fixture) { writer(fx) }, code: forgeapi.CodeInvalidRequest},
		{name: "NotAccepted", mutate: func(ta *v1alpha1.TeamAgent) { ta.Namespace = "elsewhere"; ta.Name = "agent-x" },
			req: func(r forgeapi.IssueRequest) forgeapi.IssueRequest {
				r.TeamAgent.Namespace, r.TeamAgent.Name = "elsewhere", "agent-x"
				return r
			}, setup: func(t *testing.T, fx *fixture) { writer(fx) }, code: forgeapi.CodeInvalidRequest},
		{name: "OutputOff", mutate: func(ta *v1alpha1.TeamAgent) { ta.Spec.Outputs.Issues = nil },
			setup: func(t *testing.T, fx *fixture) { writer(fx) }, code: forgeapi.CodeInvalidRequest},
		{name: "LabelOutsideSet", req: func(r forgeapi.IssueRequest) forgeapi.IssueRequest { r.Labels = []string{"secret"}; return r },
			setup: func(t *testing.T, fx *fixture) { writer(fx) }, code: forgeapi.CodeInvalidRequest},
		{name: "ForgeMismatch", mutate: func(ta *v1alpha1.TeamAgent) { ta.Spec.Outputs.Issues.Forge = "github" },
			setup: func(t *testing.T, fx *fixture) { writer(fx) }, code: forgeapi.CodeForgeUnsupported},
		{name: "NoCredential", setup: func(t *testing.T, fx *fixture) {}, code: forgeapi.CodeNoMatchingCredential},
		{name: "NoReadableCredential", setup: func(t *testing.T, fx *fixture) {
			fx.secrets.addCred("sa", "a", "tok-a")
			fx.clients[forge.NewToken([]byte("tok-a"))] = &fakeClient{repoErr: forge.ErrNotFound}
		}, code: forgeapi.CodeForgeAuthFailed},
		{name: "LabelMissingInRepository", setup: func(t *testing.T, fx *fixture) {
			c := pushable()
			c.issueErr = &forge.LabelError{Name: "ops"}
			oneWriter(fx, c)
		}, code: forgeapi.CodeInvalidRequest},
		{name: "Unreachable", setup: func(t *testing.T, fx *fixture) {
			fx.secrets.addCred("sa", "a", "tok-a")
			fx.clients[forge.NewToken([]byte("tok-a"))] = pushable()
			c := pushable()
			c.issueErr = errors.New("connection refused")
			fx.clients[forge.NewToken([]byte("tok-a"))] = c
		}, code: forgeapi.CodeForgeUnreachable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newFixture(t, "agents-payments")
			seedAgent(t, fx, c.mutate)
			c.setup(t, fx)
			req := issueReq()
			if c.req != nil {
				req = c.req(req)
			}
			rep := fx.ops.Issue(context.Background(), req, "varroa-system")
			if rep.OK || rep.Code != c.code {
				t.Fatalf("reply %+v, want code %s", rep, c.code)
			}
		})
	}
}

// TestIssue_ForbiddenNamesHint checks that a 403 on the write names the
// credential and the scope hint, and never the token.
func TestIssue_ForbiddenNamesHint(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	seedAgent(t, fx, nil)
	c := pushable()
	c.issueErr = forge.ErrForbidden
	oneWriter(fx, c)
	rep := fx.ops.Issue(context.Background(), issueReq("ops"), "varroa-system")
	if rep.OK || rep.Code != forgeapi.CodeForgeWriteDenied {
		t.Fatalf("reply %+v, want code %s", rep, forgeapi.CodeForgeWriteDenied)
	}
	for _, want := range []string{`credential "a" is forbidden`, "check the token scope and the repository access of the credential"} {
		if !strings.Contains(rep.Error, want) {
			t.Errorf("reply error %q lacks %q", rep.Error, want)
		}
	}
	if strings.Contains(rep.Error, "tok-a") {
		t.Errorf("reply error %q leaks the token", rep.Error)
	}
}

// TestIssue_ReadOnlyCredentialOpens checks that a credential that reads the
// repository but cannot push opens the issue.
func TestIssue_ReadOnlyCredentialOpens(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	seedAgent(t, fx, nil)
	c := pushable()
	c.info.Push = false
	oneWriter(fx, c)
	rep := fx.ops.Issue(context.Background(), issueReq("ops"), "varroa-system")
	if !rep.OK || rep.Number != 9 {
		t.Fatalf("reply %+v", rep)
	}
	if len(c.issueLabels) != 1 {
		t.Fatalf("CreateIssue calls = %d, want 1", len(c.issueLabels))
	}
}

// TestIssue_ReadOnlyForbiddenNamesHint checks that a read-only credential
// without issue scope gets the forge 403 as forge_write_denied with the hint.
func TestIssue_ReadOnlyForbiddenNamesHint(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	seedAgent(t, fx, nil)
	c := pushable()
	c.info.Push = false
	c.issueErr = forge.ErrForbidden
	oneWriter(fx, c)
	rep := fx.ops.Issue(context.Background(), issueReq("ops"), "varroa-system")
	if rep.OK || rep.Code != forgeapi.CodeForgeWriteDenied {
		t.Fatalf("reply %+v, want code %s", rep, forgeapi.CodeForgeWriteDenied)
	}
	if !strings.Contains(rep.Error, "check the token scope") {
		t.Fatalf("reply error %q lacks the scope hint", rep.Error)
	}
}

// TestIssue_PushingCredentialPreferred checks that a pushing credential opens
// the issue even when a read-only credential sorts before it.
func TestIssue_PushingCredentialPreferred(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	seedAgent(t, fx, nil)
	fx.secrets.addCred("sa", "a", "tok-a")
	fx.secrets.addCred("sb", "b", "tok-b")
	readOnly := pushable()
	readOnly.info.Push = false
	pusher := pushable()
	fx.clients[forge.NewToken([]byte("tok-a"))] = readOnly
	fx.clients[forge.NewToken([]byte("tok-b"))] = pusher
	rep := fx.ops.Issue(context.Background(), issueReq("ops"), "varroa-system")
	if !rep.OK {
		t.Fatalf("reply %+v", rep)
	}
	if len(readOnly.issueLabels) != 0 || len(pusher.issueLabels) != 1 {
		t.Fatalf("CreateIssue calls read-only=%d pusher=%d, want 0 and 1", len(readOnly.issueLabels), len(pusher.issueLabels))
	}
}

// twoCredentials registers credential a (tok-a) and credential b (tok-b) with
// the given clients. The ids sort a before b.
func twoCredentials(fx *fixture, a, b forge.Client) {
	fx.secrets.addCred("sa", "a", "tok-a")
	fx.secrets.addCred("sb", "b", "tok-b")
	fx.clients[forge.NewToken([]byte("tok-a"))] = a
	fx.clients[forge.NewToken([]byte("tok-b"))] = b
}

// TestIssue_ForbiddenPusherFallsBackToReader checks that a 403 from a pushing
// credential moves the issue to a read-only credential with issue scope.
func TestIssue_ForbiddenPusherFallsBackToReader(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	seedAgent(t, fx, nil)
	a := pushable()
	a.issueErr = forge.ErrForbidden
	b := pushable()
	b.info.Push = false
	twoCredentials(fx, a, b)
	rep := fx.ops.Issue(context.Background(), issueReq("ops"), "varroa-system")
	if !rep.OK || rep.Number != 9 {
		t.Fatalf("reply %+v", rep)
	}
	if len(a.issueLabels) != 1 || len(b.issueLabels) != 1 {
		t.Fatalf("CreateIssue calls a=%d b=%d, want 1 and 1", len(a.issueLabels), len(b.issueLabels))
	}
	log := fx.logBuf.String()
	if !strings.Contains(log, "forge issue opened") || !strings.Contains(log, `"credential":"b"`) {
		t.Fatalf("log %q lacks the opened line for credential b", log)
	}
}

// TestIssue_ForbiddenPusherFallsBackToNextPusher checks that the fallback
// stops at the first credential that answers, and skips the rest.
func TestIssue_ForbiddenPusherFallsBackToNextPusher(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	seedAgent(t, fx, nil)
	fx.secrets.addCred("sa", "a", "tok-a")
	fx.secrets.addCred("sb", "b", "tok-b")
	fx.secrets.addCred("sc", "c", "tok-c")
	a := pushable()
	a.issueErr = forge.ErrForbidden
	b := pushable()
	c := pushable()
	c.info.Push = false
	fx.clients[forge.NewToken([]byte("tok-a"))] = a
	fx.clients[forge.NewToken([]byte("tok-b"))] = b
	fx.clients[forge.NewToken([]byte("tok-c"))] = c
	rep := fx.ops.Issue(context.Background(), issueReq("ops"), "varroa-system")
	if !rep.OK {
		t.Fatalf("reply %+v", rep)
	}
	if len(b.issueLabels) != 1 || len(c.issueLabels) != 0 {
		t.Fatalf("CreateIssue calls b=%d c=%d, want 1 and 0", len(b.issueLabels), len(c.issueLabels))
	}
}

// TestIssue_AllForbiddenRefuses checks that every 403 refuses the issue, and
// that the reply names each credential and no token.
func TestIssue_AllForbiddenRefuses(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	seedAgent(t, fx, nil)
	a := pushable()
	a.issueErr = forge.ErrForbidden
	b := pushable()
	b.info.Push = false
	b.issueErr = forge.ErrForbidden
	twoCredentials(fx, a, b)
	rep := fx.ops.Issue(context.Background(), issueReq("ops"), "varroa-system")
	if rep.OK || rep.Code != forgeapi.CodeForgeWriteDenied {
		t.Fatalf("reply %+v, want code %s", rep, forgeapi.CodeForgeWriteDenied)
	}
	for _, want := range []string{`"a"`, `"b"`, "check the token scope"} {
		if !strings.Contains(rep.Error, want) {
			t.Errorf("reply error %q lacks %q", rep.Error, want)
		}
	}
	if strings.Contains(rep.Error, "tok-") {
		t.Errorf("reply error %q leaks a token", rep.Error)
	}
	if len(a.issueLabels) != 1 || len(b.issueLabels) != 1 {
		t.Fatalf("CreateIssue calls a=%d b=%d, want 1 and 1", len(a.issueLabels), len(b.issueLabels))
	}
}

// TestIssue_NoFallbackOnOtherError checks that an error other than 403 stops
// the attempt, and that the read-only credential is never tried.
func TestIssue_NoFallbackOnOtherError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"Auth", forge.ErrAuth, forgeapi.CodeForgeAuthFailed},
		{"Unreachable", &forge.StatusError{Status: 500}, forgeapi.CodeForgeUnreachable},
		{"NotFound", forge.ErrNotFound, forgeapi.CodeNotFound},
		{"LabelError", &forge.LabelError{Name: "ops"}, forgeapi.CodeInvalidRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newFixture(t, "agents-payments")
			seedAgent(t, fx, nil)
			a := pushable()
			a.issueErr = c.err
			b := pushable()
			b.info.Push = false
			twoCredentials(fx, a, b)
			rep := fx.ops.Issue(context.Background(), issueReq("ops"), "varroa-system")
			if rep.OK || rep.Code != c.code {
				t.Fatalf("reply %+v, want code %s", rep, c.code)
			}
			if len(b.issueLabels) != 0 {
				t.Fatalf("read-only credential got %d CreateIssue calls, want 0", len(b.issueLabels))
			}
		})
	}
}

// TestIssue_LaterProbeErrorDoesNotBlock checks that a failed repository probe
// on one credential does not stop a healthy credential from opening the issue.
func TestIssue_LaterProbeErrorDoesNotBlock(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	seedAgent(t, fx, nil)
	b := pushable()
	b.repoErr = &forge.StatusError{Status: 500}
	twoCredentials(fx, pushable(), b)
	if rep := fx.ops.Issue(context.Background(), issueReq("ops"), "varroa-system"); !rep.OK {
		t.Fatalf("reply %+v", rep)
	}
}

// TestIssue_ProbeErrorOnlyCandidateRefuses checks that a probe error on the
// only credential refuses the issue as unreachable.
func TestIssue_ProbeErrorOnlyCandidateRefuses(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	seedAgent(t, fx, nil)
	a := pushable()
	a.repoErr = &forge.StatusError{Status: 500}
	oneWriter(fx, a)
	rep := fx.ops.Issue(context.Background(), issueReq("ops"), "varroa-system")
	if rep.OK || rep.Code != forgeapi.CodeForgeUnreachable {
		t.Fatalf("reply %+v, want code %s", rep, forgeapi.CodeForgeUnreachable)
	}
}
