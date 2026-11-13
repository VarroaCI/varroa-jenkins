// Package config reads the environment of the varroa-agent process and decides
// whether the model provider a TenantAgent names is ready.
package config

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/assistantconfig"
)

// DefaultKeyPath is the signing key file that the chart mounts.
const DefaultKeyPath = "/etc/varroa/tenant-agent-key/key"

// Config is the decoded environment of varroa-agent.
type Config struct {
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
// are required. The provider configuration is not in the environment: it comes
// from the AssistantConfig object.
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
	return cfg, nil
}

// ProviderReady decides the provider condition for one TenantAgent from one
// AssistantConfig snapshot. It returns True with reason Ready when the snapshot
// has the provider and model, and the key file is present for a provider that
// needs one.
func ProviderReady(snap *assistantconfig.Snapshot, ta *v1alpha1.TenantAgent) (metav1.ConditionStatus, string, string) {
	if snap.Err != nil {
		return metav1.ConditionFalse, v1alpha1.TenantAgentReasonAssistantDisabled,
			"the assistant configuration did not load"
	}
	name, model := providerAndModel(snap, ta)
	entry, ok := snap.Registry.Get(name)
	if !ok {
		return metav1.ConditionFalse, v1alpha1.TenantAgentReasonUnknownProvider,
			fmt.Sprintf("provider %q is not configured", name)
	}
	if !snap.Registry.HasModel(name, model) {
		return metav1.ConditionFalse, v1alpha1.TenantAgentReasonUnknownModel,
			fmt.Sprintf("model %q is not listed for provider %q", model, name)
	}
	if entry.Endpoint.KeyRequired && !snap.Registry.Available(name) {
		return metav1.ConditionFalse, v1alpha1.TenantAgentReasonKeyMissing,
			fmt.Sprintf("the key file for provider %q is missing or empty", name)
	}
	return metav1.ConditionTrue, v1alpha1.TenantAgentReasonReady, "the provider and model are ready"
}

// providerAndModel returns the provider and model the TenantAgent names. A
// TenantAgent that names none takes the snapshot default.
func providerAndModel(snap *assistantconfig.Snapshot, ta *v1alpha1.TenantAgent) (string, string) {
	name, model := ta.Spec.Provider, ta.Spec.Model
	if name == "" {
		if def, ok := snap.Registry.Default(); ok {
			name = def.Endpoint.Name
		}
	}
	if model == "" {
		model = snap.Config.ModelFor(name)
	}
	return name, model
}
