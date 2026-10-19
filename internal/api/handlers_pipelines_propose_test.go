package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/prompts"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// proposeRelay is a ForgeRelay that answers the check from a field and
// records each check request.
type proposeRelay struct {
	checkReply forgeapi.CheckReply
	checkErr   error
	checkReqs  []forgeapi.CheckRequest
}

func (p *proposeRelay) Check(_ context.Context, _ string, req forgeapi.CheckRequest) (forgeapi.CheckReply, error) {
	p.checkReqs = append(p.checkReqs, req)
	return p.checkReply, p.checkErr
}

func (p *proposeRelay) Read(context.Context, string, forgeapi.ReadRequest) (forgeapi.ReadReply, error) {
	return forgeapi.ReadReply{}, errors.New("not used")
}

func (p *proposeRelay) PullRequest(context.Context, string, forgeapi.PullRequestRequest) (forgeapi.PullRequestReply, error) {
	return forgeapi.PullRequestReply{}, errors.New("not used")
}

func (p *proposeRelay) Issue(context.Context, string, forgeapi.IssueRequest) (forgeapi.IssueReply, error) {
	return forgeapi.IssueReply{}, errors.New("not used")
}

// proposeTenants answers the Team payments with one pipelines tenant on the
// core cluster. Any other team has no tenant.
type proposeTenants struct{}

func (proposeTenants) Resolve(_ context.Context, _ *auth.Claims, team string) ([]TenantRef, error) {
	if team != "payments" {
		return nil, nil
	}
	return []TenantRef{{Tenant: "payments", Team: "payments", Folder: "payments", Cluster: "core"}}, nil
}

func (proposeTenants) ResolveRun(context.Context, *auth.Claims, recordstore.Run) (TenantRef, error) {
	return TenantRef{}, errors.New("not used")
}

// proposeFixture is the assistant fixture with the proposal seams set: the
// tenant resolver, the forge relay, and three users. The writer holds the
// tenant write check, the reader holds only the read check, and the nobody
// holds no assistant use.
type proposeFixture struct {
	*assistantFixture
	relay *proposeRelay
}

const (
	proposeWriter = "writer-user"
	proposeReader = "reader-user"
	proposeNobody = "nobody-user"
	proposeRepo   = "https://git.example.org/acme/payments"
)

func newProposeFixture(t *testing.T, turns ...[]provider.Delta) *proposeFixture {
	t.Helper()
	f := newAssistantFixture(t, turns...)
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "assist"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"assistant"}, Verbs: []string{"use"}}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pl-rw"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read", "write"}}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pl-r"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read"}}}}},
	}
	tenant := &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "payments"}}}
	bind := func(name, user, role string, scope *v1alpha1.VarroaRoleBindingScope) *v1alpha1.VarroaRoleBinding {
		return &v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: user}}, RoleRef: role, Scope: scope,
			},
		}
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		bind("w-assist", proposeWriter, "assist", nil),
		bind("w-pipelines", proposeWriter, "pl-rw", tenant),
		bind("r-assist", proposeReader, "assist", nil),
		bind("r-pipelines", proposeReader, "pl-r", tenant),
		bind("n-assist", "viewer-user", "assist", nil),
	}
	f.deps.Authorizer = testResolverAndAuthorizer(roles, bindings)
	relay := &proposeRelay{checkReply: forgeapi.CheckReply{
		Status: forgeapi.Status{OK: true}, Forge: "gitea", BaseBranch: "main", CredentialID: "forge-cred",
		Revision: "5555555555555555555555555555555555555555",
	}}
	f.deps.TenantResolver = proposeTenants{}
	f.deps.ForgeRelay = relay
	f.deps.CoreCluster = "core"
	f.router = NewRouter(f.deps)
	return &proposeFixture{assistantFixture: f, relay: relay}
}

// writerClaims is the claims of the writer user.
func (f *proposeFixture) writerClaims() *auth.Claims {
	return &auth.Claims{Subject: proposeWriter, PreferredUsername: "writer", IssuedAt: time.Now()}
}

// sessionCount counts the sessions of the writer.
func (f *proposeFixture) sessionCount(t *testing.T) int {
	t.Helper()
	page, err := f.mem.ListSessions(context.Background(), store.SessionQuery{Principal: proposeWriter})
	if err != nil {
		t.Fatal(err)
	}
	return len(page.Items)
}

// seedRunning adds n running sessions for the writer, so the busy cap holds.
func (f *proposeFixture) seedRunning(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id, err := agent.NewID()
		if err != nil {
			t.Fatal(err)
		}
		sess := agent.Session{
			ID: id, Kind: agent.KindAssistant, Principal: proposeWriter, Cluster: "core", State: agent.StateRunning,
			Trigger: agent.TriggerManual, Context: map[string]any{}, Provider: "smoke", Model: "m1",
			KeySource: agent.KeySourceChart, DataSharing: agent.DataLogs, StartedAt: time.Now(),
		}
		if _, _, err := f.mem.CreateSession(context.Background(), sess, nil); err != nil {
			t.Fatal(err)
		}
	}
}

// TestProposePipeline_RefusalsInOrder runs one request per row of proposal
// section 5.5. Each refusal answers its code and writes no session row.
func TestProposePipeline_RefusalsInOrder(t *testing.T) {
	type row struct {
		name   string
		setup  func(t *testing.T, f *proposeFixture)
		user   *auth.Claims
		team   string
		repo   string
		status int
		code   string
		checks int
	}
	nobody := &auth.Claims{Subject: proposeNobody, PreferredUsername: "nobody", IssuedAt: time.Now()}
	reader := &auth.Claims{Subject: proposeReader, PreferredUsername: "reader", IssuedAt: time.Now()}
	forgeRow := func(code string, status int, checks int) row {
		return row{name: code, user: nil, team: "payments", repo: proposeRepo, status: status, code: code, checks: checks,
			setup: func(t *testing.T, f *proposeFixture) {
				f.relay.checkReply = forgeapi.CheckReply{Status: forgeapi.Status{Code: code, Error: "refused"}}
			}}
	}
	rows := []row{
		{name: "switch off", team: "payments", repo: proposeRepo, status: http.StatusConflict, code: assistantErrOff,
			setup: func(t *testing.T, f *proposeFixture) {
				off := false
				if _, err := f.mem.PutSettings(context.Background(), agent.UserScope(proposeWriter), store.SettingsPatch{Enabled: &off}, proposeWriter); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "no use verb", user: nobody, team: "payments", repo: proposeRepo, status: http.StatusForbidden},
		{name: "unreadable team", team: "nope", repo: proposeRepo, status: http.StatusNotFound},
		{name: "reader", user: reader, team: "payments", repo: proposeRepo, status: http.StatusForbidden},
		{name: "http scheme", team: "payments", repo: "http://git.example.org/acme/payments", status: http.StatusBadRequest, code: forgeapi.CodeRepoURLInvalid},
		{name: "userinfo", team: "payments", repo: "https://u:p@git.example.org/acme/payments", status: http.StatusBadRequest, code: forgeapi.CodeRepoURLInvalid},
		{name: "port 8443", team: "payments", repo: "https://git.example.org:8443/acme/payments", status: http.StatusBadRequest, code: forgeapi.CodeRepoURLInvalid},
		{name: "data level none", team: "payments", repo: proposeRepo, status: http.StatusConflict, code: proposeErrDataSharingTooLow,
			setup: func(t *testing.T, f *proposeFixture) {
				none := agent.DataNone
				if _, err := f.mem.PutSettings(context.Background(), agent.UserScope(proposeWriter), store.SettingsPatch{DataSharing: &none}, proposeWriter); err != nil {
					t.Fatal(err)
				}
			}},
		forgeRow(forgeapi.CodeNoMatchingCredential, http.StatusUnprocessableEntity, 1),
		forgeRow(forgeapi.CodeForgeUnsupported, http.StatusUnprocessableEntity, 1),
		forgeRow(forgeapi.CodeForgeAuthFailed, http.StatusUnprocessableEntity, 1),
		forgeRow(forgeapi.CodeForgeWriteDenied, http.StatusUnprocessableEntity, 1),
		forgeRow(forgeapi.CodeJenkinsfileExists, http.StatusConflict, 1),
		forgeRow(forgeapi.CodeProposalBranchExists, http.StatusConflict, 1),
		{name: "relay error", team: "payments", repo: proposeRepo, status: http.StatusBadGateway, code: forgeapi.CodeForgeUnreachable, checks: 1,
			setup: func(t *testing.T, f *proposeFixture) { f.relay.checkErr = errors.New("no reply") }},
		{name: "reply unreachable", team: "payments", repo: proposeRepo, status: http.StatusBadGateway, code: forgeapi.CodeForgeUnreachable, checks: 1,
			setup: func(t *testing.T, f *proposeFixture) {
				f.relay.checkReply = forgeapi.CheckReply{Status: forgeapi.Status{Code: forgeapi.CodeForgeUnreachable, Error: "down"}}
			}},
		{name: "busy", team: "payments", repo: proposeRepo, status: http.StatusTooManyRequests, code: assistantErrBusy, checks: 1,
			setup: func(t *testing.T, f *proposeFixture) { f.seedRunning(t, agent.MaxRunningSessions) }},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			f := newProposeFixture(t)
			if r.setup != nil {
				r.setup(t, f)
			}
			user := r.user
			if user == nil {
				user = f.writerClaims()
			}
			before := f.sessionCount(t)
			rec := f.do(t, user, http.MethodPost, "/pipelines/"+r.team+"/propose", map[string]any{"repo": r.repo})
			if rec.Code != r.status {
				t.Fatalf("status = %d, want %d, body %s", rec.Code, r.status, rec.Body)
			}
			if r.code != "" {
				var body map[string]any
				decodeBody(t, rec, &body)
				if body["code"] != r.code {
					t.Fatalf("code = %v, want %s", body["code"], r.code)
				}
			}
			if got := len(f.relay.checkReqs); got != r.checks {
				t.Fatalf("check requests = %d, want %d", got, r.checks)
			}
			if after := f.sessionCount(t); after != before {
				t.Fatalf("session rows %d -> %d on a refusal", before, after)
			}
		})
	}
}

// TestProposePipeline_DisabledAnswers503 checks that the route answers the
// disabled shape when the assistant has no runtime.
func TestProposePipeline_DisabledAnswers503(t *testing.T) {
	f := newProposeFixture(t)
	f.deps.Assistant = nil
	f.router = NewRouter(f.deps)
	rec := f.do(t, f.writerClaims(), http.MethodPost, "/pipelines/payments/propose", map[string]any{"repo": proposeRepo})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
}

// TestProposePipeline_StartsSessionWithServerContext checks the 201 answer,
// the server-set context and prompt, the first user text, and the check
// request, which carries the normalized repo.
func TestProposePipeline_StartsSessionWithServerContext(t *testing.T) {
	f := newProposeFixture(t, assistantTextTurn("done"))
	rec := f.do(t, f.writerClaims(), http.MethodPost, "/pipelines/payments/propose",
		map[string]any{"repo": "https://git.example.org/acme/payments.git/"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var body proposePipelineResponse
	decodeBody(t, rec, &body)
	if body.Repo != proposeRepo || body.Forge != "gitea" || body.BaseBranch != "main" || body.CredentialID != "forge-cred" {
		t.Fatalf("body = %+v", body)
	}
	if len(f.relay.checkReqs) != 1 || f.relay.checkReqs[0] != (forgeapi.CheckRequest{Team: "payments", Repo: proposeRepo}) {
		t.Fatalf("check requests = %+v", f.relay.checkReqs)
	}
	f.rt.Wait()

	id, err := uuid.Parse(body.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := f.mem.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Context[agent.CtxTeam] != "payments" || sess.Context[agent.CtxRoute] != "/pipelines/payments" ||
		sess.Context[agent.CtxRepo] != proposeRepo || sess.Context[agent.CtxPrompt] != prompts.ProposePipeline ||
		sess.Context["baseRevision"] != "5555555555555555555555555555555555555555" {
		t.Fatalf("context = %v", sess.Context)
	}
	events, err := f.mem.Events(context.Background(), id, 0, 10)
	if err != nil || len(events) == 0 {
		t.Fatalf("events = %v, %v", events, err)
	}
	var first agent.UserPayload
	if err := json.Unmarshal(events[0].Payload, &first); err != nil {
		t.Fatal(err)
	}
	want, _ := prompts.Get(prompts.ProposePipeline)
	if events[0].Type != agent.EventUser || first.Text != want.Text {
		t.Fatalf("first event %s %q, want the propose prompt", events[0].Type, first.Text)
	}
}

// TestProposePipeline_UserProviderSetsLevelAndSession checks that the
// provider the user chose sets the data level. An external provider under a
// none ceiling refuses; under a metadata ceiling it starts the session.
func TestProposePipeline_UserProviderSetsLevelAndSession(t *testing.T) {
	cfg := provider.Config{
		Providers: []provider.Endpoint{
			{Name: "smoke", Kind: agent.ProviderOpenAICompatible, BaseURL: "http://127.0.0.1:9/v1",
				Models: []string{"m1"}, Trust: agent.TrustInternal, Default: true},
			{Name: "ext", Kind: agent.ProviderOpenAICompatible, BaseURL: "http://127.0.0.1:9/v1",
				Models: []string{"m2"}, Trust: agent.TrustExternal},
		},
		ExternalDataSharing: agent.DataNone,
		Budgets:             provider.Budgets{Session: 100000, UserDay: 1000000, TeamDay: 1000000, Steps: 32},
		ApprovalExpiryText:  "1h",
		ApprovalExpiry:      time.Hour,
	}
	for _, tc := range []struct {
		ceiling agent.DataSharing
		status  int
		level   agent.DataSharing
	}{
		{agent.DataNone, http.StatusConflict, ""},
		{agent.DataMetadata, http.StatusCreated, agent.DataMetadata},
	} {
		f := newProposeFixture(t, assistantTextTurn("done"))
		factories := map[agent.ProviderKind]provider.Factory{
			agent.ProviderOpenAICompatible: func(provider.Endpoint) (provider.Provider, error) {
				return &scriptedAssistantProvider{turns: [][]provider.Delta{assistantTextTurn("done")}}, nil
			},
		}
		cfg.ExternalDataSharing = tc.ceiling
		reg, err := provider.NewRegistry(cfg, factories, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		f.rt.Registry = reg
		f.rt.Config = cfg
		ext := "ext"
		if _, err := f.mem.PutSettings(context.Background(), agent.UserScope(proposeWriter), store.SettingsPatch{Provider: &ext}, proposeWriter); err != nil {
			t.Fatal(err)
		}
		rec := f.do(t, f.writerClaims(), http.MethodPost, "/pipelines/payments/propose", map[string]any{"repo": proposeRepo})
		if rec.Code != tc.status {
			t.Fatalf("ceiling %s: status = %d, body %s", tc.ceiling, rec.Code, rec.Body)
		}
		if tc.status != http.StatusCreated {
			if len(f.relay.checkReqs) != 0 {
				t.Fatalf("ceiling %s: check ran for a refused level", tc.ceiling)
			}
			continue
		}
		var body proposePipelineResponse
		decodeBody(t, rec, &body)
		f.rt.Wait()
		id, _ := uuid.Parse(body.SessionID)
		sess, err := f.mem.GetSession(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if sess.Provider != "ext" || sess.DataSharing != tc.level {
			t.Fatalf("session provider %q level %q, want ext and %s", sess.Provider, sess.DataSharing, tc.level)
		}
	}
}

// TestProposePipeline_SkippedSessionAnswers201 checks that a paused assistant
// still answers 201, with the session in the skipped state.
func TestProposePipeline_SkippedSessionAnswers201(t *testing.T) {
	f := newProposeFixture(t)
	paused := true
	if _, err := f.mem.PutSettings(context.Background(), agent.ScopeGlobal, store.SettingsPatch{Paused: &paused}, "admin-user"); err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, f.writerClaims(), http.MethodPost, "/pipelines/payments/propose", map[string]any{"repo": proposeRepo})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var body proposePipelineResponse
	decodeBody(t, rec, &body)
	if body.State != string(agent.StateSkipped) {
		t.Fatalf("state = %s, want skipped", body.State)
	}
}

// TestForgeCodeStatus_RequestExpired checks that an expired pull request
// maps to a gateway timeout.
func TestForgeCodeStatus_RequestExpired(t *testing.T) {
	if got := forgeCodeStatus(forgeapi.CodeRequestExpired); got != http.StatusGatewayTimeout {
		t.Fatalf("status %d, want %d", got, http.StatusGatewayTimeout)
	}
}
