package api

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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/loop"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/sealedkey"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// scriptedAssistantProvider answers one turn of deltas per call.
type scriptedAssistantProvider struct {
	turns [][]provider.Delta
	n     int
}

func (p *scriptedAssistantProvider) Stream(context.Context, provider.Key, provider.Request) (<-chan provider.Delta, error) {
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

// assistantTestHost offers a read tool and a write tool, and echoes each call.
type assistantTestHost struct{}

func (assistantTestHost) List(context.Context) ([]agent.ToolDef, error) {
	return []agent.ToolDef{
		{Name: "get_run", Class: agent.ClassRead},
		{Name: "restart_run", Class: agent.ClassWrite},
	}, nil
}

func (assistantTestHost) Call(_ context.Context, name string, _ json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "ok:" + name}, nil
}

type assistantFixture struct {
	rt     *AssistantRuntime
	mem    *store.Memory
	deps   *Dependencies
	router http.Handler
	admin  *auth.Claims
	viewer *auth.Claims
}

// newAssistantFixture wires a runtime with one scripted provider and a router
// that carries the admin claims of the test caller.
func newAssistantFixture(t *testing.T, turns ...[]provider.Delta) *assistantFixture {
	t.Helper()
	mem := store.NewMemory(store.Options{Now: time.Now})
	factories := map[agent.ProviderKind]provider.Factory{
		agent.ProviderOpenAICompatible: func(provider.Endpoint) (provider.Provider, error) {
			return &scriptedAssistantProvider{turns: turns}, nil
		},
	}
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
	reg, err := provider.NewRegistry(cfg, factories, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt := &AssistantRuntime{
		Config:   cfg,
		Registry: reg,
		Store:    newNotifyingStore(mem, nil, logger),
		HostFor:  func(*auth.Claims) loop.ToolHost { return assistantTestHost{} },
		Owner:    "owner-test",
		Cluster:  "core",
		Logger:   logger,
	}
	admin := &auth.Claims{Subject: "admin-user", PreferredUsername: "admin", IssuedAt: time.Now()}
	viewer := &auth.Claims{Subject: "viewer-user", PreferredUsername: "viewer", IssuedAt: time.Now()}
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "admin"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"assistant"}, Verbs: []string{"use"}}}}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "admin-b"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "admin-user"}}, RoleRef: "admin"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "viewer-b"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "viewer-user"}}, RoleRef: "viewer"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "viewer2-b"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "viewer2-user"}}, RoleRef: "viewer"}},
	}
	deps := &Dependencies{
		Authorizer: testResolverAndAuthorizer(roles, bindings),
		Logger:     logger,
		Assistant:  rt,
	}
	return &assistantFixture{rt: rt, mem: mem, deps: deps, router: NewRouter(deps), admin: admin, viewer: viewer}
}

// do sends one request as the given caller and returns the recorder.
func (f *assistantFixture) do(t *testing.T, claims *auth.Claims, method, path string, body any) *httptest.ResponseRecorder {
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
	f.router.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

func assistantTextTurn(text string) []provider.Delta {
	return []provider.Delta{
		{Text: text},
		{Usage: &provider.Usage{PromptTokens: 10, CompletionTokens: 5}},
		{Stop: provider.StopEndTurn},
	}
}

func assistantRestartTurn() []provider.Delta {
	return []provider.Delta{
		{ToolCall: &provider.ToolCallDelta{Index: 0, ID: "c1", Name: "restart_run", ArgumentsFragment: `{"run":"r1"}`}},
		{Usage: &provider.Usage{PromptTokens: 10, CompletionTokens: 5}},
		{Stop: provider.StopToolUse},
	}
}

func TestAssistantCreate_RunsToCompletion(t *testing.T) {
	f := newAssistantFixture(t, assistantTextTurn("hello there"))
	rec := f.do(t, f.viewer, http.MethodPost, "/assistant/sessions", map[string]any{"message": "hi"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var sess assistantSessionJSON
	decodeBody(t, rec, &sess)
	f.rt.Wait()

	rec = f.do(t, f.viewer, http.MethodGet, "/assistant/sessions/"+sess.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	var detail struct {
		Session assistantSessionJSON `json:"session"`
	}
	decodeBody(t, rec, &detail)
	if detail.Session.State != "completed" {
		t.Errorf("state = %s, want completed", detail.Session.State)
	}
}

func TestAssistantCreate_KillSwitchSkips(t *testing.T) {
	f := newAssistantFixture(t)
	paused := true
	if _, err := f.mem.PutSettings(context.Background(), agent.ScopeGlobal, store.SettingsPatch{Paused: &paused}, "admin-user"); err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, f.viewer, http.MethodPost, "/assistant/sessions", map[string]any{"message": "hi"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var sess assistantSessionJSON
	decodeBody(t, rec, &sess)
	if sess.State != "skipped" || sess.SkipReason == nil || *sess.SkipReason != "kill_switch" {
		t.Errorf("session = %+v, want skipped by kill_switch", sess)
	}
}

func TestAssistantCreate_BadRequests(t *testing.T) {
	f := newAssistantFixture(t)
	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"neither message nor prompt", map[string]any{}, http.StatusBadRequest},
		{"unknown prompt", map[string]any{"prompt": "nope"}, http.StatusBadRequest},
		{"propose prompt is not startable", map[string]any{"prompt": "propose-pipeline"}, http.StatusForbidden},
		{"server-only context key", map[string]any{"message": "x", "context": map[string]any{"repo": "https://x"}}, http.StatusBadRequest},
		{"unknown provider", map[string]any{"message": "x", "provider": "nope"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(t, f.viewer, http.MethodPost, "/assistant/sessions", tc.body)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestAssistantCreate_NoUseVerbForbidden(t *testing.T) {
	f := newAssistantFixture(t)
	stranger := &auth.Claims{Subject: "stranger", IssuedAt: time.Now()}
	rec := f.do(t, stranger, http.MethodPost, "/assistant/sessions", map[string]any{"message": "hi"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestAssistantApprove_RunsWriteAndResumes(t *testing.T) {
	f := newAssistantFixture(t,
		assistantRestartTurn(),
		assistantTextTurn("restarted"),
	)
	rec := f.do(t, f.viewer, http.MethodPost, "/assistant/sessions", map[string]any{"message": "restart it"})
	var sess assistantSessionJSON
	decodeBody(t, rec, &sess)
	f.rt.Wait()

	page, err := f.mem.ListApprovals(context.Background(), store.ApprovalQuery{Principal: "viewer-user", SessionID: mustUUID(t, sess.ID)})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("approvals = %v, %v", page.Items, err)
	}
	rec = f.do(t, f.viewer, http.MethodPost, "/assistant/approvals/"+page.Items[0].ID.String()+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve status = %d body = %s", rec.Code, rec.Body)
	}
	var out struct {
		Result struct {
			Content string `json:"content"`
		} `json:"result"`
	}
	decodeBody(t, rec, &out)
	if out.Result.Content != "ok:restart_run" {
		t.Errorf("result = %q", out.Result.Content)
	}
	f.rt.Wait()
	rec = f.do(t, f.viewer, http.MethodGet, "/assistant/sessions/"+sess.ID, nil)
	var detail struct {
		Session assistantSessionJSON `json:"session"`
	}
	decodeBody(t, rec, &detail)
	if detail.Session.State != "completed" {
		t.Errorf("after resume: state = %s, want completed", detail.Session.State)
	}
}

func TestAssistantDeny_WritesDenialAndResumes(t *testing.T) {
	f := newAssistantFixture(t,
		assistantRestartTurn(),
		assistantTextTurn("understood"),
	)
	rec := f.do(t, f.viewer, http.MethodPost, "/assistant/sessions", map[string]any{"message": "restart it"})
	var sess assistantSessionJSON
	decodeBody(t, rec, &sess)
	f.rt.Wait()
	page, _ := f.mem.ListApprovals(context.Background(), store.ApprovalQuery{Principal: "viewer-user", SessionID: mustUUID(t, sess.ID)})
	rec = f.do(t, f.viewer, http.MethodPost, "/assistant/approvals/"+page.Items[0].ID.String()+"/deny", map[string]any{"reason": "not now"})
	if rec.Code != http.StatusOK {
		t.Fatalf("deny status = %d body = %s", rec.Code, rec.Body)
	}
	f.rt.Wait()
	rec = f.do(t, f.viewer, http.MethodGet, "/assistant/sessions/"+sess.ID, nil)
	var detail struct {
		Session   assistantSessionJSON    `json:"session"`
		Approvals []assistantApprovalJSON `json:"approvals"`
	}
	decodeBody(t, rec, &detail)
	if detail.Session.State != "completed" {
		t.Errorf("state = %s, want completed", detail.Session.State)
	}
	if len(detail.Approvals) != 1 || detail.Approvals[0].State != "denied" || detail.Approvals[0].Result == nil {
		t.Fatalf("approvals = %+v", detail.Approvals)
	}
	if !strings.HasPrefix(*detail.Approvals[0].Result, "denied by") {
		t.Errorf("result = %q", *detail.Approvals[0].Result)
	}
}

func TestAssistantApprove_OtherUser404(t *testing.T) {
	f := newAssistantFixture(t, assistantRestartTurn())
	rec := f.do(t, f.viewer, http.MethodPost, "/assistant/sessions", map[string]any{"message": "restart it"})
	var sess assistantSessionJSON
	decodeBody(t, rec, &sess)
	f.rt.Wait()
	page, _ := f.mem.ListApprovals(context.Background(), store.ApprovalQuery{Principal: "viewer-user", SessionID: mustUUID(t, sess.ID)})
	other := &auth.Claims{Subject: "other", IssuedAt: time.Now()}
	rec = f.do(t, other, http.MethodPost, "/assistant/approvals/"+page.Items[0].ID.String()+"/approve", nil)
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want a refusal", rec.Code)
	}
}

func TestAssistantSettings_AdminOnlyWrites(t *testing.T) {
	f := newAssistantFixture(t)
	rec := f.do(t, f.viewer, http.MethodPut, "/assistant/settings", map[string]any{"paused": true})
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer PUT settings = %d, want 403", rec.Code)
	}
	rec = f.do(t, f.admin, http.MethodPut, "/assistant/settings", map[string]any{"paused": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin PUT settings = %d body = %s", rec.Code, rec.Body)
	}
	rec = f.do(t, f.viewer, http.MethodGet, "/assistant/settings", nil)
	if strings.Contains(rec.Body.String(), "limits") {
		t.Errorf("viewer GET settings leaks limits: %s", rec.Body)
	}
}

func TestAssistantProviders_BaseURLAdminOnly(t *testing.T) {
	f := newAssistantFixture(t)
	rec := f.do(t, f.viewer, http.MethodGet, "/assistant/providers", nil)
	if strings.Contains(rec.Body.String(), "baseURL") {
		t.Errorf("viewer sees a base URL: %s", rec.Body)
	}
	rec = f.do(t, f.admin, http.MethodGet, "/assistant/providers", nil)
	if !strings.Contains(rec.Body.String(), "baseURL") {
		t.Errorf("admin does not see a base URL: %s", rec.Body)
	}
}

func TestAssistantPutMe_UnknownModel400(t *testing.T) {
	f := newAssistantFixture(t)
	model := "not-listed"
	rec := f.do(t, f.viewer, http.MethodPut, "/me/assistant", map[string]any{"model": model})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestAssistantMe_DisabledAvailableFalse(t *testing.T) {
	deps := &Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := NewRouter(deps)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/assistant", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "u"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"available":false`) {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestAssistantPutKey_APIKeyCaller409(t *testing.T) {
	f := newAssistantFixture(t)
	apiKeyCaller := &auth.Claims{Subject: "viewer-user"}
	f.rt.Sealer = mustSealer(t)
	rec := f.do(t, apiKeyCaller, http.MethodPut, "/me/assistant/key", map[string]any{"provider": "smoke", "apiKey": "k"})
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

func TestAssistantPutKey_NoSigner503(t *testing.T) {
	f := newAssistantFixture(t)
	rec := f.do(t, f.viewer, http.MethodPut, "/me/assistant/key", map[string]any{"provider": "smoke", "apiKey": "k"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestAssistantPutKey_CookieAttrs(t *testing.T) {
	f := newAssistantFixture(t)
	f.rt.Sealer = mustSealer(t)
	rec := f.do(t, f.viewer, http.MethodPut, "/me/assistant/key", map[string]any{"provider": "smoke", "apiKey": "k"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name != AssistantKeyCookie {
			continue
		}
		if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/api/v1" || c.Value == "" {
			t.Errorf("cookie = %+v", c)
		}
		if c.MaxAge != 0 {
			t.Errorf("cookie has a lifetime: MaxAge = %d", c.MaxAge)
		}
		return
	}
	t.Error("no assistant key cookie was set")
}

func TestAssistantCancel_FinalState409(t *testing.T) {
	f := newAssistantFixture(t, assistantTextTurn("done"))
	rec := f.do(t, f.viewer, http.MethodPost, "/assistant/sessions", map[string]any{"message": "hi"})
	var sess assistantSessionJSON
	decodeBody(t, rec, &sess)
	f.rt.Wait()
	rec = f.do(t, f.viewer, http.MethodPost, "/assistant/sessions/"+sess.ID+"/cancel", nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("cancel of a completed session = %d, want 409", rec.Code)
	}
}

func mustUUID(t *testing.T, raw string) uuid.UUID {
	t.Helper()
	u, ok := sessionUUID(raw)
	if !ok {
		t.Fatalf("bad uuid %q", raw)
	}
	return u
}

func mustSealer(t *testing.T) *sealedkey.Sealer {
	t.Helper()
	s, err := sealedkey.New("test-signing-key")
	if err != nil {
		t.Fatal(err)
	}
	return s
}
