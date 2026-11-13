package v1alpha1

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// validAssistantConfig returns one listed provider, the default provider and model,
// and every limit left to its default.
func validAssistantConfig() *AssistantConfig {
	return &AssistantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: AssistantConfigName},
		Spec: AssistantConfigSpec{
			Providers: []AssistantProvider{{
				Name:    "lab",
				Kind:    AssistantProviderKindOpenAICompatible,
				BaseURL: "https://llm.test/v1",
				Models:  []string{"model-a", "model-b"},
				Trust:   AssistantTrustExternal,
				CIDRs:   []string{"192.0.2.10/32"},
			}},
			DefaultProvider: "lab",
			DefaultModel:    "model-b",
		},
	}
}

func TestValidateAssistantConfig(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*AssistantConfig)
		wantErr string
	}{
		{name: "valid object", mutate: func(*AssistantConfig) {}},
		{name: "empty object with no provider", mutate: func(c *AssistantConfig) {
			c.Spec = AssistantConfigSpec{}
		}},
		{name: "other name refused", mutate: func(c *AssistantConfig) { c.Name = "other" }, wantErr: "must be"},
		{name: "unknown kind", mutate: func(c *AssistantConfig) { c.Spec.Providers[0].Kind = "gemini" }, wantErr: "kind"},
		{name: "openai-compatible without base URL", mutate: func(c *AssistantConfig) { c.Spec.Providers[0].BaseURL = "" }, wantErr: "baseURL is required"},
		{name: "anthropic without base URL accepted", mutate: func(c *AssistantConfig) {
			c.Spec.Providers[0].Kind = AssistantProviderKindAnthropic
			c.Spec.Providers[0].BaseURL = ""
		}},
		{name: "base URL without scheme", mutate: func(c *AssistantConfig) { c.Spec.Providers[0].BaseURL = "llm.test" }, wantErr: "http"},
		{name: "duplicate provider name", mutate: func(c *AssistantConfig) {
			c.Spec.Providers = append(c.Spec.Providers, c.Spec.Providers[0])
		}, wantErr: "not unique"},
		{name: "invalid provider name", mutate: func(c *AssistantConfig) { c.Spec.Providers[0].Name = "Lab_1" }, wantErr: "DNS label"},
		{name: "no model", mutate: func(c *AssistantConfig) { c.Spec.Providers[0].Models = nil }, wantErr: "models holds 0"},
		{name: "empty model name", mutate: func(c *AssistantConfig) { c.Spec.Providers[0].Models = []string{""} }, wantErr: "models[0]"},
		{name: "unknown trust", mutate: func(c *AssistantConfig) { c.Spec.Providers[0].Trust = "dmz" }, wantErr: "trust"},
		{name: "unknown auth", mutate: func(c *AssistantConfig) { c.Spec.Providers[0].Auth = "token" }, wantErr: "auth"},
		{name: "none auth accepted", mutate: func(c *AssistantConfig) { c.Spec.Providers[0].Auth = AssistantAuthNone }},
		{name: "invalid CIDR", mutate: func(c *AssistantConfig) { c.Spec.Providers[0].CIDRs = []string{"192.0.2.300/24"} }, wantErr: "is not a CIDR"},
		{name: "padded CIDR refused", mutate: func(c *AssistantConfig) { c.Spec.Providers[0].CIDRs = []string{" 192.0.2.10/32"} }, wantErr: "is not a CIDR"},
		{name: "port above range", mutate: func(c *AssistantConfig) { c.Spec.Providers[0].Port = 70000 }, wantErr: "out of range"},
		{name: "too many providers", mutate: func(c *AssistantConfig) {
			c.Spec.Providers = nil
			for i := 0; i < 17; i++ {
				p := validAssistantConfig().Spec.Providers[0]
				p.Name = "p" + strings.Repeat("x", i%3) + string(rune('a'+i))
				c.Spec.Providers = append(c.Spec.Providers, p)
			}
			c.Spec.DefaultProvider = "pa"
			c.Spec.DefaultModel = ""
		}, wantErr: "limit is 16"},
		{name: "default provider missing while providers listed", mutate: func(c *AssistantConfig) { c.Spec.DefaultProvider = "" }, wantErr: "defaultProvider is required"},
		{name: "default provider names no listed provider", mutate: func(c *AssistantConfig) { c.Spec.DefaultProvider = "ghost" }, wantErr: "names no listed provider"},
		{name: "default provider set with no provider", mutate: func(c *AssistantConfig) {
			c.Spec.Providers = nil
			c.Spec.DefaultModel = ""
		}, wantErr: "names no listed provider"},
		{name: "default model not listed", mutate: func(c *AssistantConfig) { c.Spec.DefaultModel = "model-z" }, wantErr: "not a model of provider"},
		{name: "default model with no default provider and no providers", mutate: func(c *AssistantConfig) {
			c.Spec = AssistantConfigSpec{DefaultModel: "model-a"}
		}, wantErr: "no listed provider"},
		{name: "external sharing level unknown", mutate: func(c *AssistantConfig) { c.Spec.ExternalDataSharing = "all" }, wantErr: "externalDataSharing"},
		{name: "negative budget", mutate: func(c *AssistantConfig) { c.Spec.Budgets.Session = -1 }, wantErr: "budgets.session"},
		{name: "zero budget means the default", mutate: func(c *AssistantConfig) { c.Spec.Budgets = AssistantBudgets{} }},
		{name: "approval expiry in seconds refused", mutate: func(c *AssistantConfig) { c.Spec.ApprovalExpiry = "30s" }, wantErr: "approvalExpiry"},
		{name: "approval expiry in hours accepted", mutate: func(c *AssistantConfig) { c.Spec.ApprovalExpiry = "48h" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validAssistantConfig()
			tt.mutate(c)
			err := ValidateAssistantConfig(c)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("want accept, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidateAssistantConfigNil(t *testing.T) {
	if err := ValidateAssistantConfig(nil); err == nil {
		t.Fatal("nil object accepted")
	}
}

func TestAssistantProviderEffectivePort(t *testing.T) {
	tests := []struct {
		name string
		p    AssistantProvider
		want int32
	}{
		{name: "explicit port wins", p: AssistantProvider{BaseURL: "https://llm.test:8443", Port: 9000}, want: 9000},
		{name: "port from the URL", p: AssistantProvider{BaseURL: "http://192.0.2.5:11434/v1"}, want: 11434},
		{name: "https default", p: AssistantProvider{BaseURL: "https://llm.test/v1"}, want: 443},
		{name: "http default", p: AssistantProvider{BaseURL: "http://llm.test/v1"}, want: 80},
		{name: "no URL falls back to 443", p: AssistantProvider{}, want: 443},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.EffectivePort(); got != tt.want {
				t.Fatalf("EffectivePort() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDefaultAssistantConfigSpecHasNoProvider(t *testing.T) {
	spec := DefaultAssistantConfigSpec()
	if len(spec.Providers) != 0 || spec.DefaultProvider != "" {
		t.Fatalf("default spec names providers: %+v", spec)
	}
	if spec.Budgets.Session != AssistantDefaultSessionBudget || spec.ApprovalExpiry != AssistantDefaultApprovalExpiry {
		t.Fatalf("default spec has the wrong limits: %+v", spec)
	}
}
