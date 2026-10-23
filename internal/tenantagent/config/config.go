// Package config reads the environment of the varroa-agent process and decides
// whether the model provider a TenantAgent names is ready.
package config

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
)

// DefaultKeyPath is the signing key file that the chart mounts.
const DefaultKeyPath = "/etc/varroa/tenant-agent-key/key"

// Config is the decoded environment of varroa-agent.
type Config struct {
	// Assistant is the provider configuration the BFF also reads. A zero value
	// means no providers are configured.
	Assistant provider.Config
	// AssistantErr holds the decode error of VARROA_ASSISTANT_CONFIG. A
	// process with a decode error reports AssistantDisabled on every TenantAgent.
	AssistantErr error
	// DSN is the record-store connection string of the varroa_client role.
	DSN string
	// BFFURL is the base URL of the BFF that serves the MCP endpoint.
	BFFURL string
	// Cluster is the core cluster name.
	Cluster string
	// PodName and PodNamespace identify this replica.
	PodName      string
	PodNamespace string
	// KeyPath is the file that holds the signing key.
	KeyPath string
}

// FromEnv reads Config from getenv. The DSN, the BFF URL, and the cluster name
// are required. A malformed VARROA_ASSISTANT_CONFIG is kept in AssistantErr so
// the status ticker can report it on each TenantAgent.
func FromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{
		DSN:          getenv("VARROA_RECORDSTORE_DSN"),
		BFFURL:       strings.TrimSuffix(getenv("VARROA_BFF_URL"), "/"),
		Cluster:      getenv("VARROA_CLUSTER"),
		PodName:      getenv("POD_NAME"),
		PodNamespace: getenv("POD_NAMESPACE"),
		KeyPath:      getenv("VARROA_TENANT_AGENT_KEY_FILE"),
	}
	if cfg.KeyPath == "" {
		cfg.KeyPath = DefaultKeyPath
	}
	for name, v := range map[string]string{
		"VARROA_RECORDSTORE_DSN": cfg.DSN,
		"VARROA_BFF_URL":         cfg.BFFURL,
		"VARROA_CLUSTER":         cfg.Cluster,
	} {
		if v == "" {
			return Config{}, fmt.Errorf("%s is required", name)
		}
	}
	if raw := getenv(provider.ConfigEnv); raw != "" {
		assistant, err := provider.Decode([]byte(raw))
		if err != nil {
			cfg.AssistantErr = err
		} else {
			cfg.Assistant = assistant
		}
	}
	return cfg, nil
}

// ProviderReady decides the InstructionsResolved-style provider condition for
// one TenantAgent. It returns True with reason Ready when reg has the provider
// and model, and the key file is present for a provider that needs one.
func ProviderReady(cfg Config, reg *provider.Registry, ta *v1alpha1.TenantAgent) (metav1.ConditionStatus, string, string) {
	if cfg.AssistantErr != nil || reg == nil {
		return metav1.ConditionFalse, v1alpha1.TenantAgentReasonAssistantDisabled,
			"the assistant configuration did not load"
	}
	name, model := providerAndModel(reg, ta)
	entry, ok := reg.Get(name)
	if !ok {
		return metav1.ConditionFalse, v1alpha1.TenantAgentReasonUnknownProvider,
			fmt.Sprintf("provider %q is not configured", name)
	}
	if !reg.HasModel(name, model) {
		return metav1.ConditionFalse, v1alpha1.TenantAgentReasonUnknownModel,
			fmt.Sprintf("model %q is not listed for provider %q", model, name)
	}
	if entry.Endpoint.HasSecret && !reg.Available(name) {
		return metav1.ConditionFalse, v1alpha1.TenantAgentReasonKeyMissing,
			fmt.Sprintf("the key file for provider %q is missing or empty", name)
	}
	return metav1.ConditionTrue, v1alpha1.TenantAgentReasonReady, "the provider and model are ready"
}

// providerAndModel returns the provider and model the TenantAgent names, or the
// registry default when the TenantAgent names none.
func providerAndModel(reg *provider.Registry, ta *v1alpha1.TenantAgent) (string, string) {
	name, model := ta.Spec.Provider, ta.Spec.Model
	if name == "" {
		if def, ok := reg.Default(); ok {
			name = def.Endpoint.Name
		}
	}
	if model == "" {
		if e, ok := reg.Get(name); ok {
			model = e.Endpoint.DefaultModel()
		}
	}
	return name, model
}
