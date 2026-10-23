package provider

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// Endpoint describes one configured provider. The secret itself is never in
// the config. HasSecret says that a key file exists for the provider.
type Endpoint struct {
	Name      string              `json:"name"`
	Kind      agent.ProviderKind  `json:"kind"`
	BaseURL   string              `json:"baseURL"`
	Models    []string            `json:"models"`
	Trust     agent.ProviderTrust `json:"trust"`
	Default   bool                `json:"default"`
	HasSecret bool                `json:"hasSecret"`
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

// Egress describes the network path of the assistant.
type Egress struct {
	NetworkPolicy bool     `json:"networkPolicy"`
	CIDRs         []string `json:"cidrs"`
	Ports         []int    `json:"ports"`
}

// Config is the decoded assistant configuration.
type Config struct {
	Providers           []Endpoint        `json:"providers"`
	ExternalDataSharing agent.DataSharing `json:"externalDataSharing"`
	Budgets             Budgets           `json:"budgets"`
	ApprovalExpiryText  string            `json:"approvalExpiry"`
	ApprovalExpiry      time.Duration     `json:"-"`
	Egress              Egress            `json:"egress"`
}

var (
	endpointNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	expiryRE       = regexp.MustCompile(`^[1-9][0-9]*(m|h)$`)
)

// Decode reads and checks the assistant configuration. Each rule has its own
// error that names the field. Unknown JSON keys are accepted.
func Decode(raw []byte) (Config, error) {
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("assistant config: %w", err)
	}
	seen := map[string]bool{}
	defaults := 0
	for i, e := range c.Providers {
		if !endpointNameRE.MatchString(e.Name) {
			return Config{}, fmt.Errorf("assistant config: providers[%d].name %q is not a valid name", i, e.Name)
		}
		if seen[e.Name] {
			return Config{}, fmt.Errorf("assistant config: providers[%d].name %q is not unique", i, e.Name)
		}
		seen[e.Name] = true
		if !containsKind(e.Kind) {
			return Config{}, fmt.Errorf("assistant config: providers[%d].kind %q is not valid", i, e.Kind)
		}
		if !containsTrust(e.Trust) {
			return Config{}, fmt.Errorf("assistant config: providers[%d].trust %q is not valid", i, e.Trust)
		}
		if e.Kind == agent.ProviderOpenAICompatible && e.BaseURL == "" {
			return Config{}, fmt.Errorf("assistant config: providers[%d].baseURL is required for openai-compatible", i)
		}
		if e.BaseURL != "" && !strings.HasPrefix(e.BaseURL, "http://") && !strings.HasPrefix(e.BaseURL, "https://") {
			return Config{}, fmt.Errorf("assistant config: providers[%d].baseURL must start with http:// or https://", i)
		}
		if len(e.Models) == 0 {
			return Config{}, fmt.Errorf("assistant config: providers[%d].models is empty", i)
		}
		if e.Default {
			defaults++
		}
	}
	if len(c.Providers) > 0 && defaults != 1 {
		return Config{}, fmt.Errorf("assistant config: exactly one provider must be default, found %d", defaults)
	}
	if len(c.Providers) > 0 && !c.ExternalDataSharing.Valid() {
		return Config{}, fmt.Errorf("assistant config: externalDataSharing %q is not valid", c.ExternalDataSharing)
	}
	if c.Budgets.Session <= 0 {
		return Config{}, fmt.Errorf("assistant config: budgets.session must be above zero")
	}
	if c.Budgets.UserDay <= 0 {
		return Config{}, fmt.Errorf("assistant config: budgets.userDay must be above zero")
	}
	if c.Budgets.TenantDay <= 0 {
		return Config{}, fmt.Errorf("assistant config: budgets.tenantDay must be above zero")
	}
	if c.Budgets.Steps <= 0 {
		return Config{}, fmt.Errorf("assistant config: budgets.steps must be above zero")
	}
	if !expiryRE.MatchString(c.ApprovalExpiryText) {
		return Config{}, fmt.Errorf("assistant config: approvalExpiry %q must match a number with m or h", c.ApprovalExpiryText)
	}
	d, err := time.ParseDuration(c.ApprovalExpiryText)
	if err != nil {
		return Config{}, fmt.Errorf("assistant config: approvalExpiry %q: %w", c.ApprovalExpiryText, err)
	}
	c.ApprovalExpiry = d
	return c, nil
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
