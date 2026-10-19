package forgeops

import (
	"context"
	"errors"
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
		{name: "ReadOnlyCredential", setup: func(t *testing.T, fx *fixture) {
			fx.secrets.addCred("sa", "a", "tok-a")
			fx.clients[forge.NewToken([]byte("tok-a"))] = &fakeClient{info: forge.Info{DefaultBranch: "main"}}
		}, code: forgeapi.CodeForgeWriteDenied},
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
