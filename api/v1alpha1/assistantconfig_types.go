package v1alpha1

import (
	"net/url"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AssistantConfigName is the only name an AssistantConfig may hold. The
// object is a cluster-wide singleton.
const AssistantConfigName = "varroa-assistant"

// AssistantKeySecretName is the Secret that holds one API key per provider.
// Each data key is named after its provider.
const AssistantKeySecretName = "varroa-assistant-keys"

// Default limits of the assistant. A zero limit in the object means the default.
const (
	AssistantDefaultSessionBudget   int64  = 200000
	AssistantDefaultUserDayBudget   int64  = 1000000
	AssistantDefaultTenantDayBudget int64  = 2000000
	AssistantDefaultSteps           int32  = 32
	AssistantDefaultApprovalExpiry  string = "24h"
	AssistantDefaultSharing         string = "metadata"
)

// Enum values of the AssistantConfig kind.
const (
	AssistantProviderKindAnthropic        = "anthropic"
	AssistantProviderKindOpenAICompatible = "openai-compatible"

	AssistantTrustInternal = "internal"
	AssistantTrustExternal = "external"

	AssistantAuthAPIKey = "apiKey"
	AssistantAuthNone   = "none"

	AssistantSharingNone     = "none"
	AssistantSharingMetadata = "metadata"
	AssistantSharingLogs     = "logs"
)

// Condition type and reasons of AssistantConfig status.
const (
	// AssistantConfigCondEgressReady reports whether every provider that
	// needs network egress has CIDRs to render.
	AssistantConfigCondEgressReady = "EgressReady"
	// AssistantEgressReasonReady means the egress policy covers every provider.
	AssistantEgressReasonReady = "EgressRendered"
	// AssistantEgressReasonMissing means at least one provider has no CIDRs.
	AssistantEgressReasonMissing = "ProviderEgressMissing"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=asc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="DEFAULT",type="string",JSONPath=".spec.defaultProvider"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'varroa-assistant'",message="the only AssistantConfig name is varroa-assistant"

// AssistantConfig is the cluster-wide assistant setup: the providers, the
// default provider and model, the limits, and the data ceiling.
type AssistantConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AssistantConfigSpec   `json:"spec,omitempty"`
	Status AssistantConfigStatus `json:"status,omitempty"`
}

// AssistantConfigSpec is the desired assistant setup.
// +kubebuilder:validation:XValidation:rule="(has(self.providers) ? size(self.providers) : 0) == 0 ? (!has(self.defaultProvider) || size(self.defaultProvider) == 0) : (has(self.defaultProvider) && size(self.defaultProvider) > 0 && self.providers.exists(p, p.name == self.defaultProvider))",message="defaultProvider must name a listed provider, and it is empty when no provider is listed"
// +kubebuilder:validation:XValidation:rule="!has(self.defaultModel) || size(self.defaultModel) == 0 || (has(self.defaultProvider) && has(self.providers) && self.providers.exists(p, p.name == self.defaultProvider && p.models.exists(m, m == self.defaultModel)))",message="defaultModel must be one of the models of defaultProvider"
type AssistantConfigSpec struct {
	// Providers lists the model providers of the assistant. The name is the key.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	Providers []AssistantProvider `json:"providers,omitempty"`
	// DefaultProvider is the provider name that new sessions use. It names a listed provider.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	DefaultProvider string `json:"defaultProvider,omitempty"`
	// DefaultModel is the model that new sessions use with DefaultProvider.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	DefaultModel string `json:"defaultModel,omitempty"`
	// ExternalDataSharing is the highest data level that an external provider may receive.
	// +optional
	// +kubebuilder:default=metadata
	// +kubebuilder:validation:Enum=none;metadata;logs
	ExternalDataSharing string `json:"externalDataSharing,omitempty"`
	// Budgets are the token and step limits of the assistant.
	// +optional
	// +kubebuilder:default={}
	Budgets AssistantBudgets `json:"budgets,omitempty"`
	// ApprovalExpiry is how long a pending approval waits for a decision.
	// +optional
	// +kubebuilder:default="24h"
	// +kubebuilder:validation:Pattern=`^[1-9][0-9]*(m|h)$`
	ApprovalExpiry string `json:"approvalExpiry,omitempty"`
}

// AssistantProvider is one model provider of the assistant.
// +kubebuilder:validation:XValidation:rule="self.kind != 'openai-compatible' || (has(self.baseURL) && size(self.baseURL) > 0)",message="baseURL is required for openai-compatible"
type AssistantProvider struct {
	// Name is the DNS label that identifies the provider. It also names the key in the key Secret.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	Name string `json:"name"`
	// Kind is the wire protocol of the provider.
	// +kubebuilder:validation:Enum=anthropic;openai-compatible
	Kind string `json:"kind"`
	// BaseURL is the endpoint root. It is required for openai-compatible.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https?://`
	BaseURL string `json:"baseURL,omitempty"`
	// Models lists the model names that the provider offers to the assistant.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=128
	// +listType=set
	Models []string `json:"models"`
	// Trust is internal for a provider in the cluster network and external for any other.
	// +kubebuilder:validation:Enum=internal;external
	Trust string `json:"trust"`
	// Auth is apiKey when the provider needs a key in the key Secret, and none otherwise.
	// +optional
	// +kubebuilder:default=apiKey
	// +kubebuilder:validation:Enum=apiKey;none
	Auth string `json:"auth,omitempty"`
	// CIDRs lists the address ranges that the egress policy allows for this provider.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=43
	// +listType=set
	CIDRs []string `json:"cidrs,omitempty"`
	// Port is the TCP port of the egress rule. Zero takes the port of BaseURL.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
}

// AssistantBudgets are the spend limits of the assistant. A zero value means the default.
type AssistantBudgets struct {
	// Session is the token limit of one session.
	// +optional
	// +kubebuilder:default=200000
	// +kubebuilder:validation:Minimum=0
	Session int64 `json:"session,omitempty"`
	// UserDay is the token limit of one user in one day.
	// +optional
	// +kubebuilder:default=1000000
	// +kubebuilder:validation:Minimum=0
	UserDay int64 `json:"userDay,omitempty"`
	// TenantDay is the token limit of one tenant in one day.
	// +optional
	// +kubebuilder:default=2000000
	// +kubebuilder:validation:Minimum=0
	TenantDay int64 `json:"tenantDay,omitempty"`
	// Steps is the model step limit of one run.
	// +optional
	// +kubebuilder:default=32
	// +kubebuilder:validation:Minimum=0
	Steps int32 `json:"steps,omitempty"`
}

// AssistantConfigStatus is the observed assistant setup.
type AssistantConfigStatus struct {
	// ObservedGeneration is the generation that the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Providers reports, per provider, whether its key is set.
	// +optional
	// +listType=map
	// +listMapKey=name
	Providers []AssistantProviderStatus `json:"providers,omitempty"`
	// Conditions holds EgressReady.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// AssistantProviderStatus reports the key state of one provider.
type AssistantProviderStatus struct {
	Name   string `json:"name"`
	KeySet bool   `json:"keySet"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// AssistantConfigList contains a list of AssistantConfig objects.
type AssistantConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AssistantConfig `json:"items"`
}

// DefaultAssistantConfigSpec returns the spec of an absent object: no provider and the default limits.
func DefaultAssistantConfigSpec() AssistantConfigSpec {
	return AssistantConfigSpec{
		ExternalDataSharing: AssistantDefaultSharing,
		Budgets: AssistantBudgets{
			Session:   AssistantDefaultSessionBudget,
			UserDay:   AssistantDefaultUserDayBudget,
			TenantDay: AssistantDefaultTenantDayBudget,
			Steps:     AssistantDefaultSteps,
		},
		ApprovalExpiry: AssistantDefaultApprovalExpiry,
	}
}

// EffectivePort returns the TCP port of the egress rule. A zero Port takes the
// explicit port of BaseURL, else 80 for http and 443 for https. A BaseURL that
// does not parse takes 443.
func (p AssistantProvider) EffectivePort() int32 {
	if p.Port != 0 {
		return p.Port
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil {
		return 443
	}
	if raw := u.Port(); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 32); err == nil && n > 0 {
			return int32(n)
		}
	}
	if u.Scheme == "http" {
		return 80
	}
	return 443
}
