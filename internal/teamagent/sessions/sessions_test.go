package sessions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/loop"
)

var testKey = []byte(strings.Repeat("k", 32))

// recordingMCP answers tools/list and tools/call and keeps every request.
type recordingMCP struct {
	mu      sync.Mutex
	bodies  []string
	headers []string
}

func (m *recordingMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.bodies = append(m.bodies, string(b))
	m.headers = append(m.headers, r.Header.Get("Authorization"))
	m.mu.Unlock()
	var req struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
	}
	_ = json.Unmarshal(b, &req)
	w.Header().Set("Content-Type", "application/json")
	switch req.Method {
	case "tools/list":
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[`+
			`{"name":"read_thing","annotations":{"readOnlyHint":true}},`+
			`{"name":"post_note","annotations":{}},`+
			`{"name":"open_issue","annotations":{}}]}}`)
	case "tools/call":
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}],"isError":false}}`)
	default:
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"message":"unknown method"}}`)
	}
}

func TestHTTPHost_NoBodyHoldsToken(t *testing.T) {
	srv := &recordingMCP{}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	h := NewHTTPHost(ts.URL, testKey, "agent:team-a/obj/triage", types.UID("uid-1"), uuid.Must(uuid.NewV7()), []string{"note"}, func() time.Time { return now })

	tools, err := h.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name] = true
	}
	if !names["read_thing"] || !names["post_note"] || names["open_issue"] {
		t.Fatalf("tools = %v; want the read tool and the enabled note output only", names)
	}
	res, err := h.Call(context.Background(), "read_thing", json.RawMessage(`{"team":"obj"}`))
	if err != nil || res.Content != "ok" || res.IsError {
		t.Fatalf("Call = %+v, %v", res, err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, a := range srv.headers {
		if !strings.HasPrefix(a, "Bearer ") {
			t.Fatalf("Authorization header = %q, want a bearer token", a)
		}
		raw := strings.TrimPrefix(a, "Bearer ")
		for _, b := range srv.bodies {
			if strings.Contains(b, raw) {
				t.Fatalf("a request body holds the bearer token")
			}
		}
	}
}

func TestHTTPHost_RemintsNearExpiry(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	h := NewHTTPHost("http://unused", testKey, "agent:team-a/obj/triage", types.UID("uid-1"), uuid.Must(uuid.NewV7()), nil,
		func() time.Time { return now })
	first, err := h.token()
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(12 * time.Minute)
	h.now = func() time.Time { return now }
	second, err := h.token()
	if err != nil {
		t.Fatal(err)
	}
	if second.ExpiresAt().Equal(first.ExpiresAt()) {
		t.Fatal("a token 3 minutes from expiry was not replaced")
	}
}

func TestDecision_OffOutputIsApproval(t *testing.T) {
	decide := NewDecision([]string{"run_pipeline"}, []string{"note"})
	cases := map[string]loop.WriteDecision{
		"post_note":    loop.WriteRunNow,
		"open_issue":   loop.WriteApprove,
		"send_webhook": loop.WriteApprove,
		"run_pipeline": loop.WriteRunNow,
		"restart_run":  loop.WriteApprove,
	}
	for name, want := range cases {
		if got := decide(agent.ToolDef{Name: name, Class: agent.ClassWrite}, nil); got != want {
			t.Errorf("%s: decision = %v, want %v", name, got, want)
		}
	}
}

func TestPool_ReserveAndRelease(t *testing.T) {
	p := NewPool(context.Background(), 1, func(context.Context, agent.Session, loop.Trigger) {})
	if !p.TryReserve() {
		t.Fatal("first reserve refused")
	}
	if p.TryReserve() {
		t.Fatal("a full pool granted a second slot")
	}
	p.Release()
	if !p.TryReserve() {
		t.Fatal("a released slot was not reusable")
	}
}

func TestPool_StartFreesSlot(t *testing.T) {
	done := make(chan struct{})
	p := NewPool(context.Background(), 1, func(context.Context, agent.Session, loop.Trigger) { close(done) })
	if !p.TryReserve() {
		t.Fatal("reserve refused")
	}
	p.Start(agent.Session{}, loop.TriggerStart)
	<-done
	deadline := time.Now().Add(5 * time.Second)
	for !p.TryReserve() {
		if time.Now().After(deadline) {
			t.Fatal("the slot of a finished run was not freed")
		}
		time.Sleep(time.Millisecond)
	}
}
