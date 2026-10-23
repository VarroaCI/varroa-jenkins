package mcp

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/varroaci/varroa-jenkins/internal/tenantagent/accept"
)

// tenantAgentFakes records the calls that the Tenant agent tools make, and answers
// them from fields the test sets.
type tenantAgentFakes struct {
	ta         v1alpha1.TenantAgent
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
	hooks      []recordstore.TenantAgentWebhookRequest
	hookReply  recordstore.TenantAgentWebhookResponse
}

func (f *tenantAgentFakes) List() []v1alpha1.TenantAgent { return []v1alpha1.TenantAgent{f.ta} }

func (f *tenantAgentFakes) ByUID(uid string) (*v1alpha1.TenantAgent, bool) {
	ta := f.ta
	return &ta, string(ta.UID) == uid
}

func (f *tenantAgentFakes) AcceptInput() accept.Input {
	return accept.Input{TenantAgents: f.List(), ReleaseNamespace: f.ta.Namespace}
}

func (f *tenantAgentFakes) CatalogSource(string, string) (*v1alpha1.CatalogSource, bool) {
	return f.cs, f.cs != nil
}

func (f *tenantAgentFakes) Synced() bool { return true }

func (f *tenantAgentFakes) Read(_ context.Context, _ string, req forgeapi.ReadRequest) (forgeapi.ReadReply, error) {
	f.readReqs = append(f.readReqs, req)
	return f.readReply, nil
}

func (f *tenantAgentFakes) Issue(_ context.Context, _ string, req forgeapi.IssueRequest) (forgeapi.IssueReply, error) {
	f.issueReqs = append(f.issueReqs, req)
	return f.issueReply, f.issueErr
}

func (f *tenantAgentFakes) Check(_ context.Context, _ string, req forgeapi.CheckRequest) (forgeapi.CheckReply, error) {
	f.checkReqs = append(f.checkReqs, req)
	return f.checkReply, nil
}

func (f *tenantAgentFakes) PullRequest(_ context.Context, _ string, req forgeapi.PullRequestRequest) (forgeapi.PullRequestReply, error) {
	f.prReqs = append(f.prReqs, req)
	return f.prReply, f.prErr
}

func (f *tenantAgentFakes) PostRunNote(_ context.Context, req recordstore.RunNotePostRequest) (recordstore.RunNotePostResponse, error) {
	f.notes = append(f.notes, req)
	return recordstore.RunNotePostResponse{Created: true, Note: &recordstore.RunNote{}}, nil
}

func (f *tenantAgentFakes) Enqueue(_ context.Context, req recordstore.TenantAgentWebhookRequest) (recordstore.TenantAgentWebhookResponse, error) {
	f.hooks = append(f.hooks, req)
	return f.hookReply, nil
}

// newTenantAgentFakes returns a Tenant agent named triage on tenant obj with every
// output on. The forge answers success and the webhook enqueue answers a
// delivery id.
func newTenantAgentFakes() *tenantAgentFakes {
	f := &tenantAgentFakes{
		readReply:  forgeapi.ReadReply{Status: forgeapi.Status{OK: true}, Revision: "rev-1", Path: "x.md"},
		issueReply: forgeapi.IssueReply{Status: forgeapi.Status{OK: true}, Number: 7, URL: "https://git.example.org/x/y/issues/7"},
		checkReply: forgeapi.CheckReply{Status: forgeapi.Status{OK: true}, Forge: "gitea", BaseBranch: "main", CredentialID: "forge-cred"},
		prReply:    forgeapi.PullRequestReply{Status: forgeapi.Status{OK: true}, Number: 3, URL: "https://git.example.org/x/y/pulls/3"},
		hookReply:  recordstore.TenantAgentWebhookResponse{OK: true, DeliveryID: "delivery-1"},
	}
	f.ta.Name = "triage"
	f.ta.Namespace = "team-a"
	f.ta.UID = "uid-triage"
	f.ta.Spec.TenantRef.Name = "obj"
	f.ta.Spec.Outputs.Notes = true
	f.ta.Spec.Outputs.Webhook = true
	f.ta.Spec.Outputs.Issues = &v1alpha1.TenantAgentIssueOutput{Repo: "https://git.example.org/x/y", Forge: "gitea"}
	f.ta.Spec.Instructions = &v1alpha1.TenantAgentInstructions{Source: "obj-src", Path: ".varroa/tenant-agent.md"}
	f.cs = &v1alpha1.CatalogSource{}
	f.cs.Spec.RepoURL = "https://git.example.org/x/y"
	f.cs.Status.ObservedRevision = "rev-observed"
	return f
}

// tenantAgentDeps returns the guard deps with the Tenant agent seams replaced by
// fakes, and the activity sink attached.
func tenantAgentDeps(t *testing.T, f *tenantAgentFakes, sink activity.EventSink) *api.Dependencies {
	t.Helper()
	deps := guardEmitDeps(t, sink)
	deps.TenantAgents = f
	deps.ForgeRelay = f
	deps.WebhookEnqueuer = f
	deps.RunNotePoster = f
	deps.Authorizer = agentAuthorizer()
	return deps
}

// agentAuthorizer grants the admin role to the admins group and to the Tenant
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

// readerClaims is a person who can read the pipelines of tenant obj but cannot
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

// agentClaims is the Tenant agent principal of triage. The admin binding names
// the principal, so the agent passes the pipeline checks it needs.
func agentClaims() *auth.Claims {
	return &auth.Claims{Subject: "agent:team-a/obj/triage"}
}

// callTenantTool runs one tool call and returns its text and whether it failed.
func callTenantTool(t *testing.T, deps *api.Dependencies, claims *auth.Claims, name string, args map[string]any) (string, bool) {
	t.Helper()
	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]any{"name": name, "arguments": args}, claims)
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	return string(b), strings.Contains(string(b), `"isError":true`)
}

func TestReadRepoFile_OutsideSessionUsesInstructionsSource(t *testing.T) {
	f := newTenantAgentFakes()
	f.readReply.ReadFile = &forgeapi.ReadFile{Size: 5, Content: "hello"}
	deps := tenantAgentDeps(t, f, &recordingSink{})
	text, failed := callTenantTool(t, deps, guardClaims, "read_repo_file", map[string]any{"tenant": "obj", "path": ".varroa/tenant-agent.md"})
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
	f := newTenantAgentFakes()
	f.readReply.ReadFile = &forgeapi.ReadFile{Size: 5, Content: "hello"}
	deps := tenantAgentDeps(t, f, &recordingSink{})
	claims, authz := readerClaims()
	deps.Authorizer = authz
	text, failed := callTenantTool(t, deps, claims, "read_repo_file", map[string]any{
		"tenant": "obj", "path": "x.md", "repo": "https://git.example.org/other",
	})
	// A reader without a tenant grant is refused at the tenant check, before the
	// write check that the repo override needs.
	if !failed || (!strings.Contains(text, "tenant not found") && !strings.Contains(text, "tenant write check")) {
		t.Fatalf("reader read another repo: failed=%v text=%s", failed, text)
	}
	if len(f.readReqs) != 0 {
		t.Fatal("a refused read reached the forge")
	}
}

func TestReadRepoFile_PagesAt32KiB(t *testing.T) {
	f := newTenantAgentFakes()
	f.readReply.ReadFile = &forgeapi.ReadFile{Size: 40000, Content: strings.Repeat("a", 40000)}
	deps := tenantAgentDeps(t, f, &recordingSink{})
	text, failed := callTenantTool(t, deps, guardClaims, "read_repo_file", map[string]any{"tenant": "obj", "path": "big.md", "length": 99999})
	if failed {
		t.Fatalf("read failed: %s", text)
	}
	if !strings.Contains(text, `\"truncated\":true`) {
		t.Fatalf("a 40000-byte file read in one call was not marked truncated: %s", text)
	}
}

func TestReadRepoFile_DirectoryReturnsEntries(t *testing.T) {
	f := newTenantAgentFakes()
	f.readReply.ReadDir = &forgeapi.ReadDir{Entries: []forgeapi.DirEntry{{Name: "a.md"}}}
	deps := tenantAgentDeps(t, f, &recordingSink{})
	text, failed := callTenantTool(t, deps, guardClaims, "read_repo_file", map[string]any{"tenant": "obj", "path": ".varroa"})
	if failed || !strings.Contains(text, "a.md") {
		t.Fatalf("directory read: failed=%v text=%s", failed, text)
	}
}

func TestPostNote_JobNoteCarriesPrincipal(t *testing.T) {
	f := newTenantAgentFakes()
	sink := &recordingSink{}
	deps := tenantAgentDeps(t, f, sink)
	text, failed := callTenantTool(t, deps, agentClaims(), "post_note", map[string]any{"tenant": "obj", "job": "build", "body": "the build failed"})
	if failed {
		t.Fatalf("post_note failed: %s", text)
	}
	if len(f.notes) != 1 || f.notes[0].Principal != "agent:team-a/obj/triage" || f.notes[0].JobPath != "build" {
		t.Fatalf("note post = %+v", f.notes)
	}
	if len(sink.events) != 1 || sink.events[0].Source != "tenant_agent" {
		t.Fatalf("activity = %+v; want one tenant_agent event", sink.events)
	}
}

func TestPostNote_AgentNotesOffRefused(t *testing.T) {
	f := newTenantAgentFakes()
	f.ta.Spec.Outputs.Notes = false
	deps := tenantAgentDeps(t, f, &recordingSink{})
	text, failed := callTenantTool(t, deps, agentClaims(), "post_note", map[string]any{"tenant": "obj", "job": "build", "body": "x"})
	if !failed || !strings.Contains(text, "notes are off") || len(f.notes) != 0 {
		t.Fatalf("notes-off agent: failed=%v text=%s posts=%d", failed, text, len(f.notes))
	}
}

func TestPostNote_BodyLimit(t *testing.T) {
	f := newTenantAgentFakes()
	deps := tenantAgentDeps(t, f, &recordingSink{})
	_, failed := callTenantTool(t, deps, guardClaims, "post_note", map[string]any{"tenant": "obj", "job": "build", "body": strings.Repeat("x", 16385)})
	if !failed || len(f.notes) != 0 {
		t.Fatal("an oversize note reached the recorder")
	}
}

func TestOpenIssue_AgentOwnTenantPasses(t *testing.T) {
	f := newTenantAgentFakes()
	sink := &recordingSink{}
	deps := tenantAgentDeps(t, f, sink)
	text, failed := callTenantTool(t, deps, agentClaims(), "open_issue", map[string]any{"tenant": "obj", "title": "deploy failed", "body": "see run", "labels": []any{"ci"}})
	if failed || !strings.Contains(text, `\"number\":7`) {
		t.Fatalf("open_issue: failed=%v text=%s", failed, text)
	}
	if len(f.issueReqs) != 1 || f.issueReqs[0].TenantAgent.Name != "triage" || f.issueReqs[0].Labels[0] != "ci" {
		t.Fatalf("issue request = %+v", f.issueReqs)
	}
	if len(sink.events) != 1 || sink.events[0].Type != "tenant_agent.issue_opened" {
		t.Fatalf("activity = %+v", sink.events)
	}
}

// TestOpenIssue_RelayTimeoutAnswersOperatorTimeout checks that a relay timeout
// answers operator_timeout, and never points at gitEgress.
func TestOpenIssue_RelayTimeoutAnswersOperatorTimeout(t *testing.T) {
	f := newTenantAgentFakes()
	f.issueErr = fmt.Errorf("forge issue request: %w", nats.ErrTimeout)
	deps := tenantAgentDeps(t, f, &recordingSink{})
	text, failed := callTenantTool(t, deps, agentClaims(), "open_issue", map[string]any{"tenant": "obj", "title": "t"})
	if !failed || !strings.Contains(text, "operator_timeout: ") || strings.Contains(text, "gitEgress") {
		t.Fatalf("failed=%v text=%s", failed, text)
	}
}

// TestOpenIssue_NoResponderAnswersOperatorUnavailable checks that a request that no
// operator received answers operator_unavailable and says no issue was opened.
func TestOpenIssue_NoResponderAnswersOperatorUnavailable(t *testing.T) {
	f := newTenantAgentFakes()
	f.issueErr = fmt.Errorf("forge issue request: %w", nats.ErrNoResponders)
	deps := tenantAgentDeps(t, f, &recordingSink{})
	text, failed := callTenantTool(t, deps, agentClaims(), "open_issue", map[string]any{"tenant": "obj", "title": "t"})
	if !failed || !strings.Contains(text, "operator_unavailable: ") || !strings.Contains(text, "No issue was opened") || strings.Contains(text, "operator_timeout") {
		t.Fatalf("failed=%v text=%s", failed, text)
	}
}

// TestOpenIssue_OtherRelayErrorAnswersForgeUnreachable checks that a relay error
// that is not a timeout and not a missing responder answers forge_unreachable.
func TestOpenIssue_OtherRelayErrorAnswersForgeUnreachable(t *testing.T) {
	f := newTenantAgentFakes()
	f.issueErr = errors.New("boom")
	deps := tenantAgentDeps(t, f, &recordingSink{})
	text, failed := callTenantTool(t, deps, agentClaims(), "open_issue", map[string]any{"tenant": "obj", "title": "t"})
	if !failed || !strings.Contains(text, "forge_unreachable: ") {
		t.Fatalf("failed=%v text=%s", failed, text)
	}
}

// TestOpenIssue_RequestExpiredKeepsCode checks that a refused issue keeps its
// reply code in the tool error.
func TestOpenIssue_RequestExpiredKeepsCode(t *testing.T) {
	f := newTenantAgentFakes()
	f.issueReply = forgeapi.IssueReply{Status: forgeapi.Status{Code: forgeapi.CodeRequestExpired, Error: "too little time"}}
	deps := tenantAgentDeps(t, f, &recordingSink{})
	text, failed := callTenantTool(t, deps, agentClaims(), "open_issue", map[string]any{"tenant": "obj", "title": "t"})
	if !failed || !strings.Contains(text, "request_expired: ") {
		t.Fatalf("failed=%v text=%s", failed, text)
	}
}

func TestOpenIssue_IssuesOffRefused(t *testing.T) {
	f := newTenantAgentFakes()
	f.ta.Spec.Outputs.Issues = nil
	deps := tenantAgentDeps(t, f, &recordingSink{})
	text, failed := callTenantTool(t, deps, agentClaims(), "open_issue", map[string]any{"tenant": "obj", "title": "t"})
	if !failed || !strings.Contains(text, "issues are off") || len(f.issueReqs) != 0 {
		t.Fatalf("issues-off: failed=%v text=%s", failed, text)
	}
}

func TestOpenIssue_ReaderRefused(t *testing.T) {
	f := newTenantAgentFakes()
	deps := tenantAgentDeps(t, f, &recordingSink{})
	claims, authz := readerClaims()
	deps.Authorizer = authz
	text, failed := callTenantTool(t, deps, claims, "open_issue", map[string]any{"tenant": "obj", "title": "t"})
	if !failed || (!strings.Contains(text, "tenant write check") && !strings.Contains(text, "tenant not found")) || len(f.issueReqs) != 0 {
		t.Fatalf("reader: failed=%v text=%s", failed, text)
	}
}

func TestSendWebhook_OutputOffRefused(t *testing.T) {
	f := newTenantAgentFakes()
	f.ta.Spec.Outputs.Webhook = false
	deps := tenantAgentDeps(t, f, &recordingSink{})
	text, failed := callTenantTool(t, deps, agentClaims(), "send_webhook", map[string]any{"tenant": "obj", "body": map[string]any{"k": "v"}})
	if !failed || !strings.Contains(text, "webhook output is off") || len(f.hooks) != 0 {
		t.Fatalf("output off: failed=%v text=%s", failed, text)
	}
}

func TestSendWebhook_Enqueues(t *testing.T) {
	f := newTenantAgentFakes()
	sink := &recordingSink{}
	deps := tenantAgentDeps(t, f, sink)
	text, failed := callTenantTool(t, deps, agentClaims(), "send_webhook", map[string]any{"tenant": "obj", "body": map[string]any{"k": "v"}})
	if failed || !strings.Contains(text, "delivery-1") {
		t.Fatalf("send_webhook: failed=%v text=%s", failed, text)
	}
	if len(f.hooks) != 1 || f.hooks[0].Principal != "agent:team-a/obj/triage" || f.hooks[0].CallID == "" {
		t.Fatalf("enqueue = %+v", f.hooks)
	}
	if len(sink.events) != 1 || sink.events[0].Type != "tenant_agent.webhook_sent" {
		t.Fatalf("activity = %+v", sink.events)
	}
}
