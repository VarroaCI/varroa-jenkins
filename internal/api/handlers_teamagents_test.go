package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/loop"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/accept"
)

// teamAgentTenants answers the tenants of each team. A team it does not
// hold answers 404, as the real resolver does.
type teamAgentTenants struct {
	refs map[string][]TenantRef
}

func (r teamAgentTenants) Resolve(_ context.Context, _ *auth.Claims, team string) ([]TenantRef, error) {
	refs, ok := r.refs[team]
	if !ok {
		return nil, &ServiceError{Status: http.StatusNotFound, Message: "not found"}
	}
	return refs, nil
}

// ResolveRun is not used by the Team agent routes.
func (r teamAgentTenants) ResolveRun(context.Context, *auth.Claims, recordstore.Run) (TenantRef, error) {
	return TenantRef{}, &ServiceError{Status: http.StatusNotFound, Message: "not found"}
}

// fakeTeamAgentSource is the TeamAgentSource of the tests.
type fakeTeamAgentSource struct {
	items []v1alpha1.TeamAgent
}

func (f *fakeTeamAgentSource) List() []v1alpha1.TeamAgent {
	return append([]v1alpha1.TeamAgent(nil), f.items...)
}

func (f *fakeTeamAgentSource) ByUID(uid string) (*v1alpha1.TeamAgent, bool) {
	for i := range f.items {
		if string(f.items[i].UID) == uid {
			return &f.items[i], true
		}
	}
	return nil, false
}

func (f *fakeTeamAgentSource) AcceptInput() accept.Input {
	return accept.Input{TeamAgents: f.items, ReleaseNamespace: "test-ns"}
}

func (f *fakeTeamAgentSource) CatalogSource(string, string) (*v1alpha1.CatalogSource, bool) {
	return nil, false
}

func (f *fakeTeamAgentSource) Synced() bool { return true }

// recordingEvents keeps every activity event the BFF publishes.
type recordingEvents struct {
	mu     sync.Mutex
	events []activity.Event
}

func (r *recordingEvents) Publish(e activity.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// teamAgentHost is the tool host of the tests. It lists a fixed registry and
// records each call with the origin it ran under.
type teamAgentHost struct {
	origin string
	calls  *callLog
}

type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (c *callLog) add(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, s)
}

func (c *callLog) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// teamAgentTools is the fixed registry of the capability tests.
var teamAgentTools = []agent.ToolDef{
	{Name: "get_run", Description: "Reads one run.", Class: agent.ClassRead},
	{Name: "restart_run", Description: "Restarts a run.", Class: agent.ClassWrite},
	{Name: "cancel_run", Description: "Cancels a run.", Class: agent.ClassWrite},
	{Name: "post_note", Description: "Writes a note.", Class: agent.ClassWrite},
	{Name: "release_fleet_lock", Description: "Releases a lock.", Class: agent.ClassWrite},
	{Name: "run_pipeline", Description: "Starts a run.", Class: agent.ClassWrite},
	{Name: "call_jenkins_tool", Description: "Hidden.", Class: agent.ClassWrite, Hidden: true},
}

func (h teamAgentHost) List(context.Context) ([]agent.ToolDef, error) {
	return teamAgentTools, nil
}

func (h teamAgentHost) Call(_ context.Context, name string, _ json.RawMessage) (agent.ToolResult, error) {
	h.calls.add(h.origin + ":" + name)
	return agent.ToolResult{Content: "ok:" + name}, nil
}

// teamAgentFixture is the BFF with two Team agents on two tenants, and the
// caller claims of each role.
type teamAgentFixture struct {
	rt     *AssistantRuntime
	mem    *store.Memory
	events *recordingEvents
	calls  *callLog
	source *fakeTeamAgentSource
	router http.Handler
	deps   *Dependencies

	reader   *auth.Claims
	writer   *auth.Claims
	ops      *auth.Claims
	admin    *auth.Claims
	stranger *auth.Claims
	agent    *auth.Claims
}

// teamAgentPrincipal is the subject of the team-a Team agent.
const teamAgentPrincipal = "agent:test-ns/team-a/ta-one"

// teamAgentObject returns one accepted TeamAgent of team in test-ns.
func teamAgentObject(name, team string) v1alpha1.TeamAgent {
	return v1alpha1.TeamAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "test-ns", UID: types.UID("uid-" + name),
			CreationTimestamp: metav1.NewTime(time.Unix(1700000000, 0)),
		},
		Spec: v1alpha1.TeamAgentSpec{
			TeamRef:          v1alpha1.TeamAgentTeamRef{Name: team},
			Provider:         "smoke",
			Model:            "m1",
			DataSharing:      "logs",
			Triggers:         []v1alpha1.TeamAgentTrigger{{Type: v1alpha1.TeamAgentTriggerManual}, {Type: v1alpha1.TeamAgentTriggerRunFailed}},
			UnattendedWrites: []string{"restart_run"},
			Outputs:          v1alpha1.TeamAgentOutputs{Notes: true},
		},
	}
}

// newTeamAgentFixture wires the BFF for the Team agent tests.
func newTeamAgentFixture(t *testing.T) *teamAgentFixture {
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
			return &scriptedAssistantProvider{}, nil
		},
	}
	reg, err := provider.NewRegistry(cfg, factories, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// team-bare has a tenant and no TeamAgent, so it answers no accepted agent.
	f := &teamAgentFixture{
		mem:    mem,
		events: &recordingEvents{},
		calls:  &callLog{},
		source: &fakeTeamAgentSource{items: []v1alpha1.TeamAgent{
			teamAgentObject("ta-one", "team-a"),
			teamAgentObject("ta-member", "team-m"),
		}},
	}
	f.rt = &AssistantRuntime{
		Config:   cfg,
		Registry: reg,
		Store:    store.WithNotify(mem, nil, logger),
		HostFor:  func(*auth.Claims) loop.ToolHost { return teamAgentHost{calls: f.calls} },
		HostForOrigin: func(_ *auth.Claims, origin string) loop.ToolHost {
			return teamAgentHost{origin: origin, calls: f.calls}
		},
		Owner:   "owner-test",
		Cluster: "core",
		Logger:  logger,
	}
	f.reader = &auth.Claims{Subject: "reader-user", PreferredUsername: "reader", IssuedAt: time.Now()}
	f.writer = &auth.Claims{Subject: "writer-user", PreferredUsername: "writer", IssuedAt: time.Now()}
	f.ops = &auth.Claims{Subject: "ops-user", PreferredUsername: "ops", IssuedAt: time.Now()}
	f.admin = &auth.Claims{Subject: "aadmin-user", PreferredUsername: "aadmin", IssuedAt: time.Now()}
	f.stranger = &auth.Claims{Subject: "stranger-user", PreferredUsername: "stranger", IssuedAt: time.Now()}
	f.agent = &auth.Claims{Subject: teamAgentPrincipal, IssuedAt: time.Now()}

	scope := &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-a"}, {Name: "team-m"}, {Name: "team-bare"}}}
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "team-reader"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read"}}, {Resources: []string{"assistant"}, Verbs: []string{"use"}}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "team-writer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read", "write"}}, {Resources: []string{"assistant"}, Verbs: []string{"use"}}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "assistant-admin"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"assistant"}, Verbs: []string{"admin"}}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "ops"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"teamagents"}, Verbs: []string{"unattended"}}, {Resources: []string{"agentsessions"}, Verbs: []string{"read"}}}}},
	}
	bind := func(name, subject, role string, sc *v1alpha1.VarroaRoleBindingScope) *v1alpha1.VarroaRoleBinding {
		return &v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: subject}}, RoleRef: role, Scope: sc,
			},
		}
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		bind("reader-b", "reader-user", "team-reader", scope),
		bind("writer-b", "writer-user", "team-writer", scope),
		bind("ops-b", "ops-user", "ops", nil),
		bind("aadmin-b", "aadmin-user", "assistant-admin", nil),
		bind("agent-b", teamAgentPrincipal, "team-writer", scope),
	}
	deps := &Dependencies{
		Authorizer: testResolverAndAuthorizer(roles, bindings),
		Logger:     logger,
		Assistant:  f.rt,
		TeamAgents: f.source,
		TenantResolver: teamAgentTenants{refs: map[string][]TenantRef{
			"team-a":    {{Tenant: "team-a", Folder: "team-a", Team: "team-a", Cluster: "core"}},
			"team-m":    {{Tenant: "team-m", Folder: "team-m", Team: "team-m", Cluster: "member-east"}},
			"team-bare": {{Tenant: "team-bare", Folder: "team-bare", Team: "team-bare", Cluster: "core"}},
		}},
		ActivityPublisher: f.events,
		CoreCluster:       "core",
	}
	f.deps = deps
	f.router = NewRouter(deps)
	return f
}

// do sends one request as claims through the router.
func (f *teamAgentFixture) do(t *testing.T, claims *auth.Claims, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, "/api/v1"+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.ContextWithClaims(req.Context(), claims))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// newTeamSession stores one team_agent session of team in state.
func (f *teamAgentFixture) newTeamSession(t *testing.T, state agent.SessionState) agent.Session {
	t.Helper()
	id, err := agent.NewID()
	if err != nil {
		t.Fatal(err)
	}
	sess := agent.Session{
		ID: id, Kind: agent.KindTeamAgent, Principal: teamAgentPrincipal, Team: "team-a",
		Cluster: "core", Trigger: "run.failed", Provider: "smoke", Model: "m1",
		KeySource: agent.KeySourceChart, DataSharing: agent.DataLogs, State: state,
	}
	created, _, err := f.mem.CreateSession(context.Background(), sess, nil)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// grantUnattended stores the grant of ta-one, as the PUT route does.
func (f *teamAgentFixture) grantUnattended(t *testing.T, tools []string) {
	t.Helper()
	ta := f.source.items[0]
	grant := &agent.UnattendedGrant{UID: string(ta.UID), Tools: tools, GrantedBy: "ops", GrantedAt: time.Now()}
	if err := f.mem.PutTeamAgentUnattended(context.Background(), "test-ns/ta-one", grant, "ops"); err != nil {
		t.Fatal(err)
	}
}

// decodeMap decodes a JSON object body.
func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	decodeBody(t, rec, &out)
	return out
}

func TestTeamAgentRoutes_GetAgentReader(t *testing.T) {
	f := newTeamAgentFixture(t)
	rec := f.do(t, f.reader, http.MethodGet, "/pipelines/team-a/agent", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	body := decodeMap(t, rec)
	if body["name"] != "ta-one" || body["accepted"] != true || body["cluster"] != "core" {
		t.Errorf("body = %v", body)
	}
	if body["dayLimit"] != float64(1000000) {
		t.Errorf("dayLimit = %v", body["dayLimit"])
	}
}

func TestTeamAgentRoutes_GetAgent404(t *testing.T) {
	f := newTeamAgentFixture(t)
	// A stranger may not read team-a, so the route answers not found.
	if rec := f.do(t, f.stranger, http.MethodGet, "/pipelines/team-a/agent", nil); rec.Code != http.StatusNotFound {
		t.Errorf("stranger status = %d, want 404", rec.Code)
	}
	// team-bare has a tenant the reader may read, but no accepted Team agent.
	if rec := f.do(t, f.reader, http.MethodGet, "/pipelines/team-bare/agent", nil); rec.Code != http.StatusNotFound {
		t.Errorf("no-agent status = %d, want 404", rec.Code)
	}
}

func TestTeamAgentRoutes_Sessions(t *testing.T) {
	f := newTeamAgentFixture(t)
	sess := f.newTeamSession(t, agent.StateCompleted)
	rec := f.do(t, f.reader, http.MethodGet, "/pipelines/team-a/agent/sessions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var page struct {
		Items []struct {
			ID               string           `json:"id"`
			Kind             string           `json:"kind"`
			PendingApprovals []map[string]any `json:"pendingApprovals"`
		} `json:"items"`
	}
	decodeBody(t, rec, &page)
	if len(page.Items) != 1 || page.Items[0].ID != sess.ID.String() || page.Items[0].Kind != "team_agent" {
		t.Errorf("items = %+v", page.Items)
	}
	if page.Items[0].PendingApprovals == nil {
		t.Errorf("pendingApprovals must be an array, not null")
	}
}

func TestTeamAgentRoutes_CapabilitiesFourGroups(t *testing.T) {
	f := newTeamAgentFixture(t)
	f.grantUnattended(t, []string{"restart_run"})
	rec := f.do(t, f.reader, http.MethodGet, "/pipelines/team-a/agent/capabilities", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var caps struct {
		Reads            []capabilityJSON `json:"reads"`
		WritesByApproval []capabilityJSON `json:"writesByApproval"`
		Unattended       []capabilityJSON `json:"unattended"`
		Never            []capabilityJSON `json:"never"`
	}
	decodeBody(t, rec, &caps)
	names := func(list []capabilityJSON) string {
		out := make([]string, 0, len(list))
		for _, c := range list {
			out = append(out, c.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(caps.Reads); got != "get_run" {
		t.Errorf("reads = %s", got)
	}
	if got := names(caps.WritesByApproval); got != "run_pipeline" {
		t.Errorf("writesByApproval = %s", got)
	}
	if got := names(caps.Unattended); got != "restart_run,post_note" {
		t.Errorf("unattended = %s", got)
	}
	if got := names(caps.Never); got != "cancel_run,release_fleet_lock" {
		t.Errorf("never = %s", got)
	}
	for _, list := range [][]capabilityJSON{caps.Reads, caps.WritesByApproval, caps.Unattended, caps.Never} {
		for _, c := range list {
			if c.Name == "call_jenkins_tool" {
				t.Errorf("hidden tool listed")
			}
		}
	}
	for _, c := range caps.Unattended {
		if c.Name == "post_note" && c.Kind != "note" {
			t.Errorf("output kind = %s, want note", c.Kind)
		}
	}
}

func TestTeamAgentRoutes_RunMemberPlacedCluster(t *testing.T) {
	f := newTeamAgentFixture(t)
	rec := f.do(t, f.writer, http.MethodPost, "/pipelines/team-m/agent/run", map[string]any{"job": "app/build"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if len(f.events.events) != 1 {
		t.Fatalf("events = %d, want 1", len(f.events.events))
	}
	e := f.events.events[0]
	if e.Type != "teamagent.run_requested" || e.Cluster != "member-east" || e.ItemPath != "team-m/app/build" {
		t.Errorf("event = %+v", e)
	}
	if e.Reason != "test-ns/ta-member" || e.Source != "user" {
		t.Errorf("event reason/source = %q/%q", e.Reason, e.Source)
	}
}

func TestTeamAgentRoutes_Run409Disabled(t *testing.T) {
	f := newTeamAgentFixture(t)
	off := false
	f.source.items[0].Spec.Enabled = &off
	rec := f.do(t, f.writer, http.MethodPost, "/pipelines/team-a/agent/run", nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"disabled"`) {
		t.Errorf("status = %d body = %s", rec.Code, rec.Body)
	}
	if len(f.events.events) != 0 {
		t.Errorf("a refused run published an event")
	}
}

func TestTeamAgentRoutes_Run202KillSwitch(t *testing.T) {
	f := newTeamAgentFixture(t)
	if _, err := f.mem.PutSettings(context.Background(), agent.ScopeGlobal, store.SettingsPatch{Paused: teamAgentBoolPtr(true)}, "admin"); err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, f.writer, http.MethodPost, "/pipelines/team-a/agent/run", nil)
	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d body = %s, want 202 under the kill switch", rec.Code, rec.Body)
	}
}

func TestTeamAgentRoutes_RunReaderForbidden(t *testing.T) {
	f := newTeamAgentFixture(t)
	if rec := f.do(t, f.reader, http.MethodPost, "/pipelines/team-a/agent/run", nil); rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestTeamAgentRoutes_AgentsNeedsClusterWide(t *testing.T) {
	f := newTeamAgentFixture(t)
	if rec := f.do(t, f.reader, http.MethodGet, "/agents", nil); rec.Code != http.StatusForbidden {
		t.Errorf("reader status = %d, want 403", rec.Code)
	}
	rec := f.do(t, f.ops, http.MethodGet, "/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("ops status = %d body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"name":"ta-one"`) {
		t.Errorf("list lacks ta-one: %s", rec.Body)
	}
}

func TestTeamAgentRoutes_UnattendedGrantDoesNotExtend(t *testing.T) {
	f := newTeamAgentFixture(t)
	rec := f.do(t, f.ops, http.MethodPut, "/agents/test-ns/ta-one/unattended", map[string]any{"enabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("grant status = %d body = %s", rec.Code, rec.Body)
	}
	// A later spec edit adds a tool. The grant keeps the set it copied.
	f.source.items[0].Spec.UnattendedWrites = append(f.source.items[0].Spec.UnattendedWrites, "cancel_run")
	rec = f.do(t, f.ops, http.MethodGet, "/agents", nil)
	var list struct {
		Items []struct {
			Name              string   `json:"name"`
			UnattendedGranted []string `json:"unattendedGranted"`
		} `json:"items"`
	}
	decodeBody(t, rec, &list)
	for _, it := range list.Items {
		if it.Name == "ta-one" {
			if len(it.UnattendedGranted) != 1 || it.UnattendedGranted[0] != "restart_run" {
				t.Errorf("granted = %v, want [restart_run]", it.UnattendedGranted)
			}
			return
		}
	}
	t.Fatal("ta-one missing from the list")
}

func TestTeamAgentRoutes_Unattended403(t *testing.T) {
	f := newTeamAgentFixture(t)
	if rec := f.do(t, f.reader, http.MethodPut, "/agents/test-ns/ta-one/unattended", map[string]any{"enabled": true}); rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestTeamAgentRoutes_Unattended409Empty(t *testing.T) {
	f := newTeamAgentFixture(t)
	f.source.items[0].Spec.UnattendedWrites = nil
	rec := f.do(t, f.ops, http.MethodPut, "/agents/test-ns/ta-one/unattended", map[string]any{"enabled": true})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"nothing_to_grant"`) {
		t.Errorf("status = %d body = %s", rec.Code, rec.Body)
	}
}

// teamAgentBoolPtr returns a pointer to v.
func teamAgentBoolPtr(v bool) *bool { return &v }
