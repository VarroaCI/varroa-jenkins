package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

//go:generate go run ../../hack/gen-tenantagent-enum -file tenantagent_types.go

// Trigger types. Each value names one event that can start a Tenant agent
// session. The enum on TenantAgentTrigger.Type lists these values.
const (
	TenantAgentTriggerRunFailed        = "run.failed"
	TenantAgentTriggerRunInterrupted   = "run.interrupted"
	TenantAgentTriggerCronMissed       = "cron.missed"
	TenantAgentTriggerSignatureRefused = "signature.refused"
	TenantAgentTriggerScanFailed       = "scan.failed"
	TenantAgentTriggerLockHeld         = "lock.held"
	TenantAgentTriggerPlacementMoved   = "placement.moved"
	TenantAgentTriggerManual           = "manual"
)

// TenantAgentTriggers returns every trigger type in registry order.
func TenantAgentTriggers() []string {
	return []string{
		TenantAgentTriggerRunFailed,
		TenantAgentTriggerRunInterrupted,
		TenantAgentTriggerCronMissed,
		TenantAgentTriggerSignatureRefused,
		TenantAgentTriggerScanFailed,
		TenantAgentTriggerLockHeld,
		TenantAgentTriggerPlacementMoved,
		TenantAgentTriggerManual,
	}
}

// Output kinds. A TenantAgent enables an output by setting its field.
const (
	TenantAgentOutputNote    = "note"
	TenantAgentOutputIssue   = "issue"
	TenantAgentOutputWebhook = "webhook"
)

// Condition types of a TenantAgent.
const (
	TenantAgentCondInstructionsResolved = "InstructionsResolved"
	TenantAgentCondProviderReady        = "ProviderReady"
	TenantAgentCondUnattendedAllowed    = "UnattendedAllowed"
	TenantAgentCondPaused               = "Paused"
)

// Condition reasons of a TenantAgent. The reasons are part of the operator
// contract: the UI and the troubleshooting page name them.
const (
	TenantAgentReasonResolved          = "Resolved"
	TenantAgentReasonNotConfigured     = "NotConfigured"
	TenantAgentReasonNotSynced         = "NotSynced"
	TenantAgentReasonNotDefaultBranch  = "NotDefaultBranch"
	TenantAgentReasonNotFound          = "NotFound"
	TenantAgentReasonTooLarge          = "TooLarge"
	TenantAgentReasonNotText           = "NotText"
	TenantAgentReasonHostNotAllowed    = "HostNotAllowed"
	TenantAgentReasonFetchFailed       = "FetchFailed"
	TenantAgentReasonReady             = "Ready"
	TenantAgentReasonUnknownProvider   = "UnknownProvider"
	TenantAgentReasonUnknownModel      = "UnknownModel"
	TenantAgentReasonKeyMissing        = "KeyMissing"
	TenantAgentReasonAssistantDisabled = "AssistantDisabled"
	TenantAgentReasonGranted           = "Granted"
	TenantAgentReasonPartiallyGranted  = "PartiallyGranted"
	TenantAgentReasonNoneRequested     = "NoneRequested"
	TenantAgentReasonNotGranted        = "NotGranted"
	TenantAgentReasonDisabled          = "Disabled"
	TenantAgentReasonKillSwitch        = "KillSwitch"
	TenantAgentReasonBudgetDay         = "BudgetDay"
	TenantAgentReasonTenantNotFound    = "TenantNotFound"
	TenantAgentReasonNamespaceNotOwned = "NamespaceNotOwned"
	TenantAgentReasonDuplicate         = "Duplicate"
)

// Labels that the operator writes on each VarroaRoleBinding of a TenantAgent.
const (
	LabelTenantAgent          = "varroa.dev/tenant-agent"
	LabelTenantAgentNamespace = "varroa.dev/tenant-agent-namespace"
	LabelTenantAgentName      = "varroa.dev/tenant-agent-name"
)

const (
	// DefaultTenantAgentInstructionsPath is the file read when a TenantAgent
	// names no instructions path.
	DefaultTenantAgentInstructionsPath = ".varroa/tenant-agent.md"
	// DefaultTenantAgentDebounce is the minimum gap between two sessions of
	// the same TenantAgent for a non-manual trigger.
	DefaultTenantAgentDebounce = 10 * time.Minute
)

// TenantAgent is the Schema for the tenant agents API. It configures one agent
// that works for one pipelines Tenant.
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="TENANT",type="string",JSONPath=".spec.tenantRef.name"
// +kubebuilder:printcolumn:name="ENABLED",type="boolean",JSONPath=".spec.enabled"
// +kubebuilder:printcolumn:name="PAUSED",type="string",JSONPath=".status.conditions[?(@.type=='Paused')].status"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="metadata.name must be at most 63 characters"
type TenantAgent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantAgentSpec   `json:"spec,omitempty"`
	Status TenantAgentStatus `json:"status,omitempty"`
}

// TenantAgentSpec configures one Tenant agent.
type TenantAgentSpec struct {
	// TenantRef names the pipelines Tenant the agent works for.
	TenantRef TenantAgentTenantRef `json:"tenantRef"`
	// Enabled turns the agent on. A nil value means enabled.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// Provider names the model provider. It is empty when the assistant
	// default applies.
	// +optional
	Provider string `json:"provider,omitempty"`
	// Model names the provider model. It is empty when the assistant
	// default applies.
	// +optional
	Model string `json:"model,omitempty"`
	// Instructions names the file that holds the standing instructions.
	// +optional
	Instructions *TenantAgentInstructions `json:"instructions,omitempty"`
	// Triggers lists the events that start a session.
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=8
	// +optional
	Triggers []TenantAgentTrigger `json:"triggers,omitempty"`
	// UnattendedWrites lists the write tools that run without a person's
	// approval. The enum is generated from the policy package.
	// +listType=set
	// +kubebuilder:validation:MaxItems=3
	// +kubebuilder:validation:items:Enum=run_pipeline;restart_run;scan_multibranch
	// +optional
	UnattendedWrites []string `json:"unattendedWrites,omitempty"`
	// Outputs turns on the outputs the agent may use.
	// +optional
	Outputs TenantAgentOutputs `json:"outputs,omitempty"`
	// Budgets caps the tokens of a session and of a day.
	// +optional
	Budgets *TenantAgentBudgets `json:"budgets,omitempty"`
	// DataSharing selects how much run data the agent reads.
	// +kubebuilder:validation:Enum=none;metadata;logs
	// +optional
	DataSharing string `json:"dataSharing,omitempty"`
}

// TenantAgentTenantRef names a pipelines Tenant.
type TenantAgentTenantRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=47
	// +kubebuilder:validation:Pattern=`^[^/.]+$`
	Name string `json:"name"`
}

// TenantAgentInstructions names the file that holds the standing instructions.
// +kubebuilder:validation:XValidation:rule="!has(self.path) || (!self.path.startsWith('/') && !self.path.matches('(^|/)[.][.](/|$)'))",message="instructions.path must be relative with no .. segment"
type TenantAgentInstructions struct {
	// Source names a CatalogSource in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Source string `json:"source"`
	// Path names the file inside the source repository.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._/-]{1,512}$`
	// +optional
	Path string `json:"path,omitempty"`
}

// TenantAgentTrigger starts a session when its event occurs.
// +kubebuilder:validation:XValidation:rule="!(self.type == 'manual' && has(self.debounce))",message="triggers[].debounce is not allowed on the manual trigger"
// +kubebuilder:validation:XValidation:rule="!has(self.debounce) || (duration(self.debounce) >= duration('1m') && duration(self.debounce) <= duration('24h'))",message="triggers[].debounce must be between 1m and 24h"
type TenantAgentTrigger struct {
	// +kubebuilder:validation:Enum=run.failed;run.interrupted;cron.missed;signature.refused;scan.failed;lock.held;placement.moved;manual
	Type string `json:"type"`
	// Debounce is the minimum gap between two sessions for this trigger.
	// +optional
	Debounce *metav1.Duration `json:"debounce,omitempty"`
}

// TenantAgentOutputs turns on the outputs of an agent.
type TenantAgentOutputs struct {
	// Notes allows the agent to write notes on runs and jobs.
	// +optional
	Notes bool `json:"notes,omitempty"`
	// Issues allows the agent to open issues on a forge.
	// +optional
	Issues *TenantAgentIssueOutput `json:"issues,omitempty"`
	// Webhook allows the agent to send a message to the tenant webhook.
	// +optional
	Webhook bool `json:"webhook,omitempty"`
}

// TenantAgentIssueOutput names the repository and the forge of issues.
type TenantAgentIssueOutput struct {
	// Repo is the repository URL. Only https is accepted.
	// +kubebuilder:validation:Pattern=`^https://`
	// +kubebuilder:validation:MaxLength=2048
	Repo string `json:"repo"`
	// Forge selects the forge kind of Repo.
	// +kubebuilder:validation:Enum=github;gitea
	Forge string `json:"forge"`
	// Labels lists the labels an issue may carry.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=50
	// +optional
	Labels []string `json:"labels,omitempty"`
}

// TenantAgentBudgets caps the tokens an agent spends.
type TenantAgentBudgets struct {
	// Session caps the tokens of one session.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Session *int64 `json:"session,omitempty"`
	// Day caps the tokens of one UTC day.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Day *int64 `json:"day,omitempty"`
}

// TenantAgentStatus reports the state of one Tenant agent. The operator and the
// recorder write it; the spec is the only input a person edits.
type TenantAgentStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// InstructionsSHA is the commit that the last instructions read used.
	// +optional
	InstructionsSHA string `json:"instructionsSHA,omitempty"`
	// LastSession names the most recent session.
	// +optional
	LastSession *TenantAgentLastSession `json:"lastSession,omitempty"`
	// SpendToday is the tokens the agent spent on the current UTC day.
	// +optional
	SpendToday int64 `json:"spendToday,omitempty"`
	// ObservedGeneration is the metadata.generation the last reconcile saw.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// TenantAgentLastSession names the most recent session of a Tenant agent.
type TenantAgentLastSession struct {
	ID      string      `json:"id"`
	Trigger string      `json:"trigger"`
	State   string      `json:"state"`
	At      metav1.Time `json:"at"`
}

// TenantAgentList contains a list of TenantAgent objects.
type TenantAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TenantAgent `json:"items"`
}
