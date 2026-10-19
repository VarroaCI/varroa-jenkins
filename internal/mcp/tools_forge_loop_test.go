package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/loop"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// loopProvider answers one scripted turn per call.
type loopProvider struct {
	turns [][]provider.Delta
	n     int
}

func (p *loopProvider) Stream(context.Context, provider.Key, provider.Request) (<-chan provider.Delta, error) {
	ch := make(chan provider.Delta, 16)
	turn := []provider.Delta{{Text: "ok"}, {Stop: provider.StopEndTurn}}
	if p.n < len(p.turns) {
		turn = p.turns[p.n]
	}
	p.n++
	go func() {
		defer close(ch)
		for _, d := range turn {
			ch <- d
		}
	}()
	return ch, nil
}

// loopCall is one turn that calls a tool with the given JSON arguments.
func loopCall(name, args string) []provider.Delta {
	return []provider.Delta{
		{ToolCall: &provider.ToolCallDelta{Index: 0, ID: "call-" + name, Name: name, ArgumentsFragment: args}},
		{Usage: &provider.Usage{PromptTokens: 10, CompletionTokens: 5}},
		{Stop: provider.StopToolUse},
	}
}

// loopText is one turn that answers in text and ends the loop.
func loopText(text string) []provider.Delta {
	return []provider.Delta{
		{Text: text},
		{Usage: &provider.Usage{PromptTokens: 10, CompletionTokens: 5}},
		{Stop: provider.StopEndTurn},
	}
}

// loopRuntime returns an assistant runtime over one scripted provider. Its
// host calls the MCP tools as the assistant origin, as the loop does.
func loopRuntime(t *testing.T, deps *api.Dependencies, turns ...[]provider.Delta) *api.AssistantRuntime {
	t.Helper()
	mem := store.NewMemory(store.Options{Now: time.Now})
	cfg := provider.Config{
		Providers: []provider.Endpoint{{
			Name: "smoke", Kind: agent.ProviderOpenAICompatible, BaseURL: "http://127.0.0.1:9/v1",
			Models: []string{"m1"}, Trust: agent.TrustInternal, Default: true,
		}},
		ExternalDataSharing: agent.DataLogs,
		Budgets:             provider.Budgets{Session: 100000, UserDay: 1000000, TeamDay: 1000000, Steps: 32},
		ApprovalExpiryText:  "1h",
		ApprovalExpiry:      time.Hour,
	}
	factories := map[agent.ProviderKind]provider.Factory{
		agent.ProviderOpenAICompatible: func(provider.Endpoint) (provider.Provider, error) {
			return &loopProvider{turns: turns}, nil
		},
	}
	reg, err := provider.NewRegistry(cfg, factories, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt := &api.AssistantRuntime{
		Config:   cfg,
		Registry: reg,
		Store:    store.WithNotify(mem, nil, logger),
		HostFor: func(c *auth.Claims) loop.ToolHost {
			return NewInProcessHostWithOrigin(NewServer(deps), c, OriginAssistant)
		},
		Owner:   "owner-test",
		Cluster: "core",
		Logger:  logger,
	}
	return rt
}

// loopDo sends one request through the router as the given claims.
func loopDo(t *testing.T, h http.Handler, claims *auth.Claims, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.ContextWithClaims(req.Context(), claims))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// loopSession is the part of the session read that the loop tests check.
type loopSession struct {
	Session struct {
		State string `json:"state"`
	} `json:"session"`
	Approvals []struct {
		ID     string  `json:"id"`
		Tool   string  `json:"tool"`
		State  string  `json:"state"`
		Result *string `json:"result"`
	} `json:"approvals"`
}

// proposeArgs returns the open_pull_request arguments that the model sends.
func proposeArgs(repo string) string {
	return `{"team":"proposal-team","repo":"` + repo + `","branch":"varroa/propose-pipeline",` +
		`"title":"Add a Jenkinsfile proposed by Varroa","body":"why",` +
		`"files":[{"path":"Jenkinsfile","content":"pipeline {}"}]}`
}

// startProposal posts one proposal and returns the session id.
func startProposal(t *testing.T, deps *api.Dependencies) string {
	t.Helper()
	rec := loopDo(t, api.NewRouter(deps), guardClaims, http.MethodPost,
		"/pipelines/proposal-team/propose", map[string]any{"repo": prRepo})
	if rec.Code != http.StatusCreated {
		t.Fatalf("propose status = %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.SessionID
}

// readSession returns the session read of the given id.
func readSession(t *testing.T, deps *api.Dependencies, id string) loopSession {
	t.Helper()
	rec := loopDo(t, api.NewRouter(deps), guardClaims, http.MethodGet, "/assistant/sessions/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get session status = %d, body %s", rec.Code, rec.Body)
	}
	var out loopSession
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestProposeLoop_ApproveOpensAndResumes runs the proposal loop: the model
// reads the repository, asks for the pull request, and parks on one approval.
// The approval opens the pull request and the loop resumes to completion.
func TestProposeLoop_ApproveOpensAndResumes(t *testing.T) {
	deps, f, _ := proposeDeps(t)
	f.readReply.ReadDir = &forgeapi.ReadDir{Entries: []forgeapi.DirEntry{{Name: "go.mod", Type: "file"}}}
	rt := loopRuntime(t, deps,
		loopCall("read_repo_file", `{"team":"proposal-team","path":".","repo":"`+prRepo+`"}`),
		loopCall("open_pull_request", proposeArgs(prRepo)),
		loopText("the pull request is open"),
	)
	deps.Assistant = rt
	id := startProposal(t, deps)
	rt.Wait()

	sess := readSession(t, deps, id)
	if sess.Session.State != "waiting_approval" || len(sess.Approvals) != 1 {
		t.Fatalf("session = %+v, want one approval parked", sess)
	}
	appr := sess.Approvals[0]
	if appr.Tool != "open_pull_request" || appr.State != "pending" {
		t.Fatalf("approval = %+v", appr)
	}
	rec := loopDo(t, api.NewRouter(deps), guardClaims, http.MethodPost, "/assistant/approvals/"+appr.ID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve status = %d, body %s", rec.Code, rec.Body)
	}
	rt.Wait()

	after := readSession(t, deps, id)
	if after.Session.State != "completed" || len(after.Approvals) != 1 || after.Approvals[0].Result == nil {
		t.Fatalf("after approve: %+v", after)
	}
	if !strings.Contains(*after.Approvals[0].Result, "https://git.example.org/x/y/pulls/3") {
		t.Fatalf("result = %q, want the pull request URL", *after.Approvals[0].Result)
	}
	if len(f.prReqs) != 1 || !strings.HasSuffix(f.prReqs[0].Body, " Assistant session "+id+".") {
		t.Fatalf("pull request requests = %+v", f.prReqs)
	}
}

// TestProposeLoop_DenyResumes checks that a denial resumes the loop with the
// denial as the tool result, and that no pull request opens.
func TestProposeLoop_DenyResumes(t *testing.T) {
	deps, f, _ := proposeDeps(t)
	rt := loopRuntime(t, deps,
		loopCall("open_pull_request", proposeArgs(prRepo)),
		loopText("the proposal was denied"),
	)
	deps.Assistant = rt
	id := startProposal(t, deps)
	rt.Wait()

	sess := readSession(t, deps, id)
	if len(sess.Approvals) != 1 {
		t.Fatalf("approvals = %+v", sess.Approvals)
	}
	rec := loopDo(t, api.NewRouter(deps), guardClaims, http.MethodPost,
		"/assistant/approvals/"+sess.Approvals[0].ID+"/deny", map[string]any{"reason": "not now"})
	if rec.Code != http.StatusOK {
		t.Fatalf("deny status = %d, body %s", rec.Code, rec.Body)
	}
	rt.Wait()

	after := readSession(t, deps, id)
	if after.Session.State != "completed" {
		t.Fatalf("state = %s, want completed", after.Session.State)
	}
	if after.Approvals[0].Result == nil || !strings.HasPrefix(*after.Approvals[0].Result, "denied by") {
		t.Fatalf("result = %v, want a denial", after.Approvals[0].Result)
	}
	if len(f.prReqs) != 0 {
		t.Fatal("a denied proposal reached the relay")
	}
}

// TestProposeLoop_ApprovedForeignRepoRefused checks that the approved run
// keeps the session context: a repo other than the session's is refused.
func TestProposeLoop_ApprovedForeignRepoRefused(t *testing.T) {
	deps, f, _ := proposeDeps(t)
	rt := loopRuntime(t, deps,
		loopCall("open_pull_request", proposeArgs("https://git.example.org/other/r")),
		loopText("done"),
	)
	deps.Assistant = rt
	id := startProposal(t, deps)
	rt.Wait()

	sess := readSession(t, deps, id)
	rec := loopDo(t, api.NewRouter(deps), guardClaims, http.MethodPost,
		"/assistant/approvals/"+sess.Approvals[0].ID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve status = %d, body %s", rec.Code, rec.Body)
	}
	rt.Wait()

	after := readSession(t, deps, id)
	if after.Approvals[0].Result == nil || !strings.Contains(*after.Approvals[0].Result, "repo_not_in_session") {
		t.Fatalf("result = %v, want repo_not_in_session", after.Approvals[0].Result)
	}
	if len(f.prReqs) != 0 {
		t.Fatal("a foreign repo reached the relay on the approved run")
	}
}
