package sessions

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/loop"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	agentstore "github.com/varroaci/varroa-jenkins/internal/agent/store"
)

// scriptedProvider answers its first call with one read-tool call, and pauses
// the kill switch while it does so. The loop then reaches the preflight before
// its second call, which must not reach the provider.
type scriptedProvider struct {
	calls int32
	store agentstore.Store
}

func (p *scriptedProvider) Stream(ctx context.Context, _ provider.Key, _ provider.Request) (<-chan provider.Delta, error) {
	n := atomic.AddInt32(&p.calls, 1)
	ch := make(chan provider.Delta, 4)
	if n == 1 {
		yes := true
		_, _ = p.store.PutSettings(ctx, agent.ScopeGlobal, agentstore.SettingsPatch{Paused: &yes}, "admin")
		ch <- provider.Delta{ToolCall: &provider.ToolCallDelta{Index: 0, ID: "c1", Name: "read_thing", ArgumentsFragment: "{}"}}
		ch <- provider.Delta{Stop: provider.StopToolUse}
	} else {
		ch <- provider.Delta{Text: "should not run", Stop: provider.StopEndTurn}
	}
	close(ch)
	return ch, nil
}

// oneToolHost lists one read tool and answers it.
type oneToolHost struct{}

func (oneToolHost) List(context.Context) ([]agent.ToolDef, error) {
	return []agent.ToolDef{{Name: "read_thing", Class: agent.ClassRead}}, nil
}

func (oneToolHost) Call(context.Context, string, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "ok"}, nil
}

// fixedTenants maps every run and Tenant to the tenant obj.
type fixedTenants struct{}

func (fixedTenants) RunTenant(context.Context, string) (string, error) { return "obj", nil }

func (fixedTenants) TenantTenant(context.Context, string) (string, error) { return "obj", nil }

// TestKillSwitch_EndsCancelledKillSwitch proves that a kill switch set during a
// run stops the next provider call. The session ends cancelled with the
// kill_switch code, and the provider sees one call.
func TestKillSwitch_EndsCancelledKillSwitch(t *testing.T) {
	ctx := context.Background()
	st := agentstore.NewMemory(agentstore.Options{Now: time.Now})
	id := uuid.Must(uuid.NewV7())
	start, err := agent.NewEvent(agent.EventUser, agent.UserPayload{Text: "go"})
	if err != nil {
		t.Fatal(err)
	}
	sess := agent.Session{
		ID: id, Kind: agent.KindTenantAgent, Principal: "agent:team-a/obj/triage", Tenant: "obj",
		Cluster: "core", Trigger: "run.failed", Provider: "p", Model: "m",
		KeySource: agent.KeySourceChart, DataSharing: agent.DataLogs,
		State: agent.StateRunning, LeaseOwner: "pod-a",
		LeaseExpiresAt: ptrTime(time.Now().Add(time.Hour)), StartedAt: time.Now(),
	}
	if _, _, err := st.CreateSession(ctx, sess, []agent.EventInput{start}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	prov := &scriptedProvider{store: st}
	cfg := loop.RunConfig{
		Host:           oneToolHost{},
		DecideWrite:    func(agent.ToolDef, json.RawMessage) loop.WriteDecision { return loop.WriteApprove },
		DaySpend:       func(context.Context) (int64, int64, error) { return 0, 1 << 40, nil },
		Provider:       prov,
		Model:          "m",
		Store:          st,
		SessionID:      id,
		Owner:          "pod-a",
		SessionBudget:  1 << 40,
		StepBudget:     8,
		ApprovalExpiry: time.Hour,
		Tenants:        fixedTenants{},
		TenantLevels:   func(context.Context) (map[string]agent.DataSharing, error) { return nil, nil },
		Now:            time.Now,
	}
	if err := loop.Run(ctx, cfg, loop.TriggerStart); err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
	if got := atomic.LoadInt32(&prov.calls); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
	got, err := st.GetSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != agent.StateCancelled {
		t.Fatalf("state = %s, want cancelled", got.State)
	}
	evs, err := st.Events(ctx, id, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var code agent.ErrorCode
	for _, e := range evs {
		if e.Type == agent.EventError {
			var p agent.ErrorPayload
			if json.Unmarshal(e.Payload, &p) == nil {
				code = p.Code
			}
		}
	}
	if code != agent.CodeKillSwitch {
		t.Fatalf("error code = %q, want %q", code, agent.CodeKillSwitch)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
