package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/loop"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// httpTestHost is a tool host that calls the HTTP MCP endpoint with the
// caller's claims, the way a remote client would.
type httpTestHost struct {
	url    string
	client *http.Client
}

func (h httpTestHost) send(ctx context.Context, method string, params any) (rpcEnvelope, error) {
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return rpcEnvelope{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(raw))
	if err != nil {
		return rpcEnvelope{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return rpcEnvelope{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return rpcEnvelope{}, err
	}
	var env rpcEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return rpcEnvelope{}, fmt.Errorf("http host: %w: %s", err, data)
	}
	return env, nil
}

func (h httpTestHost) List(ctx context.Context) ([]agent.ToolDef, error) {
	return listTools(ctx, h.send)
}

func (h httpTestHost) Call(ctx context.Context, name string, args json.RawMessage) (agent.ToolResult, error) {
	return callTool(ctx, h.send, name, args)
}

// loopScript answers one tool call, then a final text turn. An empty name and
// args mean get_run with {"run":"r1"}.
type loopScript struct {
	n    int
	name string
	args string
}

func (p *loopScript) Stream(context.Context, provider.Key, provider.Request) (<-chan provider.Delta, error) {
	ch := make(chan provider.Delta, 8)
	p.n++
	name, args := p.name, p.args
	if name == "" {
		name, args = "get_run", `{"run":"r1"}`
	}
	go func() {
		defer close(ch)
		if p.n == 1 {
			ch <- provider.Delta{ToolCall: &provider.ToolCallDelta{Index: 0, ID: "c1", Name: name, ArgumentsFragment: args}}
			ch <- provider.Delta{Stop: provider.StopToolUse}
			return
		}
		ch <- provider.Delta{Text: "done"}
		ch <- provider.Delta{Stop: provider.StopEndTurn}
	}()
	return ch, nil
}

type loopTenants struct{}

func (loopTenants) RunTenant(context.Context, string) (string, error)    { return "", nil }
func (loopTenants) TenantTenant(context.Context, string) (string, error) { return "", nil }

// runLoopOn runs one session through host and returns its event types.
func runLoopOn(t *testing.T, host loop.ToolHost) []agent.EventType {
	t.Helper()
	st, id := runLoopWith(t, host, &loopScript{})
	evs, err := st.Events(context.Background(), id, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]agent.EventType, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

// runLoopWith runs one session through host with the given script and returns
// its store and session id.
func runLoopWith(t *testing.T, host loop.ToolHost, script *loopScript) (*store.Memory, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	st := store.NewMemory(store.Options{Now: time.Now})
	id, err := agent.NewID()
	if err != nil {
		t.Fatal(err)
	}
	sess := agent.Session{
		ID: id, Kind: agent.KindAssistant, Principal: "alice", Cluster: "core",
		Trigger: agent.TriggerManual, Provider: "p", Model: "m",
		KeySource: agent.KeySourceChart, DataSharing: agent.DataLogs,
		State: agent.StateRunning, LeaseOwner: "owner-1",
	}
	user, err := agent.NewEvent(agent.EventUser, agent.UserPayload{Text: "why"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateSession(ctx, sess, []agent.EventInput{user}); err != nil {
		t.Fatal(err)
	}
	cfg := loop.RunConfig{
		Host:          host,
		DecideWrite:   loop.AlwaysApprove,
		DaySpend:      func(context.Context) (int64, int64, error) { return 0, 1_000_000, nil },
		Provider:      script,
		Model:         "m",
		Store:         st,
		SessionID:     id,
		Owner:         "owner-1",
		SessionBudget: 100_000,
		StepBudget:    32,
		Tenants:       loopTenants{},
		TenantLevels: func(context.Context) (map[string]agent.DataSharing, error) {
			return map[string]agent.DataSharing{}, nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := loop.Run(ctx, cfg, loop.TriggerStart); err != nil {
		t.Fatal(err)
	}
	return st, id
}

// TestLoop_RefusedWriteCreatesNoApprovalOnTwoHosts pins that a write with an
// undeclared argument creates no approval and gives the model the refusal as a
// tool error, on both hosts.
func TestLoop_RefusedWriteCreatesNoApprovalOnTwoHosts(t *testing.T) {
	srv := NewServer(newTestDeps())
	claims := &auth.Claims{PreferredUsername: "alice", Subject: "alice"}
	handler := HTTPHandler(srv)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(auth.ContextWithClaims(r.Context(), claims)))
	}))
	defer hs.Close()
	hosts := map[string]loop.ToolHost{
		"in-process": NewInProcessHost(srv, claims),
		"http":       httpTestHost{url: hs.URL, client: hs.Client()},
	}
	want := "input schema validation failed: <root>: " + notAllowedText(undeclaredArg)
	for name, host := range hosts {
		t.Run(name, func(t *testing.T) {
			script := &loopScript{name: "restart_run", args: `{"run":"r1","stage":"Build","` + undeclaredArg + `":1}`}
			st, id := runLoopWith(t, host, script)
			ctx := context.Background()
			page, err := st.ListApprovals(ctx, store.ApprovalQuery{Principal: "alice", SessionID: id})
			if err != nil || len(page.Items) != 0 {
				t.Fatalf("approvals = %v, %v; want none", page.Items, err)
			}
			sess, err := st.GetSession(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if sess.State != agent.StateCompleted {
				t.Fatalf("state = %s, want completed", sess.State)
			}
			evs, err := st.Events(ctx, id, 0, 1000)
			if err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, e := range evs {
				if e.Type != agent.EventToolResult {
					continue
				}
				var res agent.ToolResultPayload
				if err := json.Unmarshal(e.Payload, &res); err != nil {
					t.Fatal(err)
				}
				found = true
				if !res.IsError || res.Content != want {
					t.Errorf("tool_result = %+v, want error %q", res, want)
				}
			}
			if !found {
				t.Error("no tool_result event")
			}
		})
	}
}

// TestLoop_SameOnTwoHosts proves the loop writes the same transcript shape
// whether the tools come from the in-process host or from the HTTP endpoint.
func TestLoop_SameOnTwoHosts(t *testing.T) {
	srv := NewServer(newTestDeps())
	claims := &auth.Claims{PreferredUsername: "alice", Subject: "alice"}

	inproc := NewInProcessHost(srv, claims)
	inprocTypes := runLoopOn(t, inproc)

	handler := HTTPHandler(srv)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(auth.ContextWithClaims(r.Context(), claims)))
	}))
	defer hs.Close()
	httpTypes := runLoopOn(t, httpTestHost{url: hs.URL, client: hs.Client()})

	if fmt.Sprint(inprocTypes) != fmt.Sprint(httpTypes) {
		t.Errorf("event sequences differ:\n in-process %v\n http       %v", inprocTypes, httpTypes)
	}
	if len(inprocTypes) == 0 {
		t.Fatal("no events were written")
	}
}
