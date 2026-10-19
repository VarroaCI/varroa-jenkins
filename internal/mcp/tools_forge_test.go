package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

const (
	prRepo   = "https://git.example.org/x/y"
	prBranch = "varroa/propose-pipeline"
)

// prArgs returns valid open_pull_request arguments for team proposal-team.
func prArgs() map[string]any {
	return map[string]any{
		"team":   "proposal-team",
		"repo":   prRepo,
		"branch": prBranch,
		"title":  "Add a Jenkinsfile proposed by Varroa",
		"body":   "why",
		"files":  []any{map[string]any{"path": "Jenkinsfile", "content": "pipeline {}"}},
	}
}

// proposeDeps returns the team agent deps with the forge fake and a sink.
func proposeDeps(t *testing.T) (*api.Dependencies, *teamAgentFakes, *recordingSink) {
	t.Helper()
	f := newTeamAgentFakes()
	sink := &recordingSink{}
	return teamAgentDeps(t, f, sink), f, sink
}

// readerOnlyClaims is a person with the read check on proposal-team and no
// write check.
func readerOnlyClaims() (*auth.Claims, *api.Authorizer) {
	roles := []*v1alpha1.VarroaRole{{
		ObjectMeta: metav1.ObjectMeta{Name: "pl-read"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read"}}},
		},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{{
		ObjectMeta: metav1.ObjectMeta{Name: "rita-read"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "rita"}},
			RoleRef:  "pl-read",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "proposal-team"}}},
		},
	}}
	return &auth.Claims{Subject: "rita"}, api.NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false)
}

// sessionDeps seeds one assistant session of the caller with the repo in its
// context, and returns the session id.
func sessionDeps(t *testing.T, deps *api.Dependencies) string {
	t.Helper()
	mem := store.NewMemory(store.Options{Now: time.Now})
	id, err := agent.NewID()
	if err != nil {
		t.Fatal(err)
	}
	sess := agent.Session{
		ID: id, Kind: agent.KindAssistant, Principal: guardClaims.Subject, Cluster: "core",
		State: agent.StateRunning, Trigger: agent.TriggerManual,
		Context:  map[string]any{agent.CtxRepo: prRepo, agent.CtxTeam: "proposal-team", "baseRevision": "1111111111111111111111111111111111111111"},
		Provider: "smoke", Model: "m1", KeySource: agent.KeySourceChart, DataSharing: agent.DataLogs,
		StartedAt: time.Now(),
	}
	if _, _, err := mem.CreateSession(context.Background(), sess, nil); err != nil {
		t.Fatal(err)
	}
	deps.Assistant = &api.AssistantRuntime{Store: mem}
	return id.String()
}

// callInSession runs one tool call as an assistant session call. The
// session context and the assistant origin are set the way the loop sets
// them.
func callInSession(t *testing.T, deps *api.Dependencies, sessionID, name string, args map[string]any) (string, bool) {
	t.Helper()
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := agent.WithSessionContext(context.Background(), agent.SessionContext{SessionID: sid})
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := NewInProcessHostWithOrigin(NewServer(deps), guardClaims, OriginAssistant).Call(ctx, name, raw)
	if err != nil {
		t.Fatal(err)
	}
	return res.Content, res.IsError
}

func TestOpenPullRequest_Annotation(t *testing.T) {
	if expectedToolKinds["open_pull_request"] != kindCreate {
		t.Fatalf("open_pull_request kind = %v, want kindCreate", expectedToolKinds["open_pull_request"])
	}
	tool, ok := liveByName(t)["open_pull_request"]
	if !ok {
		t.Fatal("open_pull_request is not registered")
	}
	if !strings.Contains(string(tool.InputSchema), `"required":["path","content"]`) {
		t.Fatalf("files items do not list path and content as required: %s", tool.InputSchema)
	}
}

// TestOpenPullRequest_ToolErrors checks that every refusal answers its code
// and that no relay call happens before the rules and the checks pass.
func TestOpenPullRequest_ToolErrors(t *testing.T) {
	bigFile := strings.Repeat("a", 64<<10+1)
	many := func(n int) []any {
		out := make([]any, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, map[string]any{"path": "f" + string(rune('a'+i)) + ".txt", "content": "x"})
		}
		return out
	}
	cases := []struct {
		name   string
		mutate func(args map[string]any)
		claims func() (*auth.Claims, *api.Authorizer)
		team   string
		code   string
		relay  bool
	}{
		{name: "team not found", team: "nope", code: "not_found"},
		{name: "read only", claims: readerOnlyClaims, code: "forbidden"},
		{name: "repo url", mutate: func(a map[string]any) { a["repo"] = "http://git.example.org/x/y" }, code: "repo_url_invalid"},
		{name: "branch dots", mutate: func(a map[string]any) { a["branch"] = "..x" }, code: "invalid_argument"},
		{name: "title newline", mutate: func(a map[string]any) { a["title"] = "a\nb" }, code: "invalid_argument"},
		{name: "body too large", mutate: func(a map[string]any) { a["body"] = strings.Repeat("b", 8193) }, code: "invalid_argument"},
		{name: "no files", mutate: func(a map[string]any) { a["files"] = []any{} }, code: "invalid_argument"},
		{name: "eleven files", mutate: func(a map[string]any) { a["files"] = many(11) }, code: "invalid_argument"},
		{name: "duplicate path", mutate: func(a map[string]any) {
			a["files"] = []any{map[string]any{"path": "a", "content": "x"}, map[string]any{"path": "a", "content": "y"}}
		}, code: "invalid_argument"},
		{name: "nul content", mutate: func(a map[string]any) {
			a["files"] = []any{map[string]any{"path": "a", "content": "x\x00"}}
		}, code: "invalid_argument"},
		{name: "file over 64 KiB", mutate: func(a map[string]any) {
			a["files"] = []any{map[string]any{"path": "a", "content": bigFile}}
		}, code: "invalid_argument"},
		{name: "total over 256 KiB", mutate: func(a map[string]any) {
			files := []any{}
			for i := 0; i < 5; i++ {
				files = append(files, map[string]any{"path": "f" + string(rune('a'+i)), "content": strings.Repeat("c", 60<<10)})
			}
			a["files"] = files
		}, code: "invalid_argument"},
		{name: "no credential", code: "no_matching_credential", relay: true},
		{name: "unsupported", code: "forge_unsupported", relay: true},
		{name: "auth failed", code: "forge_auth_failed", relay: true},
		{name: "write denied", code: "forge_write_denied", relay: true},
		{name: "unreachable", code: "forge_unreachable", relay: true},
		{name: "branch exists", code: "proposal_branch_exists", relay: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			deps, f, sink := proposeDeps(t)
			claims := guardClaims
			if c.claims != nil {
				var authz *api.Authorizer
				claims, authz = c.claims()
				deps.Authorizer = authz
			}
			if c.team == "" {
				c.team = "proposal-team"
			}
			args := prArgs()
			args["team"] = c.team
			if c.mutate != nil {
				c.mutate(args)
			}
			if c.relay {
				f.prReply = forgeapi.PullRequestReply{Status: forgeapi.Status{Code: c.code, Error: "refused"}}
			}
			text, failed := callTeamTool(t, deps, claims, "open_pull_request", args)
			if !failed || !strings.Contains(text, c.code+": ") {
				t.Fatalf("want error code %s, got failed=%v text=%s", c.code, failed, text)
			}
			if got := len(f.prReqs) > 0; got != c.relay {
				t.Fatalf("relay call = %v, want %v", got, c.relay)
			}
			if len(sink.events) != 0 {
				t.Fatalf("activity on a refusal: %+v", sink.events)
			}
		})
	}
}

// TestOpenPullRequest_OutsideSessionBodyAndCommit checks the appended line,
// the commit message, and the activity source of a call outside a session.
func TestOpenPullRequest_OutsideSessionBodyAndCommit(t *testing.T) {
	deps, f, sink := proposeDeps(t)
	text, failed := callTeamTool(t, deps, guardClaims, "open_pull_request", prArgs())
	if failed {
		t.Fatalf("call failed: %s", text)
	}
	if len(f.prReqs) != 1 {
		t.Fatalf("relay calls = %d", len(f.prReqs))
	}
	req := f.prReqs[0]
	wantBody := "why\n\nOpened through Varroa by " + api.ActorFrom(guardClaims) + "."
	if req.Body != wantBody {
		t.Fatalf("body = %q, want %q", req.Body, wantBody)
	}
	if req.CommitMessage != "Add a Jenkinsfile proposed by Varroa" {
		t.Fatalf("commit message = %q", req.CommitMessage)
	}
	if req.Repo != prRepo || req.Team != "proposal-team" || req.Branch != prBranch {
		t.Fatalf("relay request = %+v", req)
	}
	if len(sink.events) == 0 {
		t.Fatal("no activity event")
	}
	last := sink.events[len(sink.events)-1]
	if last.Type != "pipeline.proposal.opened" || last.Source != "mcp" || last.URL != "https://git.example.org/x/y/pulls/3" {
		t.Fatalf("activity = %+v", last)
	}
}

// TestOpenPullRequest_InSessionBodyCommitAndSource checks the session trailer
// and the assistant source of a call inside a session.
func TestOpenPullRequest_InSessionBodyCommitAndSource(t *testing.T) {
	deps, f, sink := proposeDeps(t)
	sid := sessionDeps(t, deps)
	text, failed := callInSession(t, deps, sid, "open_pull_request", prArgs())
	if failed {
		t.Fatalf("call failed: %s", text)
	}
	req := f.prReqs[0]
	if !strings.HasSuffix(req.Body, " Assistant session "+sid+".") {
		t.Fatalf("body %q does not name the session", req.Body)
	}
	if req.BaseRevision != "1111111111111111111111111111111111111111" {
		t.Fatalf("base revision = %q, want the session pin", req.BaseRevision)
	}
	if req.CommitMessage != "Add a Jenkinsfile proposed by Varroa\n\nVarroa-Session: "+sid {
		t.Fatalf("commit message = %q", req.CommitMessage)
	}
	if last := sink.events[len(sink.events)-1]; last.Source != "assistant" {
		t.Fatalf("source = %q, want assistant", last.Source)
	}
}

// TestOpenPullRequest_InSessionForeignRepo refuses a repo that is not the
// session's repo before any relay call.
func TestOpenPullRequest_InSessionForeignRepo(t *testing.T) {
	deps, f, _ := proposeDeps(t)
	sid := sessionDeps(t, deps)
	args := prArgs()
	args["repo"] = "https://git.example.org/other/r"
	text, failed := callInSession(t, deps, sid, "open_pull_request", args)
	if !failed || !strings.Contains(text, "repo_not_in_session: ") {
		t.Fatalf("failed=%v text=%s", failed, text)
	}
	if len(f.prReqs) != 0 {
		t.Fatal("a foreign repo reached the relay")
	}
}

// TestOpenPullRequest_ReplyErrorIsToolError checks that a reply error reaches
// the caller as a tool error with its code.
func TestOpenPullRequest_ReplyErrorIsToolError(t *testing.T) {
	deps, f, sink := proposeDeps(t)
	f.prReply = forgeapi.PullRequestReply{Status: forgeapi.Status{Code: "proposal_branch_exists", Error: "branch exists"}}
	text, failed := callTeamTool(t, deps, guardClaims, "open_pull_request", prArgs())
	if !failed || !strings.Contains(text, "proposal_branch_exists: branch exists") {
		t.Fatalf("failed=%v text=%s", failed, text)
	}
	if len(sink.events) != 0 {
		t.Fatal("activity on a failed pull request")
	}
}

// TestReadRepoFile_InSessionForeignRepoRefused checks that a session reads
// only its own repo.
func TestReadRepoFile_InSessionForeignRepoRefused(t *testing.T) {
	f := newTeamAgentFakes()
	deps := teamAgentDeps(t, f, &recordingSink{})
	sid := sessionDeps(t, deps)
	text, failed := callInSession(t, deps, sid, "read_repo_file", map[string]any{
		"team": "obj", "path": "x.md", "repo": "https://git.example.org/other",
	})
	if !failed || !strings.Contains(text, "repo outside the session") {
		t.Fatalf("failed=%v text=%s", failed, text)
	}
	if len(f.readReqs) != 0 {
		t.Fatal("a foreign repo reached the forge")
	}
}

// TestReadRepoFile_InSessionDefaultsToSessionRepo checks that a read without
// a repo reads the session's repo.
func TestReadRepoFile_InSessionDefaultsToSessionRepo(t *testing.T) {
	f := newTeamAgentFakes()
	f.readReply.ReadDir = &forgeapi.ReadDir{Entries: []forgeapi.DirEntry{{Name: "go.mod"}}}
	deps := teamAgentDeps(t, f, &recordingSink{})
	sid := sessionDeps(t, deps)
	text, failed := callInSession(t, deps, sid, "read_repo_file", map[string]any{"team": "obj", "path": "."})
	if failed || !strings.Contains(text, "go.mod") {
		t.Fatalf("failed=%v text=%s", failed, text)
	}
	if len(f.readReqs) != 1 || f.readReqs[0].Repo != prRepo {
		t.Fatalf("read requests = %+v", f.readReqs)
	}
}

// TestReadRepoFile_RepoWorksWithoutTeamAgentCache checks that a read that
// names a repo does not need the Team agent cache.
func TestReadRepoFile_RepoWorksWithoutTeamAgentCache(t *testing.T) {
	f := newTeamAgentFakes()
	f.readReply.ReadDir = &forgeapi.ReadDir{Entries: []forgeapi.DirEntry{{Name: "go.mod"}}}
	deps := teamAgentDeps(t, f, &recordingSink{})
	deps.TeamAgents = nil
	text, failed := callTeamTool(t, deps, guardClaims, "read_repo_file", map[string]any{
		"team": "obj", "path": ".", "repo": prRepo,
	})
	if failed || !strings.Contains(text, "go.mod") {
		t.Fatalf("failed=%v text=%s", failed, text)
	}
	if len(f.readReqs) != 1 || !reflect.DeepEqual(f.readReqs[0].Repo, prRepo) {
		t.Fatalf("read requests = %+v", f.readReqs)
	}
}
