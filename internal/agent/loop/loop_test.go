package loop

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/guide"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
)

// stubHost offers one read tool and one write tool. Every call echoes its name.
type stubHost struct{ calls []string }

func (h *stubHost) List(context.Context) ([]agent.ToolDef, error) {
	return []agent.ToolDef{
		{Name: "get_run", Class: agent.ClassRead},
		{Name: "restart_run", Class: agent.ClassWrite},
		{Name: "call_jenkins_tool", Class: agent.ClassWrite, Hidden: true},
	}, nil
}

func (h *stubHost) Call(_ context.Context, name string, _ json.RawMessage) (agent.ToolResult, error) {
	h.calls = append(h.calls, name)
	return agent.ToolResult{Content: "ok:" + name}, nil
}

// scriptProvider answers one turn of deltas per call.
type scriptProvider struct {
	turns [][]provider.Delta
	n     int
}

func (p *scriptProvider) Stream(context.Context, provider.Key, provider.Request) (<-chan provider.Delta, error) {
	ch := make(chan provider.Delta, 64)
	turn := []provider.Delta{{Stop: provider.StopEndTurn}}
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

type noTenants struct{}

func (noTenants) RunTenant(context.Context, string) (string, error)  { return "", nil }
func (noTenants) TeamTenant(context.Context, string) (string, error) { return "", nil }

func toolTurn(id, name, args string) []provider.Delta {
	return []provider.Delta{
		{ToolCall: &provider.ToolCallDelta{Index: 0, ID: id, Name: name, ArgumentsFragment: args}},
		{Usage: &provider.Usage{PromptTokens: 10, CompletionTokens: 5}},
		{Stop: provider.StopToolUse},
	}
}

func textTurn(text string) []provider.Delta {
	return []provider.Delta{
		{Text: text},
		{Usage: &provider.Usage{PromptTokens: 10, CompletionTokens: 5}},
		{Stop: provider.StopEndTurn},
	}
}

type fixture struct {
	st   *store.Memory
	id   uuid.UUID
	host *stubHost
	cfg  RunConfig
}

// newFixture creates a running session under owner-1 with one user message.
func newFixture(t *testing.T, turns ...[]provider.Delta) fixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	st := store.NewMemory(store.Options{Now: func() time.Time { return now }})
	id, err := agent.NewID()
	if err != nil {
		t.Fatal(err)
	}
	sess := agent.Session{
		ID: id, Kind: agent.KindAssistant, Principal: "u", Cluster: "core",
		Trigger: agent.TriggerManual, Provider: "p", Model: "m",
		KeySource: agent.KeySourceChart, DataSharing: agent.DataLogs,
		State: agent.StateRunning, LeaseOwner: "owner-1",
	}
	user, err := agent.NewEvent(agent.EventUser, agent.UserPayload{Text: "why did it fail"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateSession(ctx, sess, []agent.EventInput{user}); err != nil {
		t.Fatal(err)
	}
	host := &stubHost{}
	cfg := RunConfig{
		Host:           host,
		DecideWrite:    AlwaysApprove,
		DaySpend:       func(context.Context) (int64, int64, error) { return 0, 1_000_000, nil },
		Provider:       &scriptProvider{turns: turns},
		Model:          "m",
		Store:          st,
		SessionID:      id,
		Owner:          "owner-1",
		SessionBudget:  100_000,
		StepBudget:     32,
		ApprovalExpiry: time.Hour,
		Tenants:        noTenants{},
		TenantLevels: func(context.Context) (map[string]agent.DataSharing, error) {
			return map[string]agent.DataSharing{}, nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return now },
	}
	return fixture{st: st, id: id, host: host, cfg: cfg}
}

func (f fixture) types(t *testing.T) []agent.EventType {
	t.Helper()
	evs, err := f.st.Events(context.Background(), f.id, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]agent.EventType, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

func (f fixture) session(t *testing.T) agent.Session {
	t.Helper()
	s, err := f.st.GetSession(context.Background(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRun_ReadToolCompletes(t *testing.T) {
	f := newFixture(t,
		toolTurn("c1", "get_run", `{"run":"r1"}`),
		textTurn("The stage failed."),
	)
	if err := Run(context.Background(), f.cfg, TriggerStart); err != nil {
		t.Fatal(err)
	}
	s := f.session(t)
	if s.State != agent.StateCompleted {
		t.Fatalf("state = %s, want completed", s.State)
	}
	if s.Steps != 2 || s.PromptTokens != 20 {
		t.Errorf("steps = %d prompt = %d, want 2 and 20", s.Steps, s.PromptTokens)
	}
	if len(f.host.calls) != 1 || f.host.calls[0] != "get_run" {
		t.Errorf("host calls = %v, want [get_run]", f.host.calls)
	}
	want := []agent.EventType{agent.EventUser, agent.EventAssistant, agent.EventToolCall,
		agent.EventToolResult, agent.EventAssistant, agent.EventDone}
	if got := f.types(t); !equalTypes(got, want) {
		t.Errorf("event types = %v, want %v", got, want)
	}
}

func TestRun_HiddenToolIsUnknown(t *testing.T) {
	f := newFixture(t,
		toolTurn("c1", "call_jenkins_tool", `{}`),
		textTurn("ok"),
	)
	if err := Run(context.Background(), f.cfg, TriggerStart); err != nil {
		t.Fatal(err)
	}
	if len(f.host.calls) != 0 {
		t.Errorf("a hidden tool ran: %v", f.host.calls)
	}
}

func TestRun_WriteParksThenResumes(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t,
		toolTurn("c1", "restart_run", `{"run":"r1"}`),
		textTurn("Restarted."),
	)
	if err := Run(ctx, f.cfg, TriggerStart); err != nil {
		t.Fatal(err)
	}
	if s := f.session(t); s.State != agent.StateWaitingApproval || s.LeaseOwner != "" {
		t.Fatalf("after park: state = %s lease = %q", s.State, s.LeaseOwner)
	}
	page, err := f.st.ListApprovals(ctx, store.ApprovalQuery{Principal: "u", SessionID: f.id})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("approvals = %v, %v; want one", page.Items, err)
	}
	ap := page.Items[0]
	if ap.State != agent.ApprovalPending || ap.Tool != "restart_run" {
		t.Fatalf("approval = %+v", ap)
	}

	// The approver resolves the approval, appends its result, and takes the lease.
	if _, err := f.st.ResolveApproval(ctx, ap.ID, store.Resolution{State: agent.ApprovalApproved, By: "u", Owner: "u"}); err != nil {
		t.Fatal(err)
	}
	res, err := agent.NewEvent(agent.EventToolResult, agent.ToolResultPayload{CallID: "c1", Name: "restart_run", Content: "ok:restart_run"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.AppendEvent(ctx, f.id, store.Guard{}, res); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.TakeLease(ctx, f.id, "owner-2", []agent.SessionState{agent.StateWaitingApproval}); err != nil {
		t.Fatal(err)
	}
	f.cfg.Owner = "owner-2"
	if err := Run(ctx, f.cfg, TriggerResume); err != nil {
		t.Fatal(err)
	}
	if s := f.session(t); s.State != agent.StateCompleted {
		t.Fatalf("after resume: state = %s, want completed", s.State)
	}
}

func TestRun_KillSwitchCancels(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, textTurn("never"))
	paused := true
	if _, err := f.st.PutSettings(ctx, agent.ScopeGlobal, store.SettingsPatch{Paused: &paused}, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, f.cfg, TriggerStart); err != nil {
		t.Fatal(err)
	}
	if s := f.session(t); s.State != agent.StateCancelled {
		t.Fatalf("state = %s, want cancelled", s.State)
	}
}

func TestRun_SessionBudgetFails(t *testing.T) {
	f := newFixture(t,
		toolTurn("c1", "get_run", `{}`),
		textTurn("never reached"),
	)
	f.cfg.SessionBudget = 10 // the first call spends 15 tokens
	if err := Run(context.Background(), f.cfg, TriggerStart); err != nil {
		t.Fatal(err)
	}
	if s := f.session(t); s.State != agent.StateFailed {
		t.Fatalf("state = %s, want failed", s.State)
	}
}

func TestRun_StepBudgetFails(t *testing.T) {
	f := newFixture(t, toolTurn("c1", "get_run", `{}`), toolTurn("c2", "get_run", `{}`))
	f.cfg.StepBudget = 1
	if err := Run(context.Background(), f.cfg, TriggerStart); err != nil {
		t.Fatal(err)
	}
	if s := f.session(t); s.State != agent.StateFailed {
		t.Fatalf("state = %s, want failed", s.State)
	}
}

func TestRun_MaxTokensPartialToolCallFails(t *testing.T) {
	f := newFixture(t, []provider.Delta{
		{ToolCall: &provider.ToolCallDelta{Index: 0, ID: "c1", Name: "get_run", ArgumentsFragment: `{"run":`}},
		{Stop: provider.StopMaxTokens},
	})
	if err := Run(context.Background(), f.cfg, TriggerStart); err != nil {
		t.Fatal(err)
	}
	if s := f.session(t); s.State != agent.StateFailed {
		t.Fatalf("state = %s, want failed", s.State)
	}
	if len(f.host.calls) != 0 {
		t.Errorf("a partial tool call ran: %v", f.host.calls)
	}
}

func TestRun_ProviderErrorIsGeneric(t *testing.T) {
	f := newFixture(t, []provider.Delta{{Stop: provider.StopError, Err: io.ErrUnexpectedEOF}})
	if err := Run(context.Background(), f.cfg, TriggerStart); err != nil {
		t.Fatal(err)
	}
	if s := f.session(t); s.State != agent.StateFailed {
		t.Fatalf("state = %s, want failed", s.State)
	}
	evs, _ := f.st.Events(context.Background(), f.id, 0, 1000)
	for _, e := range evs {
		if e.Type == agent.EventError && strings.Contains(string(e.Payload), "unexpected EOF") {
			t.Errorf("the raw provider error reached the transcript: %s", e.Payload)
		}
	}
}

func TestRebuild_TurnsAndResults(t *testing.T) {
	ev := func(typ agent.EventType, payload any) agent.Event {
		in, err := agent.NewEvent(typ, payload)
		if err != nil {
			t.Fatal(err)
		}
		return agent.Event{Type: in.Type, Payload: in.Payload}
	}
	events := []agent.Event{
		ev(agent.EventUser, agent.UserPayload{Text: "why"}),
		ev(agent.EventAssistant, agent.AssistantPayload{Turn: 1, Text: "Let me ", Final: false}),
		ev(agent.EventAssistant, agent.AssistantPayload{Turn: 1, Text: "check.", Final: true}),
		ev(agent.EventToolCall, agent.ToolCallPayload{CallID: "c1", Name: "get_run", Arguments: json.RawMessage(`{}`)}),
		ev(agent.EventToolResult, agent.ToolResultPayload{CallID: "c1", Name: "get_run", Content: "ok"}),
		ev(agent.EventAssistant, agent.AssistantPayload{Turn: 2, Text: "done", Final: true}),
		ev(agent.EventDone, agent.DonePayload{State: agent.StateCompleted}),
	}
	msgs, err := Rebuild(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4: %+v", len(msgs), msgs)
	}
	if msgs[1].Text != "Let me check." || len(msgs[1].ToolCalls) != 1 {
		t.Errorf("assistant turn 1 = %+v", msgs[1])
	}
	if msgs[2].Role != provider.RoleTool || msgs[2].ToolCallID != "c1" {
		t.Errorf("tool result = %+v", msgs[2])
	}
	if msgs[3].Text != "done" {
		t.Errorf("turn 2 = %+v", msgs[3])
	}
}

func TestCutResult(t *testing.T) {
	short := "abc"
	if CutResult(short) != short {
		t.Error("a short result changed")
	}
	long := strings.Repeat("é", MaxResultBytes)
	got := CutResult(long)
	if !strings.HasSuffix(got, "[truncated at 32768 bytes]") {
		t.Errorf("no truncation marker: %q", got[len(got)-40:])
	}
	if !json.Valid([]byte(`"` + strings.TrimSuffix(got, "\n[truncated at 32768 bytes]") + `"`)) {
		t.Error("the cut split a UTF-8 rune")
	}
}

func TestSystemText_NamesRepo(t *testing.T) {
	text := SystemText(map[string]any{agent.CtxRepo: "https://git.example/r", agent.CtxRun: "r1"}, agent.DataMetadata)
	if !strings.Contains(text, "Context repo: https://git.example/r") {
		t.Errorf("repo missing: %q", text)
	}
	if !strings.Contains(text, "Data level: metadata") {
		t.Errorf("data level missing: %q", text)
	}
}

func TestSystemText_CarriesGuideBeforeContext(t *testing.T) {
	text := SystemText(map[string]any{agent.CtxRun: "r1"}, agent.DataLogs)
	if !strings.HasPrefix(text, systemBase) {
		t.Fatalf("system text does not start with the base text: %.40q", text)
	}
	gi := strings.Index(text, guide.Text)
	if gi < 0 {
		t.Fatal("system text does not carry the operator guide")
	}
	for _, after := range []string{"Context run: r1", "Data level:"} {
		if ci := strings.Index(text, after); ci < 0 || ci < gi {
			t.Errorf("%q must follow the guide (guide at %d, %q at %d)", after, gi, after, ci)
		}
	}
}

func TestAlwaysApprove(t *testing.T) {
	if AlwaysApprove(agent.ToolDef{}, nil) != WriteApprove {
		t.Error("AlwaysApprove must park every write")
	}
}

func equalTypes(a, b []agent.EventType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
