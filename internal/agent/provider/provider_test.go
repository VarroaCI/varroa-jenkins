package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// canary is a fake key. It must never appear in any output of a Key.
const canary = "canary-provider-key-7f3a"

// TestKeyRedactedEverywhere asserts that every formatter, logger, and encoder
// prints the redacted text, and that SetHeader still sends the bytes.
func TestKeyRedactedEverywhere(t *testing.T) {
	t.Parallel()
	k := NewKey([]byte(canary))

	outputs := []string{
		fmt.Sprintf("%v", k), fmt.Sprintf("%+v", k), fmt.Sprintf("%#v", k),
		fmt.Sprintf("%s", k), fmt.Sprintf("%q", k), fmt.Sprintf("%x", k),
		k.String(),
	}
	var logBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&logBuf, nil)).Info("probe", "key", k)
	outputs = append(outputs, logBuf.String())
	if b, err := json.Marshal(k); err == nil {
		outputs = append(outputs, string(b))
	}
	if b, err := k.MarshalText(); err == nil {
		outputs = append(outputs, string(b))
	}
	for _, out := range outputs {
		if strings.Contains(out, canary) {
			t.Fatalf("key leaked: %s", out)
		}
	}

	h := http.Header{}
	k.SetHeader(h, "x-api-key", "")
	if h.Get("x-api-key") != canary {
		t.Fatalf("SetHeader sent %q, want the key", h.Get("x-api-key"))
	}
	empty := http.Header{}
	Key{}.SetHeader(empty, "Authorization", "Bearer ")
	if _, ok := empty["Authorization"]; ok {
		t.Fatal("an empty key set a header")
	}
}

// validConfig returns a config document that passes every rule.
func validConfig() string {
	return `{
	  "providers": [
	    {"name":"smoke","kind":"openai-compatible","baseURL":"http://127.0.0.1:8082/v1",
	     "models":["varroa-smoke"],"trust":"internal","default":true,"hasSecret":false}
	  ],
	  "externalDataSharing":"metadata",
	  "budgets":{"session":100000,"userDay":500000,"tenantDay":2000000,"steps":32},
	  "approvalExpiry":"30m",
	  "egress":{"networkPolicy":true,"cidrs":[],"ports":[443]}
	}`
}

// TestDecodeRules checks each rule of the configuration decoder with one bad
// document, and accepts the valid one.
func TestDecodeRules(t *testing.T) {
	t.Parallel()
	if _, err := Decode([]byte(validConfig())); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	bad := map[string]string{
		"not json":     `{`,
		"bad name":     strings.Replace(validConfig(), `"name":"smoke"`, `"name":"Smoke!"`, 1),
		"bad kind":     strings.Replace(validConfig(), `"kind":"openai-compatible"`, `"kind":"other"`, 1),
		"bad trust":    strings.Replace(validConfig(), `"trust":"internal"`, `"trust":"maybe"`, 1),
		"no base url":  strings.Replace(validConfig(), `"baseURL":"http://127.0.0.1:8082/v1",`, ``, 1),
		"bad base url": strings.Replace(validConfig(), `http://127.0.0.1:8082/v1`, `ftp://x`, 1),
		"no models":    strings.Replace(validConfig(), `["varroa-smoke"]`, `[]`, 1),
		"no default":   strings.Replace(validConfig(), `"default":true`, `"default":false`, 1),
		"bad external": strings.Replace(validConfig(), `"externalDataSharing":"metadata"`, `"externalDataSharing":"all"`, 1),
		"zero budget":  strings.Replace(validConfig(), `"session":100000`, `"session":0`, 1),
		"bad expiry":   strings.Replace(validConfig(), `"approvalExpiry":"30m"`, `"approvalExpiry":"30s"`, 1),
		"zero steps":   strings.Replace(validConfig(), `"steps":32`, `"steps":0`, 1),
	}
	for name, doc := range bad {
		if _, err := Decode([]byte(doc)); err == nil {
			t.Fatalf("%s: Decode accepted the document", name)
		}
	}

	empty := `{"providers":[],"externalDataSharing":"none","budgets":{"session":1,"userDay":1,"tenantDay":1,"steps":1},"approvalExpiry":"2h"}`
	c, err := Decode([]byte(empty))
	if err != nil {
		t.Fatalf("empty provider list refused: %v", err)
	}
	if c.ApprovalExpiry.Hours() != 2 {
		t.Fatalf("approval expiry = %s, want 2h", c.ApprovalExpiry)
	}
	if _, err := Decode([]byte(strings.Replace(validConfig(), `"egress"`, `"futureKey":1,"egress"`, 1))); err != nil {
		t.Fatalf("unknown key refused: %v", err)
	}
}

// fakeProvider is a Provider that does nothing; the registry tests need only the value.
type fakeProvider struct{}

func (fakeProvider) Stream(_ context.Context, _ Key, _ Request) (<-chan Delta, error) {
	return nil, nil
}

// TestLoadKeyReadsFileEachCall asserts that LoadKey reads the file again on
// every call, so a rotated key takes effect without a restart.
func TestLoadKeyReadsFileEachCall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := Config{Providers: []Endpoint{{Name: "smoke", Kind: agent.ProviderOpenAICompatible, BaseURL: "http://x",
		Models: []string{"m"}, Trust: agent.TrustExternal, Default: true, HasSecret: true}}}
	r, err := NewRegistry(cfg, map[agent.ProviderKind]Factory{
		agent.ProviderOpenAICompatible: func(Endpoint) (Provider, error) { return fakeProvider{}, nil },
	}, dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	writeKey(t, dir, "smoke", "first\n")
	k, err := r.LoadKey("smoke")
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	if got := mustBytes(t, k); got != "first" {
		t.Fatalf("key = %q, want first (trimmed)", got)
	}
	writeKey(t, dir, "smoke", "second")
	k, _ = r.LoadKey("smoke")
	if got := mustBytes(t, k); got != "second" {
		t.Fatalf("rotated key = %q, want second", got)
	}
	if _, err := r.LoadKey("missing"); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("unknown provider = %v, want ErrKeyUnavailable", err)
	}
}

// mustBytes reads the key through its only reader, SetHeader.
func mustBytes(t *testing.T, k Key) string {
	t.Helper()
	h := http.Header{}
	k.SetHeader(h, "X", "")
	return h.Get("X")
}

func writeKey(t *testing.T, dir, name, value string) {
	t.Helper()
	sub := filepath.Join(dir, name)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "apiKey"), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAvailable asserts that a provider with a secret is available only when
// its key file is non-empty.
func TestAvailable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := Config{Providers: []Endpoint{
		{Name: "keyed", Kind: agent.ProviderAnthropic, Models: []string{"m"}, Trust: agent.TrustExternal, Default: true, HasSecret: true},
		{Name: "open", Kind: agent.ProviderOpenAICompatible, BaseURL: "http://x", Models: []string{"m"}, Trust: agent.TrustInternal},
	}}
	r, err := NewRegistry(cfg, map[agent.ProviderKind]Factory{
		agent.ProviderAnthropic:        func(Endpoint) (Provider, error) { return fakeProvider{}, nil },
		agent.ProviderOpenAICompatible: func(Endpoint) (Provider, error) { return fakeProvider{}, nil },
	}, dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if r.Available("keyed") {
		t.Fatal("keyed provider without a key file is available")
	}
	writeKey(t, dir, "keyed", "   ")
	if r.Available("keyed") {
		t.Fatal("keyed provider with a blank key file is available")
	}
	writeKey(t, dir, "keyed", canary)
	if !r.Available("keyed") {
		t.Fatal("keyed provider with a key is unavailable")
	}
	if !r.Available("open") {
		t.Fatal("keyless provider is unavailable")
	}
	if !r.HasModel("open", "m") || r.HasModel("open", "other") {
		t.Fatal("HasModel answered wrongly")
	}
	if e, ok := r.Default(); !ok || e.Endpoint.Name != "keyed" {
		t.Fatal("Default is not the keyed provider")
	}
}

// TestNewRegistryUnknownFactory asserts that a kind with no factory is refused.
func TestNewRegistryUnknownFactory(t *testing.T) {
	t.Parallel()
	cfg := Config{Providers: []Endpoint{{Name: "x", Kind: agent.ProviderAnthropic, Models: []string{"m"}, Trust: agent.TrustExternal, Default: true}}}
	if _, err := NewRegistry(cfg, map[agent.ProviderKind]Factory{}, t.TempDir()); err == nil {
		t.Fatal("registry built without a factory for the kind")
	}
}

// TestCollectJoinsToolCallsAndReportsErrors covers the stream joiner.
func TestCollectJoinsToolCallsAndReportsErrors(t *testing.T) {
	t.Parallel()
	ch := make(chan Delta, 8)
	ch <- Delta{Text: "hi "}
	ch <- Delta{ToolCall: &ToolCallDelta{Index: 1, ID: "b", Name: "second", ArgumentsFragment: `{"x"`}}
	ch <- Delta{ToolCall: &ToolCallDelta{Index: 0, ID: "a", Name: "first", ArgumentsFragment: `{}`}}
	ch <- Delta{ToolCall: &ToolCallDelta{Index: 1, ArgumentsFragment: `:1}`}}
	ch <- Delta{Text: "there", Usage: &Usage{PromptTokens: 3, CompletionTokens: 4}, Stop: StopToolUse}
	close(ch)
	resp, err := Collect(ch)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if resp.Text != "hi there" || resp.Stop != StopToolUse || resp.Usage.PromptTokens != 3 {
		t.Fatalf("response = %+v", resp)
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].ID != "b" || string(resp.ToolCalls[0].Arguments) != `{"x":1}` {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}

	failed := make(chan Delta, 1)
	failed <- Delta{Stop: StopError, Err: ErrIdle}
	close(failed)
	if _, err := Collect(failed); !errors.Is(err, ErrIdle) {
		t.Fatalf("error stream = %v, want ErrIdle", err)
	}

	open := make(chan Delta, 1)
	open <- Delta{Text: "cut"}
	close(open)
	if _, err := Collect(open); !errors.Is(err, ErrBrokenStream) {
		t.Fatalf("stream without a stop = %v, want ErrBrokenStream", err)
	}
}
