package api

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
)

// newChooseProviderRuntime wires a runtime with two chart providers. The
// provider alpha is the registry default.
func newChooseProviderRuntime(t *testing.T) *AssistantRuntime {
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
	return &AssistantRuntime{Registry: reg, Config: cfg}
}

func TestChooseProvider_FallbackOrder(t *testing.T) {
	cases := []struct {
		name        string
		reqProvider string
		reqModel    string
		user        agent.Settings
		global      agent.Settings
		wantProv    string
		wantModel   string
		wantErr     bool
	}{
		{
			name:      "stale user provider uses the global default",
			user:      agent.Settings{Provider: "gone", Model: "x"},
			global:    agent.Settings{DefaultProvider: "beta", DefaultModel: "b2"},
			wantProv:  "beta",
			wantModel: "b2",
		},
		{
			name:      "stale user and stale global use the registry default",
			user:      agent.Settings{Provider: "gone"},
			global:    agent.Settings{DefaultProvider: "gone2"},
			wantProv:  "alpha",
			wantModel: "a1",
		},
		{
			name:      "known user provider wins over the global default",
			user:      agent.Settings{Provider: "beta", Model: "b1"},
			global:    agent.Settings{DefaultProvider: "alpha"},
			wantProv:  "beta",
			wantModel: "b1",
		},
		{
			name:      "empty user uses the global default",
			global:    agent.Settings{DefaultProvider: "beta"},
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
			rt := newChooseProviderRuntime(t)
			prov, model, entry, err := rt.chooseProvider(tc.reqProvider, tc.reqModel, tc.user, tc.global)
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
