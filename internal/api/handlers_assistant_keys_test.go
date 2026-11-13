package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/assistantconfig"
)

// assistantKeyFixture gives the fixture two providers: smoke takes a key and
// local takes none. The fake operator records the key writes.
func assistantKeyFixture(t *testing.T) (*assistantFixture, *fakeAssistantOperator, *recordingActivitySink) {
	t.Helper()
	f, op, sink := assistantConfigFixture(t)
	f.rt.Live = assistantconfig.NewLiveWith(&assistantconfig.Snapshot{Config: provider.Config{
		Providers: []provider.Endpoint{
			{Name: "smoke", Kind: agent.ProviderOpenAICompatible, Models: []string{"m1"}, Trust: agent.TrustInternal, KeyRequired: true},
			{Name: "local", Kind: agent.ProviderOpenAICompatible, Models: []string{"m1"}, Trust: agent.TrustInternal, KeyRequired: false},
		},
	}})
	return f, op, sink
}

func keyPath(name string) string { return "/assistant/providers/" + name + "/key" }

func TestPutAssistantProviderKeyNeedsAdmin(t *testing.T) {
	f, op, _ := assistantKeyFixture(t)
	rec := f.do(t, f.viewer, http.MethodPut, keyPath("smoke"), map[string]string{"apiKey": "sk-test-key"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer PUT: %d, want 403", rec.Code)
	}
	if len(op.keys) != 0 {
		t.Fatalf("a refused write reached the operator: %v", op.keys)
	}
}

func TestPutAssistantProviderKeyRefusesUnknownProvider(t *testing.T) {
	f, op, _ := assistantKeyFixture(t)
	rec := f.do(t, f.admin, http.MethodPut, keyPath("nope"), map[string]string{"apiKey": "sk-test-key"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown provider: %d, want 400", rec.Code)
	}
	if len(op.keys) != 0 {
		t.Fatalf("an unknown provider reached the operator: %v", op.keys)
	}
}

func TestPutAssistantProviderKeyRefusesKeylessProvider(t *testing.T) {
	f, op, _ := assistantKeyFixture(t)
	rec := f.do(t, f.admin, http.MethodPut, keyPath("local"), map[string]string{"apiKey": "sk-test-key"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("provider with no key: %d, want 400", rec.Code)
	}
	if len(op.keys) != 0 {
		t.Fatalf("a keyless provider reached the operator: %v", op.keys)
	}
}

func TestPutAssistantProviderKeyRefusesEmptyAndLongKey(t *testing.T) {
	f, op, _ := assistantKeyFixture(t)
	for name, key := range map[string]string{
		"empty":      "",
		"whitespace": "   \t ",
		"too long":   strings.Repeat("k", assistantKeyMaxBytes+1),
	} {
		rec := f.do(t, f.admin, http.MethodPut, keyPath("smoke"), map[string]string{"apiKey": key})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s key: %d, want 400", name, rec.Code)
		}
	}
	if len(op.keys) != 0 {
		t.Fatalf("an invalid key reached the operator")
	}
}

func TestPutAssistantProviderKeySendsKeyToOperator(t *testing.T) {
	f, op, _ := assistantKeyFixture(t)
	rec := f.do(t, f.admin, http.MethodPut, keyPath("smoke"), map[string]string{"apiKey": "sk-test-key"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PUT: %d %s, want 204", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("204 reply holds a body: %q", rec.Body.String())
	}
	if got := string(op.keys["smoke"]); got != "sk-test-key" {
		t.Fatalf("operator received %q, want the key for smoke", got)
	}
}

func TestDeleteAssistantProviderKey(t *testing.T) {
	f, op, _ := assistantKeyFixture(t)
	rec := f.do(t, f.admin, http.MethodDelete, keyPath("smoke"), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s, want 204", rec.Code, rec.Body.String())
	}
	if len(op.cleared) != 1 || op.cleared[0] != "smoke" {
		t.Fatalf("operator clears %v, want [smoke]", op.cleared)
	}
	if rec := f.do(t, f.admin, http.MethodDelete, keyPath("local"), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("DELETE on a keyless provider: %d, want 400", rec.Code)
	}
}

// TestAssistantKeyActivityHoldsNoKey pins that the activity event names the
// provider and never holds the key.
func TestAssistantKeyActivityHoldsNoKey(t *testing.T) {
	const key = "sk-activity-probe-9f3a"
	f, _, sink := assistantKeyFixture(t)
	if rec := f.do(t, f.admin, http.MethodPut, keyPath("smoke"), map[string]string{"apiKey": key}); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT: %d", rec.Code)
	}
	if rec := f.do(t, f.admin, http.MethodDelete, keyPath("smoke"), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d", rec.Code)
	}
	if len(sink.events) != 2 || sink.events[0].Type != "assistant.key.set" || sink.events[1].Type != "assistant.key.cleared" {
		t.Fatalf("activity events %+v, want assistant.key.set then assistant.key.cleared", sink.events)
	}
	raw, err := json.Marshal(sink.events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), key) {
		t.Fatalf("an activity event holds the key: %s", raw)
	}
}
