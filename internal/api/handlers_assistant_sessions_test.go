package api

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/assistantconfig"
)

// newChooseSnapshot builds a snapshot with two chart providers. The provider
// alpha is the registry default. defProv and defModel are the defaults of the
// object.
func newChooseSnapshot(t *testing.T, defProv, defModel string) *assistantconfig.Snapshot {
	t.Helper()
	cfg := provider.Config{
		Providers: []provider.Endpoint{
			{
				Name: "alpha", Kind: agent.ProviderOpenAICompatible, BaseURL: "http://127.0.0.1:9/v1",
				Models: []string{"a1"}, Trust: agent.TrustInternal, Default: true,
			},
			{
				Name: "beta", Kind: agent.ProviderOpenAICompatible, BaseURL: "http://127.0.0.1:9/v1",
				Models: []string{"b1", "b2"}, Trust: agent.TrustInternal,
			},
		},
		DefaultProvider:     defProv,
		DefaultModel:        defModel,
		ExternalDataSharing: agent.DataLogs,
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
	return &assistantconfig.Snapshot{Config: cfg, Registry: reg}
}

// TestChooseProviderUsesConfigDefault proves that a new session with no saved
// provider takes the default provider and default model of the snapshot.
func TestChooseProviderUsesConfigDefault(t *testing.T) {
	snap := newChooseSnapshot(t, "beta", "b2")
	rt := &AssistantRuntime{}
	prov, model, entry, err := rt.chooseProvider(snap, "", "", agent.Settings{})
	if err != nil {
		t.Fatalf("chooseProvider: %v", err)
	}
	if prov != "beta" || model != "b2" || entry == nil {
		t.Fatalf("chooseProvider = (%q, %q), want (beta, b2)", prov, model)
	}
}

func TestChooseProvider_FallbackOrder(t *testing.T) {
	cases := []struct {
		name        string
		reqProvider string
		reqModel    string
		user        agent.Settings
		snapProv    string
		snapModel   string
		wantProv    string
		wantModel   string
		wantErr     bool
	}{
		{
			name:      "stale user provider uses the snapshot default",
			user:      agent.Settings{Provider: "gone", Model: "x"},
			snapProv:  "beta",
			snapModel: "b2",
			wantProv:  "beta",
			wantModel: "b2",
		},
		{
			name:      "stale user and no snapshot default use the registry default",
			user:      agent.Settings{Provider: "gone"},
			snapProv:  "gone2",
			wantProv:  "alpha",
			wantModel: "a1",
		},
		{
			name:      "known user provider wins over the snapshot default",
			user:      agent.Settings{Provider: "beta", Model: "b1"},
			snapProv:  "alpha",
			wantProv:  "beta",
			wantModel: "b1",
		},
		{
			name:      "empty user uses the snapshot default model",
			snapProv:  "beta",
			snapModel: "b2",
			wantProv:  "beta",
			wantModel: "b2",
		},
		{
			name:      "a non-default provider takes its first model",
			user:      agent.Settings{Provider: "beta"},
			snapProv:  "alpha",
			wantProv:  "beta",
			wantModel: "b1",
		},
		{
			name:        "unknown request provider is refused",
			reqProvider: "gone",
			wantErr:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := newChooseSnapshot(t, tc.snapProv, tc.snapModel)
			rt := &AssistantRuntime{}
			prov, model, entry, err := rt.chooseProvider(snap, tc.reqProvider, tc.reqModel, tc.user)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("chooseProvider() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("chooseProvider() error = %v", err)
			}
			if prov != tc.wantProv || model != tc.wantModel {
				t.Fatalf("chooseProvider() = (%q, %q), want (%q, %q)", prov, model, tc.wantProv, tc.wantModel)
			}
			if entry == nil || entry.Endpoint.Name != tc.wantProv {
				t.Fatalf("chooseProvider() entry = %+v, want endpoint %q", entry, tc.wantProv)
			}
		})
	}
}
