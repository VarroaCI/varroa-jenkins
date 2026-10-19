package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

//go:generate go run ../../hack/gen-teamagent-enum -file teamagent_types.go

// Trigger types. Each value names one event that can start a Team agent
// session. The enum on TeamAgentTrigger.Type lists these values.
const (
	TeamAgentTriggerRunFailed        = "run.failed"
	TeamAgentTriggerRunInterrupted   = "run.interrupted"
	TeamAgentTriggerCronMissed       = "cron.missed"
	TeamAgentTriggerSignatureRefused = "signature.refused"
	TeamAgentTriggerScanFailed       = "scan.failed"
	TeamAgentTriggerLockHeld         = "lock.held"
	TeamAgentTriggerPlacementMoved   = "placement.moved"
	TeamAgentTriggerManual           = "manual"
)

// TeamAgentTriggers returns every trigger type in registry order.
func TeamAgentTriggers() []string {
	return []string{
		TeamAgentTriggerRunFailed,
		TeamAgentTriggerRunInterrupted,
		TeamAgentTriggerCronMissed,
		TeamAgentTriggerSignatureRefused,
		TeamAgentTriggerScanFailed,
		TeamAgentTriggerLockHeld,
		TeamAgentTriggerPlacementMoved,
		TeamAgentTriggerManual,
	}
}

// Output kinds. A TeamAgent enables an output by setting its field.
const (
	TeamAgentOutputNote    = "note"
	TeamAgentOutputIssue   = "issue"
	TeamAgentOutputWebhook = "webhook"
)

// Condition types of a TeamAgent.
const (
	TeamAgentCondInstructionsResolved = "InstructionsResolved"
	TeamAgentCondProviderReady        = "ProviderReady"
	TeamAgentCondUnattendedAllowed    = "UnattendedAllowed"
	TeamAgentCondPaused               = "Paused"
)

// Condition reasons of a TeamAgent. The reasons are part of the operator
// contract: the UI and the troubleshooting page name them.
const (
	TeamAgentReasonResolved          = "Resolved"
	TeamAgentReasonNotConfigured     = "NotConfigured"
	TeamAgentReasonNotSynced         = "NotSynced"
	TeamAgentReasonNotDefaultBranch  = "NotDefaultBranch"
	TeamAgentReasonNotFound          = "NotFound"
	TeamAgentReasonTooLarge          = "TooLarge"
	TeamAgentReasonNotText           = "NotText"
	TeamAgentReasonHostNotAllowed    = "HostNotAllowed"
	TeamAgentReasonFetchFailed       = "FetchFailed"
	TeamAgentReasonReady             = "Ready"
	TeamAgentReasonUnknownProvider   = "UnknownProvider"
	TeamAgentReasonUnknownModel      = "UnknownModel"
	TeamAgentReasonKeyMissing        = "KeyMissing"
	TeamAgentReasonAssistantDisabled = "AssistantDisabled"
	TeamAgentReasonGranted           = "Granted"
	TeamAgentReasonPartiallyGranted  = "PartiallyGranted"
	TeamAgentReasonNoneRequested     = "NoneRequested"
	TeamAgentReasonNotGranted        = "NotGranted"
	TeamAgentReasonDisabled          = "Disabled"
	TeamAgentReasonKillSwitch        = "KillSwitch"
	TeamAgentReasonBudgetDay         = "BudgetDay"
	TeamAgentReasonTeamNotFound      = "TeamNotFound"
	TeamAgentReasonNamespaceNotOwned = "NamespaceNotOwned"
	TeamAgentReasonDuplicate         = "Duplicate"
)

// Labels that the operator writes on each VarroaRoleBinding of a TeamAgent.
const (
	LabelTeamAgent          = "varroa.dev/team-agent"
	LabelTeamAgentNamespace = "varroa.dev/team-agent-namespace"
	LabelTeamAgentName      = "varroa.dev/team-agent-name"
)

const (
	// DefaultTeamAgentInstructionsPath is the file read when a TeamAgent
	// names no instructions path.
	DefaultTeamAgentInstructionsPath = ".varroa/team-agent.md"
	// DefaultTeamAgentDebounce is the minimum gap between two sessions of
	// the same TeamAgent for a non-manual trigger.
	DefaultTeamAgentDebounce = 10 * time.Minute
)

// TeamAgent is the Schema for the team agents API. It configures one agent
// that works for one pipelines Team.
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="TEAM",type="string",JSONPath=".spec.teamRef.name"
// +kubebuilder:printcolumn:name="ENABLED",type="boolean",JSONPath=".spec.enabled"
// +kubebuilder:printcolumn:name="PAUSED",type="string",JSONPath=".status.conditions[?(@.type=='Paused')].status"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="metadata.name must be at most 63 characters"
type TeamAgent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TeamAgentSpec   `json:"spec,omitempty"`
	Status TeamAgentStatus `json:"status,omitempty"`
}

// TeamAgentSpec configures one Team agent.
type TeamAgentSpec struct {
	// TeamRef names the pipelines Team the agent works for.
	TeamRef TeamAgentTeamRef `json:"teamRef"`
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
	Instructions *TeamAgentInstructions `json:"instructions,omitempty"`
	// Triggers lists the events that start a session.
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=8
	// +optional
	Triggers []TeamAgentTrigger `json:"triggers,omitempty"`
	// UnattendedWrites lists the write tools that run without a person's
	// approval. The enum is generated from the policy package.
	// +listType=set
	// +kubebuilder:validation:MaxItems=3
	// +kubebuilder:validation:items:Enum=run_pipeline;restart_run;scan_multibranch
	// +optional
	UnattendedWrites []string `json:"unattendedWrites,omitempty"`
	// Outputs turns on the outputs the agent may use.
	// +optional
	Outputs TeamAgentOutputs `json:"outputs,omitempty"`
	// Budgets caps the tokens of a session and of a day.
	// +optional
	Budgets *TeamAgentBudgets `json:"budgets,omitempty"`
	// DataSharing selects how much run data the agent reads.
	// +kubebuilder:validation:Enum=none;metadata;logs
	// +optional
	DataSharing string `json:"dataSharing,omitempty"`
}

// TeamAgentTeamRef names a pipelines Team.
type TeamAgentTeamRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=47
	// +kubebuilder:validation:Pattern=`^[^/.]+$`
	Name string `json:"name"`
}

// TeamAgentInstructions names the file that holds the standing instructions.
// +kubebuilder:validation:XValidation:rule="!has(self.path) || (!self.path.startsWith('/') && !self.path.matches('(^|/)[.][.](/|$)'))",message="instructions.path must be relative with no .. segment"
type TeamAgentInstructions struct {
	// Source names a CatalogSource in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Source string `json:"source"`
	// Path names the file inside the source repository.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._/-]{1,512}$`
	// +optional
	Path string `json:"path,omitempty"`
}

// TeamAgentTrigger starts a session when its event occurs.
// +kubebuilder:validation:XValidation:rule="!(self.type == 'manual' && has(self.debounce))",message="triggers[].debounce is not allowed on the manual trigger"
// +kubebuilder:validation:XValidation:rule="!has(self.debounce) || (duration(self.debounce) >= duration('1m') && duration(self.debounce) <= duration('24h'))",message="triggers[].debounce must be between 1m and 24h"
type TeamAgentTrigger struct {
	// +kubebuilder:validation:Enum=run.failed;run.interrupted;cron.missed;signature.refused;scan.failed;lock.held;placement.moved;manual
	Type string `json:"type"`
	// Debounce is the minimum gap between two sessions for this trigger.
	// +optional
	Debounce *metav1.Duration `json:"debounce,omitempty"`
}

// TeamAgentOutputs turns on the outputs of an agent.
type TeamAgentOutputs struct {
	// Notes allows the agent to write notes on runs and jobs.
	// +optional
	Notes bool `json:"notes,omitempty"`
	// Issues allows the agent to open issues on a forge.
	// +optional
	Issues *TeamAgentIssueOutput `json:"issues,omitempty"`
	// Webhook allows the agent to send a message to the tenant webhook.
	// +optional
	Webhook bool `json:"webhook,omitempty"`
}

// TeamAgentIssueOutput names the repository and the forge of issues.
type TeamAgentIssueOutput struct {
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

// TeamAgentBudgets caps the tokens an agent spends.
type TeamAgentBudgets struct {
	// Session caps the tokens of one session.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Session *int64 `json:"session,omitempty"`
	// Day caps the tokens of one UTC day.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Day *int64 `json:"day,omitempty"`
}

// TeamAgentStatus reports the state of one Team agent. The operator and the
// recorder write it; the spec is the only input a person edits.
type TeamAgentStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// InstructionsSHA is the commit that the last instructions read used.
	// +optional
	InstructionsSHA string `json:"instructionsSHA,omitempty"`
	// LastSession names the most recent session.
	// +optional
	LastSession *TeamAgentLastSession `json:"lastSession,omitempty"`
	// SpendToday is the tokens the agent spent on the current UTC day.
	// +optional
	SpendToday int64 `json:"spendToday,omitempty"`
	// ObservedGeneration is the metadata.generation the last reconcile saw.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// TeamAgentLastSession names the most recent session of a Team agent.
type TeamAgentLastSession struct {
	ID      string      `json:"id"`
	Trigger string      `json:"trigger"`
	State   string      `json:"state"`
	At      metav1.Time `json:"at"`
}

// TeamAgentList contains a list of TeamAgent objects.
type TeamAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TeamAgent `json:"items"`
}
