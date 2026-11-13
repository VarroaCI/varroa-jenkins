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

// fakeProvider is a Provider that does nothing; the registry tests need only the value.
type fakeProvider struct{}

func (fakeProvider) Stream(_ context.Context, _ Key, _ Request) (<-chan Delta, error) {
	return nil, nil
}

// TestLoadKeyReadsFileEachCall asserts that LoadKey reads the file again on
// every call, so a rotated key takes effect without a restart.
func TestLoadKeyReadsFileNamedForProvider(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := Config{Providers: []Endpoint{{Name: "smoke", Kind: agent.ProviderOpenAICompatible, BaseURL: "http://x",
		Models: []string{"m"}, Trust: agent.TrustExternal, Default: true, KeyRequired: true}}}
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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAvailable asserts that a provider with a secret is available only when
// its key file is non-empty.
func TestAvailable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := Config{Providers: []Endpoint{
		{Name: "keyed", Kind: agent.ProviderAnthropic, Models: []string{"m"}, Trust: agent.TrustExternal, Default: true, KeyRequired: true},
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
