package provider

import (
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// validCfg returns a configuration that passes every rule of Validate.
func validCfg() Config {
	return Config{
		Providers: []Endpoint{{
			Name: "smoke", Kind: agent.ProviderOpenAICompatible, BaseURL: "http://127.0.0.1:8082/v1",
			Models: []string{"varroa-smoke", "varroa-large"}, Trust: agent.TrustInternal, Default: true, KeyRequired: false,
		}},
		DefaultProvider:     "smoke",
		DefaultModel:        "varroa-large",
		ExternalDataSharing: agent.DataMetadata,
		Budgets:             Budgets{Session: 100000, UserDay: 1000000, TenantDay: 2000000, Steps: 32},
		ApprovalExpiry:      30 * time.Minute,
	}
}

// TestValidateRules checks each rule of Validate with one bad configuration,
// and accepts the valid one.
func TestValidateRules(t *testing.T) {
	t.Parallel()
	good := validCfg()
	if err := Validate(&good); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	bad := map[string]func(c *Config){
		"bad name":        func(c *Config) { c.Providers[0].Name = "Smoke!" },
		"duplicate name":  func(c *Config) { c.Providers = append(c.Providers, c.Providers[0]) },
		"bad kind":        func(c *Config) { c.Providers[0].Kind = "other" },
		"bad trust":       func(c *Config) { c.Providers[0].Trust = "maybe" },
		"no base url":     func(c *Config) { c.Providers[0].BaseURL = "" },
		"base url scheme": func(c *Config) { c.Providers[0].BaseURL = "localhost:8080" },
		"no models":       func(c *Config) { c.Providers[0].Models = nil },
		"no default":      func(c *Config) { c.Providers[0].Default = false },
		"two defaults": func(c *Config) {
			c.Providers = append(c.Providers, Endpoint{Name: "b", Kind: agent.ProviderAnthropic, Models: []string{"m"}, Trust: agent.TrustExternal, Default: true})
		},
		"unlisted model":     func(c *Config) { c.DefaultModel = "not-listed" },
		"default names none": func(c *Config) { c.Providers = nil },
		"bad external":       func(c *Config) { c.ExternalDataSharing = "all" },
		"zero budget":        func(c *Config) { c.Budgets.Session = 0 },
		"zero expiry":        func(c *Config) { c.ApprovalExpiry = 0 },
		"zero steps":         func(c *Config) { c.Budgets.Steps = 0 },
	}
	for name, mutate := range bad {
		c := validCfg()
		mutate(&c)
		if err := Validate(&c); err == nil {
			t.Errorf("%s: Validate accepted the configuration", name)
		}
	}
	empty := Config{ExternalDataSharing: agent.DataNone,
		Budgets: Budgets{Session: 1, UserDay: 1, TenantDay: 1, Steps: 1}, ApprovalExpiry: 2 * time.Hour}
	if err := Validate(&empty); err != nil {
		t.Fatalf("empty provider list refused: %v", err)
	}
}

// TestModelFor asserts that the default provider takes the default model and
// any other provider takes its first listed model.
func TestModelFor(t *testing.T) {
	t.Parallel()
	c := validCfg()
	c.Providers = append(c.Providers, Endpoint{Name: "other", Kind: agent.ProviderAnthropic,
		Models: []string{"o1", "o2"}, Trust: agent.TrustExternal})
	if got := c.ModelFor("smoke"); got != "varroa-large" {
		t.Errorf("default provider model = %q, want varroa-large", got)
	}
	if got := c.ModelFor("other"); got != "o1" {
		t.Errorf("other provider model = %q, want o1", got)
	}
	if got := c.ModelFor("missing"); got != "" {
		t.Errorf("unknown provider model = %q, want empty", got)
	}
}
