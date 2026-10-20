package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/accept"
)

// teamAgentFakes records the calls that the Team agent tools make, and answers
// them from fields the test sets.
type teamAgentFakes struct {
	ta         v1alpha1.TeamAgent
	cs         *v1alpha1.CatalogSource
	readReply  forgeapi.ReadReply
	readReqs   []forgeapi.ReadRequest
	issueReply forgeapi.IssueReply
	issueErr   error
	issueReqs  []forgeapi.IssueRequest
	checkReply forgeapi.CheckReply
	checkReqs  []forgeapi.CheckRequest
	prReply    forgeapi.PullRequestReply
	prErr      error
	prReqs     []forgeapi.PullRequestRequest
	notes      []recordstore.RunNotePostRequest
	hooks      []recordstore.TeamAgentWebhookRequest
	hookReply  recordstore.TeamAgentWebhookResponse
}

func (f *teamAgentFakes) List() []v1alpha1.TeamAgent { return []v1alpha1.TeamAgent{f.ta} }

func (f *teamAgentFakes) ByUID(uid string) (*v1alpha1.TeamAgent, bool) {
	ta := f.ta
	return &ta, string(ta.UID) == uid
}

func (f *teamAgentFakes) AcceptInput() accept.Input {
	return accept.Input{TeamAgents: f.List(), ReleaseNamespace: f.ta.Namespace}
}

func (f *teamAgentFakes) CatalogSource(string, string) (*v1alpha1.CatalogSource, bool) {
	return f.cs, f.cs != nil
}

func (f *teamAgentFakes) Synced() bool { return true }

func (f *teamAgentFakes) Read(_ context.Context, _ string, req forgeapi.ReadRequest) (forgeapi.ReadReply, error) {
	f.readReqs = append(f.readReqs, req)
	return f.readReply, nil
}

func (f *teamAgentFakes) Issue(_ context.Context, _ string, req forgeapi.IssueRequest) (forgeapi.IssueReply, error) {
	f.issueReqs = append(f.issueReqs, req)
	return f.issueReply, f.issueErr
}

func (f *teamAgentFakes) Check(_ context.Context, _ string, req forgeapi.CheckRequest) (forgeapi.CheckReply, error) {
	f.checkReqs = append(f.checkReqs, req)
	return f.checkReply, nil
}

func (f *teamAgentFakes) PullRequest(_ context.Context, _ string, req forgeapi.PullRequestRequest) (forgeapi.PullRequestReply, error) {
	f.prReqs = append(f.prReqs, req)
	return f.prReply, f.prErr
}

func (f *teamAgentFakes) PostRunNote(_ context.Context, req recordstore.RunNotePostRequest) (recordstore.RunNotePostResponse, error) {
	f.notes = append(f.notes, req)
	return recordstore.RunNotePostResponse{Created: true, Note: &recordstore.RunNote{}}, nil
}

func (f *teamAgentFakes) Enqueue(_ context.Context, req recordstore.TeamAgentWebhookRequest) (recordstore.TeamAgentWebhookResponse, error) {
	f.hooks = append(f.hooks, req)
	return f.hookReply, nil
}

// newTeamAgentFakes returns a Team agent named triage on team obj with every
// output on. The forge answers success and the webhook enqueue answers a
// delivery id.
func newTeamAgentFakes() *teamAgentFakes {
	f := &teamAgentFakes{
		readReply:  forgeapi.ReadReply{Status: forgeapi.Status{OK: true}, Revision: "rev-1", Path: "x.md"},
		issueReply: forgeapi.IssueReply{Status: forgeapi.Status{OK: true}, Number: 7, URL: "https://git.example.org/x/y/issues/7"},
		checkReply: forgeapi.CheckReply{Status: forgeapi.Status{OK: true}, Forge: "gitea", BaseBranch: "main", CredentialID: "forge-cred"},
		prReply:    forgeapi.PullRequestReply{Status: forgeapi.Status{OK: true}, Number: 3, URL: "https://git.example.org/x/y/pulls/3"},
		hookReply:  recordstore.TeamAgentWebhookResponse{OK: true, DeliveryID: "delivery-1"},
	}
	f.ta.Name = "triage"
	f.ta.Namespace = "team-a"
	f.ta.UID = "uid-triage"
	f.ta.Spec.TeamRef.Name = "obj"
	f.ta.Spec.Outputs.Notes = true
	f.ta.Spec.Outputs.Webhook = true
	f.ta.Spec.Outputs.Issues = &v1alpha1.TeamAgentIssueOutput{Repo: "https://git.example.org/x/y", Forge: "gitea"}
	f.ta.Spec.Instructions = &v1alpha1.TeamAgentInstructions{Source: "obj-src", Path: ".varroa/team-agent.md"}
	f.cs = &v1alpha1.CatalogSource{}
	f.cs.Spec.RepoURL = "https://git.example.org/x/y"
	f.cs.Status.ObservedRevision = "rev-observed"
	return f
}

// teamAgentDeps returns the guard deps with the Team agent seams replaced by
// fakes, and the activity sink attached.
func teamAgentDeps(t *testing.T, f *teamAgentFakes, sink activity.EventSink) *api.Dependencies {
	t.Helper()
	deps := guardEmitDeps(t, sink)
	deps.TeamAgents = f
	deps.ForgeRelay = f
	deps.WebhookEnqueuer = f
	deps.RunNotePoster = f
	deps.Authorizer = agentAuthorizer()
	return deps
}

// agentAuthorizer grants the admin role to the admins group and to the Team
// agent principal of triage, so both kinds of caller pass the pipeline checks.
func agentAuthorizer() *api.Authorizer {
	roles := []*v1alpha1.VarroaRole{{
		ObjectMeta: metav1.ObjectMeta{Name: "admin"},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}}},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{{
		ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{
				{Kind: "Group", Name: "admins"},
				{Kind: "User", Name: "agent:team-a/obj/triage"},
			},
			RoleRef: "admin",
		},
	}}
	return api.NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false)
}

// readerClaims is a person who can read the pipelines of team obj but cannot
// write them.
func readerClaims() (*auth.Claims, *api.Authorizer) {
	roles := []*v1alpha1.VarroaRole{{
		ObjectMeta: metav1.ObjectMeta{Name: "reader"},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"get", "list", "watch"}}}},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{{
		ObjectMeta: metav1.ObjectMeta{Name: "reader-binding"},
		Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "readers"}}, RoleRef: "reader"},
	}}
	return &auth.Claims{Subject: "rita", Groups: []string{"readers"}},
		api.NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false)
}

// agentClaims is the Team agent principal of triage. The admin binding names
// the principal, so the agent passes the pipeline checks it needs.
func agentClaims() *auth.Claims {
	return &auth.Claims{Subject: "agent:team-a/obj/triage"}
}

// callTeamTool runs one tool call and returns its text and whether it failed.
func callTeamTool(t *testing.T, deps *api.Dependencies, claims *auth.Claims, name string, args map[string]any) (string, bool) {
	t.Helper()
	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]any{"name": name, "arguments": args}, claims)
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	return string(b), strings.Contains(string(b), `"isError":true`)
}

func TestReadRepoFile_OutsideSessionUsesInstructionsSource(t *testing.T) {
	f := newTeamAgentFakes()
	f.readReply.ReadFile = &forgeapi.ReadFile{Size: 5, Content: "hello"}
	deps := teamAgentDeps(t, f, &recordingSink{})
	text, failed := callTeamTool(t, deps, guardClaims, "read_repo_file", map[string]any{"team": "obj", "path": ".varroa/team-agent.md"})
	if failed {
		t.Fatalf("read failed: %s", text)
	}
	if len(f.readReqs) != 1 {
		t.Fatalf("forge reads = %d, want 1", len(f.readReqs))
	}
	got := f.readReqs[0]
	if got.Repo != "https://git.example.org/x/y" || got.Revision != "rev-observed" {
		t.Fatalf("read request = %+v; want the source repo at the observed revision", got)
	}
}

func TestReadRepoFile_RepoOutsideSessionNeedsWrite(t *testing.T) {
	f := newTeamAgentFakes()
	f.readReply.ReadFile = &forgeapi.ReadFile{Size: 5, Content: "hello"}
	deps := teamAgentDeps(t, f, &recordingSink{})
	claims, authz := readerClaims()
	deps.Authorizer = authz
	text, failed := callTeamTool(t, deps, claims, "read_repo_file", map[string]any{
		"team": "obj", "path": "x.md", "repo": "https://git.example.org/other",
	})
	// A reader without a tenant grant is refused at the team check, before the
	// write check that the repo override needs.
	if !failed || (!strings.Contains(text, "team not found") && !strings.Contains(text, "tenant write check")) {
		t.Fatalf("reader read another repo: failed=%v text=%s", failed, text)
	}
	if len(f.readReqs) != 0 {
		t.Fatal("a refused read reached the forge")
	}
}

func TestReadRepoFile_PagesAt32KiB(t *testing.T) {
	f := newTeamAgentFakes()
	f.readReply.ReadFile = &forgeapi.ReadFile{Size: 40000, Content: strings.Repeat("a", 40000)}
	deps := teamAgentDeps(t, f, &recordingSink{})
	text, failed := callTeamTool(t, deps, guardClaims, "read_repo_file", map[string]any{"team": "obj", "path": "big.md", "length": 99999})
	if failed {
		t.Fatalf("read failed: %s", text)
	}
	if !strings.Contains(text, `\"truncated\":true`) {
		t.Fatalf("a 40000-byte file read in one call was not marked truncated: %s", text)
	}
}

func TestReadRepoFile_DirectoryReturnsEntries(t *testing.T) {
	f := newTeamAgentFakes()
	f.readReply.ReadDir = &forgeapi.ReadDir{Entries: []forgeapi.DirEntry{{Name: "a.md"}}}
	deps := teamAgentDeps(t, f, &recordingSink{})
	text, failed := callTeamTool(t, deps, guardClaims, "read_repo_file", map[string]any{"team": "obj", "path": ".varroa"})
	if failed || !strings.Contains(text, "a.md") {
		t.Fatalf("directory read: failed=%v text=%s", failed, text)
	}
}

func TestPostNote_JobNoteCarriesPrincipal(t *testing.T) {
	f := newTeamAgentFakes()
	sink := &recordingSink{}
	deps := teamAgentDeps(t, f, sink)
	text, failed := callTeamTool(t, deps, agentClaims(), "post_note", map[string]any{"team": "obj", "job": "build", "body": "the build failed"})
	if failed {
		t.Fatalf("post_note failed: %s", text)
	}
	if len(f.notes) != 1 || f.notes[0].Principal != "agent:team-a/obj/triage" || f.notes[0].JobPath != "build" {
		t.Fatalf("note post = %+v", f.notes)
	}
	if len(sink.events) != 1 || sink.events[0].Source != "team_agent" {
		t.Fatalf("activity = %+v; want one team_agent event", sink.events)
	}
}

func TestPostNote_AgentNotesOffRefused(t *testing.T) {
	f := newTeamAgentFakes()
	f.ta.Spec.Outputs.Notes = false
	deps := teamAgentDeps(t, f, &recordingSink{})
	text, failed := callTeamTool(t, deps, agentClaims(), "post_note", map[string]any{"team": "obj", "job": "build", "body": "x"})
	if !failed || !strings.Contains(text, "notes are off") || len(f.notes) != 0 {
		t.Fatalf("notes-off agent: failed=%v text=%s posts=%d", failed, text, len(f.notes))
	}
}

func TestPostNote_BodyLimit(t *testing.T) {
	f := newTeamAgentFakes()
	deps := teamAgentDeps(t, f, &recordingSink{})
	_, failed := callTeamTool(t, deps, guardClaims, "post_note", map[string]any{"team": "obj", "job": "build", "body": strings.Repeat("x", 16385)})
	if !failed || len(f.notes) != 0 {
		t.Fatal("an oversize note reached the recorder")
	}
}

func TestOpenIssue_AgentOwnTeamPasses(t *testing.T) {
	f := newTeamAgentFakes()
	sink := &recordingSink{}
	deps := teamAgentDeps(t, f, sink)
	text, failed := callTeamTool(t, deps, agentClaims(), "open_issue", map[string]any{"team": "obj", "title": "deploy failed", "body": "see run", "labels": []any{"ci"}})
	if failed || !strings.Contains(text, `\"number\":7`) {
		t.Fatalf("open_issue: failed=%v text=%s", failed, text)
	}
	if len(f.issueReqs) != 1 || f.issueReqs[0].TeamAgent.Name != "triage" || f.issueReqs[0].Labels[0] != "ci" {
		t.Fatalf("issue request = %+v", f.issueReqs)
	}
	if len(sink.events) != 1 || sink.events[0].Type != "team_agent.issue_opened" {
		t.Fatalf("activity = %+v", sink.events)
	}
}

// TestOpenIssue_RelayTimeoutAnswersOperatorTimeout checks that a relay timeout
// answers operator_timeout, and never points at gitEgress.
func TestOpenIssue_RelayTimeoutAnswersOperatorTimeout(t *testing.T) {
	f := newTeamAgentFakes()
	f.issueErr = fmt.Errorf("forge issue request: %w", nats.ErrTimeout)
	deps := teamAgentDeps(t, f, &recordingSink{})
	text, failed := callTeamTool(t, deps, agentClaims(), "open_issue", map[string]any{"team": "obj", "title": "t"})
	if !failed || !strings.Contains(text, "operator_timeout: ") || strings.Contains(text, "gitEgress") {
		t.Fatalf("failed=%v text=%s", failed, text)
	}
}

// TestOpenIssue_NoResponderAnswersNoIssueOpened checks that a request that no
// operator received says no issue was opened, and does not say the issue can exist.
func TestOpenIssue_NoResponderAnswersNoIssueOpened(t *testing.T) {
	f := newTeamAgentFakes()
	f.issueErr = fmt.Errorf("forge issue request: %w", nats.ErrNoResponders)
	deps := teamAgentDeps(t, f, &recordingSink{})
	text, failed := callTeamTool(t, deps, agentClaims(), "open_issue", map[string]any{"team": "obj", "title": "t"})
	if !failed || !strings.Contains(text, "No issue was opened") || strings.Contains(text, "operator_timeout") {
		t.Fatalf("failed=%v text=%s", failed, text)
	}
}

// TestOpenIssue_RequestExpiredKeepsCode checks that a refused issue keeps its
// reply code in the tool error.
func TestOpenIssue_RequestExpiredKeepsCode(t *testing.T) {
	f := newTeamAgentFakes()
	f.issueReply = forgeapi.IssueReply{Status: forgeapi.Status{Code: forgeapi.CodeRequestExpired, Error: "too little time"}}
	deps := teamAgentDeps(t, f, &recordingSink{})
	text, failed := callTeamTool(t, deps, agentClaims(), "open_issue", map[string]any{"team": "obj", "title": "t"})
	if !failed || !strings.Contains(text, "request_expired: ") {
		t.Fatalf("failed=%v text=%s", failed, text)
	}
}

func TestOpenIssue_IssuesOffRefused(t *testing.T) {
	f := newTeamAgentFakes()
	f.ta.Spec.Outputs.Issues = nil
	deps := teamAgentDeps(t, f, &recordingSink{})
	text, failed := callTeamTool(t, deps, agentClaims(), "open_issue", map[string]any{"team": "obj", "title": "t"})
	if !failed || !strings.Contains(text, "issues are off") || len(f.issueReqs) != 0 {
		t.Fatalf("issues-off: failed=%v text=%s", failed, text)
	}
}

func TestOpenIssue_ReaderRefused(t *testing.T) {
	f := newTeamAgentFakes()
	deps := teamAgentDeps(t, f, &recordingSink{})
	claims, authz := readerClaims()
	deps.Authorizer = authz
	text, failed := callTeamTool(t, deps, claims, "open_issue", map[string]any{"team": "obj", "title": "t"})
	if !failed || (!strings.Contains(text, "tenant write check") && !strings.Contains(text, "team not found")) || len(f.issueReqs) != 0 {
		t.Fatalf("reader: failed=%v text=%s", failed, text)
	}
}

func TestSendWebhook_OutputOffRefused(t *testing.T) {
	f := newTeamAgentFakes()
	f.ta.Spec.Outputs.Webhook = false
	deps := teamAgentDeps(t, f, &recordingSink{})
	text, failed := callTeamTool(t, deps, agentClaims(), "send_webhook", map[string]any{"team": "obj", "body": map[string]any{"k": "v"}})
	if !failed || !strings.Contains(text, "webhook output is off") || len(f.hooks) != 0 {
		t.Fatalf("output off: failed=%v text=%s", failed, text)
	}
}

func TestSendWebhook_Enqueues(t *testing.T) {
	f := newTeamAgentFakes()
	sink := &recordingSink{}
	deps := teamAgentDeps(t, f, sink)
	text, failed := callTeamTool(t, deps, agentClaims(), "send_webhook", map[string]any{"team": "obj", "body": map[string]any{"k": "v"}})
	if failed || !strings.Contains(text, "delivery-1") {
		t.Fatalf("send_webhook: failed=%v text=%s", failed, text)
	}
	if len(f.hooks) != 1 || f.hooks[0].Principal != "agent:team-a/obj/triage" || f.hooks[0].CallID == "" {
		t.Fatalf("enqueue = %+v", f.hooks)
	}
	if len(sink.events) != 1 || sink.events[0].Type != "team_agent.webhook_sent" {
		t.Fatalf("activity = %+v", sink.events)
	}
}
