package provider

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// Endpoint describes one configured provider. The secret itself is never in
// the config. KeyRequired says that the provider reads a key file.
type Endpoint struct {
	Name        string              `json:"name"`
	Kind        agent.ProviderKind  `json:"kind"`
	BaseURL     string              `json:"baseURL"`
	Models      []string            `json:"models"`
	Trust       agent.ProviderTrust `json:"trust"`
	Default     bool                `json:"default"`
	KeyRequired bool                `json:"keyRequired"`
}

// DefaultModel returns the first model of the endpoint.
func (e Endpoint) DefaultModel() string {
	if len(e.Models) == 0 {
		return ""
	}
	return e.Models[0]
}

// Budgets are the spend limits of the assistant. Each limit is above zero.
type Budgets struct {
	Session   int64 `json:"session"`
	UserDay   int64 `json:"userDay"`
	TenantDay int64 `json:"tenantDay"`
	Steps     int   `json:"steps"`
}

// Config is the assistant configuration that the runtime reads. It is built
// from an AssistantConfig object and checked by Validate.
type Config struct {
	Providers           []Endpoint        `json:"providers"`
	DefaultProvider     string            `json:"defaultProvider"`
	DefaultModel        string            `json:"defaultModel"`
	ExternalDataSharing agent.DataSharing `json:"externalDataSharing"`
	Budgets             Budgets           `json:"budgets"`
	ApprovalExpiry      time.Duration     `json:"approvalExpiry"`
}

var endpointNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Validate checks a configuration. Each rule has its own error that names the
// field. An empty provider list is valid, and then the runtime has no
// providers.
func Validate(c *Config) error {
	seen := map[string]bool{}
	defaults := 0
	for i, e := range c.Providers {
		if !endpointNameRE.MatchString(e.Name) {
			return fmt.Errorf("assistant config: providers[%d].name %q is not a valid name", i, e.Name)
		}
		if seen[e.Name] {
			return fmt.Errorf("assistant config: providers[%d].name %q is not unique", i, e.Name)
		}
		seen[e.Name] = true
		if !containsKind(e.Kind) {
			return fmt.Errorf("assistant config: providers[%d].kind %q is not valid", i, e.Kind)
		}
		if !containsTrust(e.Trust) {
			return fmt.Errorf("assistant config: providers[%d].trust %q is not valid", i, e.Trust)
		}
		if e.Kind == agent.ProviderOpenAICompatible && e.BaseURL == "" {
			return fmt.Errorf("assistant config: providers[%d].baseURL is required for openai-compatible", i)
		}
		if e.BaseURL != "" && !strings.HasPrefix(e.BaseURL, "http://") && !strings.HasPrefix(e.BaseURL, "https://") {
			return fmt.Errorf("assistant config: providers[%d].baseURL must start with http:// or https://", i)
		}
		if len(e.Models) == 0 {
			return fmt.Errorf("assistant config: providers[%d].models is empty", i)
		}
		if e.Default {
			defaults++
		}
	}
	if len(c.Providers) > 0 && defaults != 1 {
		return fmt.Errorf("assistant config: exactly one provider must be the default, found %d", defaults)
	}
	if len(c.Providers) == 0 && c.DefaultProvider != "" {
		return fmt.Errorf("assistant config: defaultProvider %q names no provider", c.DefaultProvider)
	}
	if c.DefaultModel != "" && !listsModel(c, c.DefaultProvider, c.DefaultModel) {
		return fmt.Errorf("assistant config: defaultModel %q is not listed for provider %q", c.DefaultModel, c.DefaultProvider)
	}
	if !c.ExternalDataSharing.Valid() {
		return fmt.Errorf("assistant config: externalDataSharing %q is not valid", c.ExternalDataSharing)
	}
	if c.Budgets.Session <= 0 {
		return fmt.Errorf("assistant config: budgets.session must be above zero")
	}
	if c.Budgets.UserDay <= 0 {
		return fmt.Errorf("assistant config: budgets.userDay must be above zero")
	}
	if c.Budgets.TenantDay <= 0 {
		return fmt.Errorf("assistant config: budgets.tenantDay must be above zero")
	}
	if c.Budgets.Steps <= 0 {
		return fmt.Errorf("assistant config: budgets.steps must be above zero")
	}
	if c.ApprovalExpiry <= 0 {
		return fmt.Errorf("assistant config: approvalExpiry must be above zero")
	}
	return nil
}

// ModelFor returns the model a session uses on a provider. The default
// provider uses the configured default model. Any other provider uses its
// first listed model.
func (c Config) ModelFor(name string) string {
	for _, e := range c.Providers {
		if e.Name != name {
			continue
		}
		if e.Name == c.DefaultProvider && c.DefaultModel != "" {
			return c.DefaultModel
		}
		return e.DefaultModel()
	}
	return ""
}

func listsModel(c *Config, name, model string) bool {
	for _, e := range c.Providers {
		if e.Name != name {
			continue
		}
		for _, m := range e.Models {
			if m == model {
				return true
			}
		}
	}
	return false
}

func containsKind(k agent.ProviderKind) bool {
	for _, v := range agent.ProviderKinds() {
		if v == k {
			return true
		}
	}
	return false
}

func containsTrust(t agent.ProviderTrust) bool {
	for _, v := range agent.ProviderTrusts() {
		if v == t {
			return true
		}
	}
	return false
}
