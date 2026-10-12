package items

import (
	"fmt"
	"regexp"
	"strings"
)

// ---- Top-level manifest ----

// Manifest is the top-level structure of an items.yaml file.
type Manifest struct {
	RemoveStrategy *RemoveStrategy `yaml:"removeStrategy,omitempty"`
	Root           string          `yaml:"root,omitempty"`
	// Retain names top-level item paths whose managed items the engine
	// keeps as they are: it does not delete them and does not write them.
	// A cell writes it for a tenant folder that it holds, because a folder
	// absent from a sync manifest is otherwise deleted.
	Retain []string `yaml:"retain,omitempty"`
	Items  []Item   `yaml:"items"`
}

// RemoveStrategy defines how existing items and RBAC are handled.
type RemoveStrategy struct {
	Items string `yaml:"items,omitempty"`
	Rbac  string `yaml:"rbac,omitempty"`
}

// ---- Item ----

// Item represents a single Jenkins item declaration.
type Item struct {
	Kind        string `yaml:"kind"`
	Name        string `yaml:"name"`
	DisplayName string `yaml:"displayName,omitempty"`
	Description string `yaml:"description,omitempty"`
	Disabled    bool   `yaml:"disabled,omitempty"`

	// RBAC (all item kinds)
	Groups        []Group  `yaml:"groups,omitempty"`
	FilteredRoles []string `yaml:"filteredRoles,omitempty"`

	// Folder-specific: nested items
	Items []Item `yaml:"items,omitempty"`

	// Shared across multiple kinds. A pipeline or freeStyle item declares
	// its build parameters here, as a flat list. Either kind may also
	// declare parameters through its properties (see Property.Parameters).
	// A controller renders both sources into one job.
	Properties     []Property      `yaml:"properties,omitempty"`
	BuildDiscarder *BuildDiscarder `yaml:"buildDiscarder,omitempty"`
	Parameters     []Parameter     `yaml:"parameters,omitempty"`
	// Triggers is valid on pipeline and freeStyle only.
	Triggers []Trigger `yaml:"triggers,omitempty"`

	// Pipeline-specific
	Definition      *PipelineDefinition `yaml:"definition,omitempty"`
	ConcurrentBuild bool                `yaml:"concurrentBuild,omitempty"`
	ResumeBlocked   bool                `yaml:"resumeBlocked,omitempty"`
	QuietPeriod     int                 `yaml:"quietPeriod,omitempty"`

	// FreeStyle-specific
	SCM                              *SCM        `yaml:"scm,omitempty"`
	Builders                         []Builder   `yaml:"builders,omitempty"`
	PublishersList                   []Publisher `yaml:"publishersList,omitempty"`
	BlockBuildWhenDownstreamBuilding bool        `yaml:"blockBuildWhenDownstreamBuilding,omitempty"`
	BlockBuildWhenUpstreamBuilding   bool        `yaml:"blockBuildWhenUpstreamBuilding,omitempty"`
	CustomWorkspace                  string      `yaml:"customWorkspace,omitempty"`
	ScmCheckoutStrategy              string      `yaml:"scmCheckoutStrategy,omitempty"`
	Label                            string      `yaml:"label,omitempty"`

	// Multibranch-specific
	SourcesList          []BranchSourceEntry `yaml:"sourcesList,omitempty"`
	ProjectFactory       *ProjectFactory     `yaml:"projectFactory,omitempty"`
	OrphanedItemStrategy *OrphanedStrategy   `yaml:"orphanedItemStrategy,omitempty"`

	// OrganizationFolder-specific
	Navigators       []SCMNavigator   `yaml:"navigators,omitempty"`
	ProjectFactories []ProjectFactory `yaml:"projectFactories,omitempty"`
}

// ---- Pipeline Definition ----

// PipelineDefinition wraps the possible pipeline definition types.
type PipelineDefinition struct {
	CpsScmFlowDefinition *CpsScmFlowDefinition     `yaml:"cpsScmFlowDefinition,omitempty"`
	Script               string                    `yaml:"script,omitempty"`
	VarroaTemplate       *VarroaTemplateDefinition `yaml:"varroaTemplate,omitempty"`
}

// VarroaTemplateDefinition selects a catalog-delivered pipeline template for
// an items-declared pipeline job. The operator can see this content in
// full, so a composer validates Values against the resolved template's
// schema before the item reaches a controller.
type VarroaTemplateDefinition struct {
	Template        string         `yaml:"template"`
	TemplateVersion string         `yaml:"templateVersion,omitempty"`
	Values          map[string]any `yaml:"values,omitempty"`
}

// CpsScmFlowDefinition defines a pipeline from SCM with a Jenkinsfile.
type CpsScmFlowDefinition struct {
	SCM         SCM    `yaml:"scm"`
	ScriptPath  string `yaml:"scriptPath"`
	Lightweight bool   `yaml:"lightweight,omitempty"`
}

// ---- SCM ----

// SCM defines source control configuration.
type SCM struct {
	GitSCM *GitSCM `yaml:"gitSCM,omitempty"`
}

// GitSCM mirrors the Jenkins GitSCM plugin configuration.
type GitSCM struct {
	GitTool                           string             `yaml:"gitTool,omitempty"`
	UserRemoteConfigs                 []UserRemoteConfig `yaml:"userRemoteConfigs,omitempty"`
	Branches                          []BranchSpec       `yaml:"branches,omitempty"`
	Browser                           *GitBrowser        `yaml:"browser,omitempty"`
	Extensions                        []GitExtension     `yaml:"extensions,omitempty"`
	DoGenerateSubmoduleConfigurations bool               `yaml:"doGenerateSubmoduleConfigurations,omitempty"`
}

// UserRemoteConfig wraps a single remote repository configuration.
type UserRemoteConfig struct {
	UserRemoteConfig RemoteConfig `yaml:"userRemoteConfig"`
}

// RemoteConfig defines a git remote URL and optional credentials.
type RemoteConfig struct {
	URL           string `yaml:"url"`
	Name          string `yaml:"name,omitempty"`
	CredentialsID string `yaml:"credentialsId,omitempty"`
}

// BranchSpec wraps a single branch specification.
type BranchSpec struct {
	BranchSpec BranchSpecConfig `yaml:"branchSpec"`
}

// BranchSpecConfig defines a branch name pattern.
type BranchSpecConfig struct {
	Name string `yaml:"name"`
}

// GitBrowser defines the repository browser for SCM links.
type GitBrowser struct {
	GithubWeb *GithubWeb `yaml:"githubWeb,omitempty"`
}

// GithubWeb configures GitHub web links.
type GithubWeb struct {
	RepoURL string `yaml:"repoUrl"`
}

// ---- Git Extensions ----

// GitExtension is a marker interface for git SCM extensions.
type GitExtension struct {
	CleanBeforeCheckout     *CleanBeforeCheckout       `yaml:"cleanBeforeCheckout,omitempty"`
	CleanCheckout           *CleanCheckout             `yaml:"cleanCheckout,omitempty"`
	CheckoutOption          *CheckoutOption            `yaml:"checkoutOption,omitempty"`
	CloneOption             *CloneOption               `yaml:"cloneOption,omitempty"`
	RelativeTargetDirectory *RelativeTargetDirectory   `yaml:"relativeTargetDirectory,omitempty"`
	SparseCheckoutPaths     *SparseCheckoutPathsConfig `yaml:"sparseCheckoutPaths,omitempty"`
	PruneStaleBranch        *PruneStaleBranch          `yaml:"pruneStaleBranch,omitempty"`
	WipeWorkspace           *WipeWorkspace             `yaml:"wipeWorkspace,omitempty"`
}

// CleanBeforeCheckout cleans the workspace before checkout.
type CleanBeforeCheckout struct {
	DeleteUntrackedNestedRepositories bool `yaml:"deleteUntrackedNestedRepositories,omitempty"`
}

// CleanCheckout cleans after checkout.
type CleanCheckout struct {
	DeleteUntrackedNestedRepositories bool `yaml:"deleteUntrackedNestedRepositories,omitempty"`
}

// CheckoutOption configures checkout timeout.
type CheckoutOption struct {
	Timeout int `yaml:"timeout,omitempty"`
}

// CloneOption configures clone behavior.
type CloneOption struct {
	Reference    string `yaml:"reference,omitempty"`
	NoTags       bool   `yaml:"noTags,omitempty"`
	HonorRefspec bool   `yaml:"honorRefspec,omitempty"`
	Shallow      bool   `yaml:"shallow,omitempty"`
	Timeout      int    `yaml:"timeout,omitempty"`
}

// RelativeTargetDirectory checks out to a subdirectory.
type RelativeTargetDirectory struct {
	RelativeTargetDir string `yaml:"relativeTargetDir"`
}

// SparseCheckoutPathsConfig configures sparse checkout.
type SparseCheckoutPathsConfig struct {
	SparseCheckoutPaths []SparseCheckoutPath `yaml:"sparseCheckoutPaths,omitempty"`
}

// SparseCheckoutPath defines a single path for sparse checkout.
type SparseCheckoutPath struct {
	SparseCheckoutPath PathSpec `yaml:"sparseCheckoutPath"`
}

// PathSpec holds a single path string.
type PathSpec struct {
	Path string `yaml:"path"`
}

// PruneStaleBranch enables pruning stale remote branches.
type PruneStaleBranch struct{}

// WipeWorkspace wipes the workspace before build.
type WipeWorkspace struct{}

// ---- Builders ----

// Builder defines a build step.
type Builder struct {
	Shell *ShellBuilder `yaml:"shell,omitempty"`
	Maven *MavenBuilder `yaml:"maven,omitempty"`
}

// ShellBuilder runs a shell command.
type ShellBuilder struct {
	Command string `yaml:"command"`
}

// MavenBuilder runs Maven targets.
type MavenBuilder struct {
	Targets              string         `yaml:"targets"`
	Settings             *MavenSettings `yaml:"settings,omitempty"`
	GlobalSettings       *MavenSettings `yaml:"globalSettings,omitempty"`
	InjectBuildVariables bool           `yaml:"injectBuildVariables,omitempty"`
	UsePrivateRepository bool           `yaml:"usePrivateRepository,omitempty"`
}

// MavenSettings wraps standard/alternate Maven settings.
type MavenSettings struct {
	Standard *StandardMavenSettings `yaml:"standard,omitempty"`
}

// StandardMavenSettings uses the built-in Maven settings.
type StandardMavenSettings struct{}

// ---- Publishers (post-build actions) ----

// Publisher defines a post-build action.
type Publisher struct {
	ArchiveArtifacts    *ArchiveArtifacts    `yaml:"archiveArtifacts,omitempty"`
	JUnitResultArchiver *JUnitResultArchiver `yaml:"jUnitResultArchiver,omitempty"`
	Mailer              *MailerPublisher     `yaml:"mailer,omitempty"`
}

// ArchiveArtifacts archives build artifacts.
type ArchiveArtifacts struct {
	AllowEmptyArchive bool   `yaml:"allowEmptyArchive,omitempty"`
	CaseSensitive     bool   `yaml:"caseSensitive,omitempty"`
	OnlyIfSuccessful  bool   `yaml:"onlyIfSuccessful,omitempty"`
	Fingerprint       bool   `yaml:"fingerprint,omitempty"`
	DefaultExcludes   bool   `yaml:"defaultExcludes,omitempty"`
	FollowSymlinks    bool   `yaml:"followSymlinks,omitempty"`
	Artifacts         string `yaml:"artifacts,omitempty"`
}

// JUnitResultArchiver publishes JUnit test results.
type JUnitResultArchiver struct {
	TestResults       string  `yaml:"testResults"`
	AllowEmptyResults bool    `yaml:"allowEmptyResults,omitempty"`
	HealthScaleFactor float64 `yaml:"healthScaleFactor,omitempty"`
	KeepLongStdio     bool    `yaml:"keepLongStdio,omitempty"`
}

// MailerPublisher sends email notifications.
type MailerPublisher struct {
	NotifyEveryUnstableBuild bool   `yaml:"notifyEveryUnstableBuild,omitempty"`
	SendToIndividuals        bool   `yaml:"sendToIndividuals,omitempty"`
	Recipients               string `yaml:"recipients,omitempty"`
}

// ---- Triggers ----

// Trigger defines a build trigger. An entry sets exactly one trigger type.
type Trigger struct {
	PollSCM        *PollSCMTrigger        `yaml:"pollSCM,omitempty"`
	Cron           *CronTrigger           `yaml:"cron,omitempty"`
	GitHubPush     *GitHubPushTrigger     `yaml:"githubPush,omitempty"`
	VarroaUpstream *VarroaUpstreamTrigger `yaml:"varroaUpstream,omitempty"`
	GenericTrigger *GenericTrigger        `yaml:"genericTrigger,omitempty"`
}

// GenericTrigger starts a build when a webhook call reaches the
// generic-webhook-trigger endpoint with this token. The token is the
// job's own secret. A tenant door call must carry "<team>:<name>".
type GenericTrigger struct {
	Token                  string            `yaml:"token"`
	CauseString            string            `yaml:"causeString,omitempty"`
	RegexpFilterText       string            `yaml:"regexpFilterText,omitempty"`
	RegexpFilterExpression string            `yaml:"regexpFilterExpression,omitempty"`
	GenericVariables       []GenericVariable `yaml:"genericVariables,omitempty"`
}

// genericExpressionTypes holds the two expression types the plugin's
// ExpressionType enum accepts. Any other value fails to load the trigger.
var genericExpressionTypes = map[string]bool{
	"JSONPath": true,
	"XPath":    true,
}

// GenericVariable extracts one value from the webhook body into a build
// variable. Header values are not extractable from items.yaml.
type GenericVariable struct {
	Key            string `yaml:"key"`
	Value          string `yaml:"value"`
	ExpressionType string `yaml:"expressionType,omitempty"` // defaults to JSONPath
	RegexpFilter   string `yaml:"regexpFilter,omitempty"`
	DefaultValue   string `yaml:"defaultValue,omitempty"`
}

// GitHubPushTrigger starts a build when the github plugin receives a push
// webhook for the job's repository. It has no fields.
type GitHubPushTrigger struct{}

// PollSCMTrigger polls SCM on a schedule.
type PollSCMTrigger struct {
	IgnorePostCommitHooks bool   `yaml:"ignorePostCommitHooks,omitempty"`
	ScmpollSpec           string `yaml:"scmpoll_spec,omitempty"`
}

// defaultPollSCMSpec is the poll schedule of a pollSCM trigger with no spec.
const defaultPollSCMSpec = "H * * * *"

// EffectiveSpec returns the poll schedule Jenkins runs. A blank spec
// gives the default schedule. Every reader of the schedule calls it.
func (p *PollSCMTrigger) EffectiveSpec() string {
	if spec := strings.TrimSpace(p.ScmpollSpec); spec != "" {
		return spec
	}
	return defaultPollSCMSpec
}

// CronTrigger triggers builds on a cron schedule.
type CronTrigger struct {
	Spec string `yaml:"spec"`
}

// varroaUpstreamResults holds the three thresholds a VarroaUpstreamTrigger
// accepts, design.md D9. A threshold means "this result or better",
// ReverseBuildTrigger's rule.
var varroaUpstreamResults = map[string]bool{
	"SUCCESS":  true,
	"UNSTABLE": true,
	"FAILURE":  true,
}

// varroaUpstreamDefaultResult is the threshold an item declares when it
// omits result. The plugin's own trigger applies the same default.
const varroaUpstreamDefaultResult = "SUCCESS"

// VarroaUpstreamTrigger declares a subscription to another tenant's job.
// The recorder, not the cell, matches it against a finished run; the
// trigger itself starts no timer and polls nothing. Tenant and Job name
// the producer; Job is relative to the producer's own folder. Result is
// the threshold "this result or better" and defaults to SUCCESS.
type VarroaUpstreamTrigger struct {
	Tenant string `yaml:"tenant"`
	Job    string `yaml:"job"`
	Result string `yaml:"result,omitempty"`
}

// triggerKinds holds the two item kinds whose renderer writes triggers.
// No other kind runs builds of its own.
var triggerKinds = map[string]bool{
	"pipeline":  true,
	"freeStyle": true,
}

// collectTriggers returns every trigger a renderer writes onto one job:
// the item-level list first, then each pipelineTriggers property in order.
func collectTriggers(item Item) []Trigger {
	all := append([]Trigger(nil), item.Triggers...)
	for _, p := range item.Properties {
		if p.PipelineTriggers != nil {
			all = append(all, p.PipelineTriggers.Triggers...)
		}
	}
	return all
}

// triggerType returns the YAML name of the one type a trigger entry sets.
// It returns the empty string when the entry sets none or more than one.
func triggerType(t Trigger) string {
	var names []string
	if t.PollSCM != nil {
		names = append(names, "pollSCM")
	}
	if t.Cron != nil {
		names = append(names, "cron")
	}
	if t.GitHubPush != nil {
		names = append(names, "githubPush")
	}
	if t.VarroaUpstream != nil {
		names = append(names, "varroaUpstream")
	}
	if t.GenericTrigger != nil {
		names = append(names, "genericTrigger")
	}
	if len(names) != 1 {
		return ""
	}
	return names[0]
}

// validateTriggers checks the triggers from both spellings together,
// because the renderer writes them onto one job. Jenkins keeps one
// trigger per type, so a repeated type is an error. The item-level list
// is valid on pipeline and freeStyle. The pipelineTriggers property is
// valid on pipeline only. An empty varroaUpstream result is not an error:
// the renderer applies the SUCCESS default.
func validateTriggers(item Item) error {
	name, kind := item.Name, item.Kind
	if len(item.Triggers) > 0 && !triggerKinds[kind] {
		return fmt.Errorf("item %q: triggers are only valid on kind=pipeline and kind=freeStyle, not kind=%s", name, kind)
	}
	for _, p := range item.Properties {
		if p.PipelineTriggers != nil && kind != "pipeline" {
			return fmt.Errorf("item %q: pipelineTriggers is only valid on kind=pipeline, not kind=%s", name, kind)
		}
	}
	seen := make(map[string]bool)
	for _, t := range collectTriggers(item) {
		typ := triggerType(t)
		if typ == "" {
			return fmt.Errorf("item %q: a trigger requires exactly one of pollSCM, cron, githubPush, and varroaUpstream", name)
		}
		if seen[typ] {
			return fmt.Errorf("item %q: more than one %s trigger across triggers and pipelineTriggers", name, typ)
		}
		seen[typ] = true
		if t.Cron != nil && strings.TrimSpace(t.Cron.Spec) == "" {
			return fmt.Errorf("item %q: cron trigger requires spec", name)
		}
		if vu := t.VarroaUpstream; vu != nil {
			if strings.TrimSpace(vu.Tenant) == "" {
				return fmt.Errorf("item %q: varroaUpstream trigger requires tenant", name)
			}
			if strings.TrimSpace(vu.Job) == "" {
				return fmt.Errorf("item %q: varroaUpstream trigger requires job", name)
			}
			if vu.Result != "" && !varroaUpstreamResults[vu.Result] {
				return fmt.Errorf("item %q: varroaUpstream trigger result %q must be one of SUCCESS, UNSTABLE, FAILURE", name, vu.Result)
			}
		}
		if gt := t.GenericTrigger; gt != nil {
			if strings.TrimSpace(gt.Token) == "" {
				return fmt.Errorf("item %q: genericTrigger requires token", name)
			}
			for _, v := range gt.GenericVariables {
				if v.ExpressionType != "" && !genericExpressionTypes[v.ExpressionType] {
					return fmt.Errorf("item %q: genericTrigger variable %q expressionType %q must be JSONPath or XPath", name, v.Key, v.ExpressionType)
				}
			}
		}
	}
	return nil
}

// parameterKinds holds the two item kinds a controller renders parameters
// for. No renderer reads a parameter declared on any other kind.
var parameterKinds = map[string]bool{
	"pipeline":  true,
	"freeStyle": true,
}

// validateParameters rejects a parameter declared on a kind no renderer
// consumes it from, a build parameter that sets none or more than one of
// string, choice, and booleanParam, an unnamed parameter, a choice
// parameter with no choices, and a name repeated across the two spellings.
func validateParameters(itemName, kind string, params []Parameter) error {
	if len(params) > 0 && !parameterKinds[kind] {
		return fmt.Errorf("item %q: a build parameter, from either the item-level list or a parameters property, is only valid on kind=pipeline and kind=freeStyle, not kind=%s", itemName, kind)
	}
	seen := make(map[string]bool, len(params))
	for _, p := range params {
		set := 0
		var name string
		if p.String != nil {
			set++
			name = p.String.Name
		}
		if p.Choice != nil {
			set++
			name = p.Choice.Name
			if len(p.Choice.Choices) == 0 {
				return fmt.Errorf("item %q: choice parameter %q requires at least one choice", itemName, name)
			}
		}
		if p.BooleanParam != nil {
			set++
			name = p.BooleanParam.Name
		}
		if set != 1 {
			return fmt.Errorf("item %q: a parameter requires exactly one of string, choice, and booleanParam", itemName)
		}
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("item %q: a parameter requires a name", itemName)
		}
		if seen[name] {
			return fmt.Errorf("item %q: parameter %q is declared more than once", itemName, name)
		}
		seen[name] = true
	}
	return nil
}

// validDurabilityHints holds the three FlowDurabilityHint names Jenkins
// core defines.
var validDurabilityHints = map[string]bool{
	"PERFORMANCE_OPTIMIZED": true,
	"SURVIVABLE_NONATOMIC":  true,
	"MAX_SURVIVABILITY":     true,
}

// validateDurabilityHints rejects a durabilityHint property declared on a
// kind other than pipeline, the only kind whose renderer reads it, and a
// value that is not one of the three FlowDurabilityHint names. Jenkins
// fails to load a job with any other value, on a cell or not, so this runs
// for every controller rather than only the cell lint pass.
func validateDurabilityHints(itemName, kind string, props []Property) error {
	for _, p := range props {
		if p.DurabilityHint == nil {
			continue
		}
		if kind != "pipeline" {
			return fmt.Errorf("item %q: durabilityHint is only valid on kind=pipeline, not kind=%s", itemName, kind)
		}
		if !validDurabilityHints[p.DurabilityHint.Hint] {
			return fmt.Errorf("item %q: durabilityHint %q must be one of PERFORMANCE_OPTIMIZED, SURVIVABLE_NONATOMIC, MAX_SURVIVABILITY",
				itemName, p.DurabilityHint.Hint)
		}
	}
	return nil
}

// ---- Parameters ----

// Parameter defines one build parameter. A controller renders each entry
// as one Jenkins ParameterDefinition, whether it reaches the job through
// Item.Parameters or through ParametersProperty.
type Parameter struct {
	String       *StringParameter  `yaml:"string,omitempty"`
	Choice       *ChoiceParameter  `yaml:"choice,omitempty"`
	BooleanParam *BooleanParameter `yaml:"booleanParam,omitempty"`
}

// StringParameter is a string build parameter.
type StringParameter struct {
	Name         string `yaml:"name"`
	Description  string `yaml:"description,omitempty"`
	DefaultValue string `yaml:"defaultValue,omitempty"`
	Trim         bool   `yaml:"trim,omitempty"`
}

// ChoiceParameter is a dropdown build parameter.
type ChoiceParameter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description,omitempty"`
	Choices     []string `yaml:"choices"`
}

// BooleanParameter is a boolean build parameter.
type BooleanParameter struct {
	Name         string `yaml:"name"`
	Description  string `yaml:"description,omitempty"`
	DefaultValue bool   `yaml:"defaultValue,omitempty"`
}

// ---- Properties ----

// Property defines an item or folder property.
type Property struct {
	// Pipeline/freestyle properties
	BuildDiscarder          *BuildDiscarderProperty  `yaml:"buildDiscarder,omitempty"`
	DisableConcurrentBuilds *DisableConcurrentBuilds `yaml:"disableConcurrentBuilds,omitempty"`
	DisableResume           *DisableResume           `yaml:"disableResume,omitempty"`
	Parameters              *ParametersProperty      `yaml:"parameters,omitempty"`
	GithubProjectProperty   *GithubProjectProperty   `yaml:"githubProjectProperty,omitempty"`
	DurabilityHint          *DurabilityHint          `yaml:"durabilityHint,omitempty"`
	PreserveStashes         *PreserveStashes         `yaml:"preserveStashes,omitempty"`
	RateLimitBuilds         *RateLimitBuilds         `yaml:"rateLimitBuilds,omitempty"`
	PipelineTriggers        *PipelineTriggers        `yaml:"pipelineTriggers,omitempty"`
	// Folder properties
	EnvVars                   *EnvVarsProperty           `yaml:"envVars,omitempty"`
	FolderCredentialsProperty *FolderCredentialsProperty `yaml:"folderCredentialsProperty,omitempty"`
	FolderLibraries           *FolderLibrariesProperty   `yaml:"folderLibraries,omitempty"`
	ItemRestrictions          *ItemRestrictionsProperty  `yaml:"itemRestrictions,omitempty"`
	Kubernetes                *KubernetesFolderProperty  `yaml:"kubernetes,omitempty"`
}

// BuildDiscarderProperty for pipeline job properties format.
type BuildDiscarderProperty struct {
	Strategy LogRotatorStrategy `yaml:"strategy"`
}

// LogRotatorStrategy wraps a log rotator inside a strategy key.
type LogRotatorStrategy struct {
	LogRotator LogRotator `yaml:"logRotator"`
}

// BuildDiscarder for top-level freestyle-style use.
type BuildDiscarder struct {
	LogRotator LogRotator `yaml:"logRotator"`
}

// LogRotator configures build log retention.
type LogRotator struct {
	DaysToKeep         int `yaml:"daysToKeep,omitempty"`
	NumToKeep          int `yaml:"numToKeep,omitempty"`
	ArtifactDaysToKeep int `yaml:"artifactDaysToKeep,omitempty"`
	ArtifactNumToKeep  int `yaml:"artifactNumToKeep,omitempty"`
}

// DisableConcurrentBuilds prevents concurrent builds.
type DisableConcurrentBuilds struct{}

// DisableResume prevents pipeline resume.
type DisableResume struct{}

// ParametersProperty is a build parameter list nested under properties, an
// alternative to the item-level spelling (Item.Parameters). A pipeline or
// freeStyle job carries parameters from both sources at once. Every other
// kind rejects it.
type ParametersProperty struct {
	ParameterDefinitions []Parameter `yaml:"parameterDefinitions,omitempty"`
}

// GithubProjectProperty links a job to a GitHub project.
type GithubProjectProperty struct {
	ProjectURLStr string `yaml:"projectUrlStr"`
}

// DurabilityHint sets a pipeline job's flow durability from its first
// build. Hint must be one of PERFORMANCE_OPTIMIZED, SURVIVABLE_NONATOMIC,
// and MAX_SURVIVABILITY, the three names Jenkins core's FlowDurabilityHint
// defines. Item.Validate rejects any other value, for every controller.
type DurabilityHint struct {
	Hint string `yaml:"hint"`
}

// PreserveStashes preserves pipeline stashes.
type PreserveStashes struct {
	BuildCount int `yaml:"buildCount"`
}

// RateLimitBuilds throttles build rates.
type RateLimitBuilds struct {
	Throttle ThrottleConfig `yaml:"throttle"`
}

// ThrottleConfig configures the rate limit.
type ThrottleConfig struct {
	Throttle ThrottleValues `yaml:"throttle"`
}

// ThrottleValues sets count, boost, and duration for throttling.
type ThrottleValues struct {
	Count        int    `yaml:"count"`
	UserBoost    bool   `yaml:"userBoost,omitempty"`
	DurationName string `yaml:"durationName"`
}

// PipelineTriggers holds triggers as a pipeline property. The renderer
// merges them with the item-level triggers into one job property.
type PipelineTriggers struct {
	Triggers []Trigger `yaml:"triggers,omitempty"`
}

// ---- Folder Properties ----

// EnvVarsProperty sets environment variables on a folder.
type EnvVarsProperty struct {
	Vars map[string]string `yaml:"vars"`
}

// FolderCredentialsProperty defines folder-scoped credentials.
type FolderCredentialsProperty struct {
	FolderCredentials []FolderCredentialEntry `yaml:"folderCredentials,omitempty"`
}

// FolderCredentialEntry holds a list of credentials under a domain.
type FolderCredentialEntry struct {
	Credentials []FolderCredential `yaml:"credentials,omitempty"`
	Domain      DomainConfig       `yaml:"domain,omitempty"`
}

// DomainConfig for credential domains (typically `{}` for global).
type DomainConfig struct{}

// FolderCredential is a union of credential types.
type FolderCredential struct {
	UsernamePassword       *UsernamePasswordCredential       `yaml:"usernamePassword,omitempty"`
	SecretText             *SecretTextCredential             `yaml:"secretText,omitempty"`
	BasicSSHUserPrivateKey *BasicSSHUserPrivateKeyCredential `yaml:"basicSSHUserPrivateKey,omitempty"`
	File                   *FileCredential                   `yaml:"file,omitempty"`
	Certificate            *CertificateCredential            `yaml:"certificate,omitempty"`
	GitHubApp              *GitHubAppCredential              `yaml:"gitHubApp,omitempty"`
}

// GitHubAppCredential is a GitHub App credential. A multibranch source that
// uses it can publish GitHub Checks. PrivateKey is a PKCS#8 PEM key.
type GitHubAppCredential struct {
	ID          string `yaml:"id"`
	Scope       string `yaml:"scope,omitempty"`
	Description string `yaml:"description,omitempty"`
	AppID       string `yaml:"appID"`
	PrivateKey  string `yaml:"privateKey"`
	Owner       string `yaml:"owner,omitempty"`
	APIURI      string `yaml:"apiUri,omitempty"`
}

// FileCredential is a secret file credential. SecretBytes is the base64 of the file content.
type FileCredential struct {
	ID          string `yaml:"id"`
	Scope       string `yaml:"scope,omitempty"`
	Description string `yaml:"description,omitempty"`
	FileName    string `yaml:"fileName"`
	SecretBytes string `yaml:"secretBytes"`
}

// CertificateCredential is a PKCS12 certificate credential.
type CertificateCredential struct {
	ID             string         `yaml:"id"`
	Scope          string         `yaml:"scope,omitempty"`
	Description    string         `yaml:"description,omitempty"`
	Password       string         `yaml:"password,omitempty"`
	KeyStoreSource KeyStoreSource `yaml:"keyStoreSource"`
}

// KeyStoreSource is a union of keystore sources; only an uploaded keystore is supported.
type KeyStoreSource struct {
	Uploaded *UploadedKeyStoreSource `yaml:"uploaded,omitempty"`
}

// UploadedKeyStoreSource holds the base64 of the PKCS12 keystore bytes.
type UploadedKeyStoreSource struct {
	UploadedKeystore string `yaml:"uploadedKeystore"`
}

// BasicSSHUserPrivateKeyCredential is an SSH username and private key credential.
type BasicSSHUserPrivateKeyCredential struct {
	ID               string           `yaml:"id"`
	Scope            string           `yaml:"scope,omitempty"`
	Description      string           `yaml:"description,omitempty"`
	Username         string           `yaml:"username"`
	Passphrase       string           `yaml:"passphrase,omitempty"`
	PrivateKeySource PrivateKeySource `yaml:"privateKeySource"`
}

// PrivateKeySource is a union of private key sources; only direct entry is supported.
type PrivateKeySource struct {
	DirectEntry *DirectEntryPrivateKeySource `yaml:"directEntry,omitempty"`
}

// DirectEntryPrivateKeySource holds the private key text itself.
type DirectEntryPrivateKeySource struct {
	PrivateKey string `yaml:"privateKey"`
}

// UsernamePasswordCredential is a username/password credential.
type UsernamePasswordCredential struct {
	Password       string `yaml:"password"`
	Scope          string `yaml:"scope,omitempty"`
	Description    string `yaml:"description,omitempty"`
	ID             string `yaml:"id"`
	UsernameSecret bool   `yaml:"usernameSecret,omitempty"`
	Username       string `yaml:"username"`
}

// SecretTextCredential is a secret text credential.
type SecretTextCredential struct {
	ID          string `yaml:"id"`
	Scope       string `yaml:"scope,omitempty"`
	Description string `yaml:"description,omitempty"`
	Secret      string `yaml:"secret"`
}

// FolderLibrariesProperty defines shared pipeline libraries at folder level.
type FolderLibrariesProperty struct {
	Libraries []LibraryConfig `yaml:"libraries,omitempty"`
}

// LibraryConfig wraps a library configuration.
type LibraryConfig struct {
	LibraryConfiguration LibraryConfiguration `yaml:"libraryConfiguration"`
}

// LibraryConfiguration defines a shared library.
type LibraryConfiguration struct {
	Implicit             bool             `yaml:"implicit,omitempty"`
	AllowVersionOverride bool             `yaml:"allowVersionOverride,omitempty"`
	Retriever            LibraryRetriever `yaml:"retriever"`
	Name                 string           `yaml:"name"`
	DefaultVersion       string           `yaml:"defaultVersion,omitempty"`
	IncludeInChangesets  bool             `yaml:"includeInChangesets,omitempty"`
}

// LibraryRetriever defines how the library is retrieved. Exactly one of
// ModernSCM and LegacySCM is set.
type LibraryRetriever struct {
	ModernSCM *ModernSCMRetriever `yaml:"modernSCM,omitempty"`
	LegacySCM *LegacySCMRetriever `yaml:"legacySCM,omitempty"`
}

// ModernSCMRetriever retrieves a library from SCM.
type ModernSCMRetriever struct {
	SCM LibrarySCM `yaml:"scm"`
}

// LegacySCMRetriever retrieves a library through the legacy SCM retriever,
// which takes a plain git SCM block.
type LegacySCMRetriever struct {
	SCM SCM `yaml:"scm"`
}

// LibrarySCM defines the SCM for a shared library. Exactly one of GitHub
// and Git is set.
type LibrarySCM struct {
	GitHub *GitHubSCMSource `yaml:"github,omitempty"`
	Git    *GitSCMSource    `yaml:"git,omitempty"`
}

// LibraryCredentialRef names one credential a folder library needs and the
// credential kinds its source accepts.
type LibraryCredentialRef struct {
	ID    string
	Kinds []string
}

// RequiredPlugins returns the Jenkins plugin IDs that the library needs.
// pipeline-groovy-lib is always required.
func (c LibraryConfiguration) RequiredPlugins() []string {
	plugins := []string{"pipeline-groovy-lib"}
	if m := c.Retriever.ModernSCM; m != nil {
		if m.SCM.GitHub != nil {
			plugins = append(plugins, "github-branch-source")
		}
		if m.SCM.Git != nil {
			plugins = append(plugins, "git")
		}
	}
	if c.Retriever.LegacySCM != nil {
		plugins = append(plugins, "git")
	}
	return plugins
}

// CredentialRefs returns every non-empty credential ID the library names,
// with the credential kinds its source accepts. The kind strings match the
// credentialKind constants in internal/controller.
func (c LibraryConfiguration) CredentialRefs() []LibraryCredentialRef {
	const userPass = "usernamePassword"
	const sshKey = "sshPrivateKey"
	var refs []LibraryCredentialRef
	add := func(id string, kinds ...string) {
		if id != "" {
			refs = append(refs, LibraryCredentialRef{ID: id, Kinds: kinds})
		}
	}
	if m := c.Retriever.ModernSCM; m != nil {
		if m.SCM.GitHub != nil {
			add(m.SCM.GitHub.CredentialsID, userPass)
		}
		if m.SCM.Git != nil {
			add(m.SCM.Git.CredentialsID, userPass, sshKey)
		}
	}
	if l := c.Retriever.LegacySCM; l != nil && l.SCM.GitSCM != nil {
		for _, urc := range l.SCM.GitSCM.UserRemoteConfigs {
			add(urc.UserRemoteConfig.CredentialsID, userPass, sshKey)
		}
	}
	return refs
}

// ItemRestrictionsProperty restricts which item types can be created in a folder.
type ItemRestrictionsProperty struct {
	AllowedTypes []string `yaml:"allowedTypes"`
	Filter       bool     `yaml:"filter,omitempty"`
}

// KubernetesFolderProperty restricts which kubernetes-plugin clouds a
// folder's items may use.
type KubernetesFolderProperty struct {
	PermittedClouds []string `yaml:"permittedClouds"`
}

// ---- Groups (item-level RBAC) ----

// Group defines an RBAC group on an item.
type Group struct {
	Name    string  `yaml:"name"`
	Members Members `yaml:"members"`
	Roles   []Role  `yaml:"roles"`
}

// Members defines the members of a group.
type Members struct {
	Users          []string `yaml:"users,omitempty"`
	ExternalGroups []string `yaml:"external_groups,omitempty"`
}

// Role is a named RBAC role.
type Role struct {
	Name string `yaml:"name"`
}

// ---- Multibranch ----

// BranchSourceEntry wraps the official spec's branchSource format.
// In the CloudBees spec, sourcesList items have a branchSource key.
type BranchSourceEntry struct {
	BranchSource BranchSource `yaml:"branchSource,omitempty"`
	// Legacy format: bare git/github at top level
	Git    *GitSCMSource    `yaml:"git,omitempty"`
	GitHub *GitHubSCMSource `yaml:"github,omitempty"`
}

// BranchSource defines a branch source with source and strategy.
type BranchSource struct {
	Source   BranchSourceConfig   `yaml:"source"`
	Strategy BranchSourceStrategy `yaml:"strategy,omitempty"`
}

// BranchSourceConfig contains the actual SCM source.
type BranchSourceConfig struct {
	GitHub *GitHubSCMSource `yaml:"github,omitempty"`
	Git    *GitSCMSource    `yaml:"git,omitempty"`
	Gitea  *GiteaSCMSource  `yaml:"gitea,omitempty"`
}

// BranchSourceStrategy defines the branch discovery strategy.
type BranchSourceStrategy struct {
	AllBranchesSame *AllBranchesSame `yaml:"allBranchesSame,omitempty"`
}

// AllBranchesSame treats all branches the same.
type AllBranchesSame struct{}

// GitSCMSource for multibranch git source.
type GitSCMSource struct {
	Remote        string `yaml:"remote"`
	CredentialsID string `yaml:"credentialsId,omitempty"`
}

// GitHubSCMSource for multibranch GitHub org source (official spec format).
type GitHubSCMSource struct {
	RepoOwner       string        `yaml:"repoOwner"`
	ID              string        `yaml:"id,omitempty"`
	Repository      string        `yaml:"repository"`
	ConfiguredByURL bool          `yaml:"configuredByUrl,omitempty"`
	RepositoryURL   string        `yaml:"repositoryUrl,omitempty"`
	CredentialsID   string        `yaml:"credentialsId,omitempty"`
	Traits          []GitHubTrait `yaml:"traits,omitempty"`
}

// GiteaSCMSource for a multibranch Gitea or Forgejo repository source.
type GiteaSCMSource struct {
	ServerURL     string       `yaml:"serverUrl"`
	RepoOwner     string       `yaml:"repoOwner"`
	Repository    string       `yaml:"repository"`
	CredentialsID string       `yaml:"credentialsId,omitempty"`
	Traits        []GiteaTrait `yaml:"traits,omitempty"`
}

// GiteaTrait is a union of Gitea branch source discovery traits. Exactly one
// field is set in each entry.
type GiteaTrait struct {
	GiteaBranchDiscovery      *GiteaStrategyTrait `yaml:"giteaBranchDiscovery,omitempty"`
	GiteaPullRequestDiscovery *GiteaStrategyTrait `yaml:"giteaPullRequestDiscovery,omitempty"`
	GiteaForkDiscovery        *GiteaForkTrait     `yaml:"giteaForkDiscovery,omitempty"`
	GiteaTagDiscovery         *GiteaTagTrait      `yaml:"giteaTagDiscovery,omitempty"`
}

// GiteaStrategyTrait carries the strategy of a branch or pull request
// discovery trait. The values are 1 (exclude), 2 (only), or 3 (both).
type GiteaStrategyTrait struct {
	StrategyID int `yaml:"strategyId"`
}

// GiteaForkTrait is the fork pull request discovery trait.
type GiteaForkTrait struct {
	StrategyID int    `yaml:"strategyId"`
	Trust      string `yaml:"trust"`
}

// GiteaTagTrait is the tag discovery trait. It carries no settings.
type GiteaTagTrait struct{}

// GiteaNavigator scans a Gitea or Forgejo server for repositories.
type GiteaNavigator struct {
	ServerURL     string       `yaml:"serverUrl"`
	RepoOwner     string       `yaml:"repoOwner"`
	CredentialsID string       `yaml:"credentialsId,omitempty"`
	Traits        []GiteaTrait `yaml:"traits,omitempty"`
}

// GitHubTrait is a union of GitHub branch source discovery traits.
type GitHubTrait struct {
	GitHubBranchDiscovery      *GitHubBranchDiscovery      `yaml:"gitHubBranchDiscovery,omitempty"`
	GitHubPullRequestDiscovery *GitHubPullRequestDiscovery `yaml:"gitHubPullRequestDiscovery,omitempty"`
	GitHubForkDiscovery        *GitHubForkDiscovery        `yaml:"gitHubForkDiscovery,omitempty"`
	HeadWildcardFilter         *HeadWildcardFilter         `yaml:"headWildcardFilter,omitempty"`
	GitHubSshCheckout          *GitHubSshCheckout          `yaml:"gitHubSshCheckout,omitempty"`
}

// GitHubBranchDiscovery trait.
type GitHubBranchDiscovery struct {
	StrategyID int `yaml:"strategyId"`
}

// GitHubPullRequestDiscovery trait.
type GitHubPullRequestDiscovery struct {
	StrategyID int `yaml:"strategyId"`
}

// GitHubForkDiscovery trait.
type GitHubForkDiscovery struct {
	Trust      GitHubTrust `yaml:"trust"`
	StrategyID int         `yaml:"strategyId"`
}

// GitHubTrust for fork discovery.
type GitHubTrust struct {
	GitHubTrustEveryone *GitHubTrustEveryone `yaml:"gitHubTrustEveryone,omitempty"`
}

// GitHubTrustEveryone trusts all forks.
type GitHubTrustEveryone struct{}

// HeadWildcardFilter filters branches by name pattern.
type HeadWildcardFilter struct {
	Excludes string `yaml:"excludes,omitempty"`
	Includes string `yaml:"includes,omitempty"`
}

// GitHubSshCheckout uses SSH for checkout.
type GitHubSshCheckout struct {
	CredentialsID string `yaml:"credentialsId"`
}

// ProjectFactory for multibranch (and organization folders).
type ProjectFactory struct {
	MultiBranch    *MultiBranchProjectFactory    `yaml:"multiBranchProjectFactory,omitempty"`
	WorkflowBranch *WorkflowBranchProjectFactory `yaml:"workflowBranchProjectFactory,omitempty"`
	VarroaMarker   *VarroaMarkerProjectFactory   `yaml:"varroaMarkerProjectFactory,omitempty"`
}

// VarroaMarkerProjectFactory selects the plugin's branch project factory.
// The factory reads the marker file at a fixed path on each branch, so this
// type carries no field.
type VarroaMarkerProjectFactory struct{}

// MultiBranchProjectFactory configures the workflow factory script path.
type MultiBranchProjectFactory struct {
	ScriptPath string `yaml:"scriptPath"`
}

// WorkflowBranchProjectFactory is the official spec name for the multibranch factory.
type WorkflowBranchProjectFactory struct {
	ScriptPath string `yaml:"scriptPath"`
}

// OrphanedStrategy for handling deleted branches in multibranch pipelines.
type OrphanedStrategy struct {
	DaysToKeep int `yaml:"daysToKeep,omitempty"`
	NumToKeep  int `yaml:"numToKeep,omitempty"`
}

// ---- Enums ----

// SCMNavigator for organization folders.
type SCMNavigator struct {
	GitHub *GitHubNavigator `yaml:"githubNavigator,omitempty"`
	Git    *GitNavigator    `yaml:"gitNavigator,omitempty"`
	Gitea  *GiteaNavigator  `yaml:"giteaNavigator,omitempty"`
}

// GitHubNavigator scans a GitHub org for repositories.
type GitHubNavigator struct {
	RepoOwner     string `yaml:"repoOwner"`
	CredentialsID string `yaml:"credentialsId,omitempty"`
}

// GitNavigator scans a git server for repositories.
type GitNavigator struct {
	Remote        string `yaml:"remote"`
	CredentialsID string `yaml:"credentialsId,omitempty"`
}

// ---- Enums ----

// Remove strategy constants.
const (
	RemoveNone            = "none"
	RemoveSync            = "sync"
	RemoveRemoveSupported = "remove-supported"
	RemoveRemoveAll       = "remove-all"
)

// Validate checks the item's kind and required fields.
func (i *Item) Validate() error {
	if strings.TrimSpace(i.Name) == "" {
		return fmt.Errorf("item missing name")
	}
	if i.Kind == "" {
		return fmt.Errorf("item %q: kind is required", i.Name)
	}
	if err := validateTriggers(*i); err != nil {
		return err
	}
	// Validate every parameter a renderer will merge onto one job, together,
	// so a name repeated across the two spellings is caught here rather
	// than reaching Jenkins as two conflicting definitions.
	if err := validateParameters(i.Name, i.Kind, collectParameters(*i)); err != nil {
		return err
	}
	if err := validateDurabilityHints(i.Name, i.Kind, i.Properties); err != nil {
		return err
	}
	if err := validateFolderLibraries(*i); err != nil {
		return err
	}
	switch i.Kind {
	case "folder", "freeStyle":
		return nil
	case "pipeline":
		if i.Definition == nil {
			return fmt.Errorf("item %q: kind=pipeline requires definition.cpsScmFlowDefinition, definition.script, or definition.varroaTemplate", i.Name)
		}
		set := 0
		if i.Definition.CpsScmFlowDefinition != nil {
			set++
		}
		if i.Definition.Script != "" {
			set++
		}
		if i.Definition.VarroaTemplate != nil {
			set++
		}
		if set != 1 {
			return fmt.Errorf("item %q: kind=pipeline requires exactly one of definition.cpsScmFlowDefinition, definition.script, and definition.varroaTemplate", i.Name)
		}
		if i.Definition.CpsScmFlowDefinition != nil {
			def := i.Definition.CpsScmFlowDefinition
			if def.SCM.GitSCM == nil {
				return fmt.Errorf("item %q: kind=pipeline requires definition.cpsScmFlowDefinition.scm.gitSCM", i.Name)
			}
		}
		return nil
	case "multibranch":
		if len(i.SourcesList) == 0 {
			return fmt.Errorf("item %q: kind=multibranch requires at least one branch source in sourcesList", i.Name)
		}
		for _, entry := range i.SourcesList {
			if g := entry.BranchSource.Source.Gitea; g != nil {
				if err := validateGiteaServer(i.Name, g.ServerURL, g.RepoOwner); err != nil {
					return err
				}
				if strings.TrimSpace(g.Repository) == "" {
					return fmt.Errorf("item %q: gitea source requires repository", i.Name)
				}
				if err := validateGiteaTraits(i.Name, g.Traits); err != nil {
					return err
				}
			}
		}
		return nil
	case "organizationFolder":
		if len(i.Navigators) == 0 {
			return fmt.Errorf("item %q: kind=organizationFolder requires at least one navigator", i.Name)
		}
		for _, nav := range i.Navigators {
			if g := nav.Gitea; g != nil {
				if err := validateGiteaServer(i.Name, g.ServerURL, g.RepoOwner); err != nil {
					return err
				}
				if err := validateGiteaTraits(i.Name, g.Traits); err != nil {
					return err
				}
			}
		}
		return nil
	default:
		return fmt.Errorf("item %q: unknown kind %q", i.Name, i.Kind)
	}
}

// giteaServerURLPattern accepts an http or https URL without spaces or '$'.
var giteaServerURLPattern = regexp.MustCompile(`^https?://[^\s$]+$`)

// validateGiteaServer checks the server URL and the owner of a Gitea source
// or navigator.
func validateGiteaServer(itemName, serverURL, repoOwner string) error {
	if !giteaServerURLPattern.MatchString(serverURL) {
		return fmt.Errorf("item %q: gitea serverUrl %q must be an http or https URL", itemName, serverURL)
	}
	if strings.TrimSpace(repoOwner) == "" {
		return fmt.Errorf("item %q: gitea source requires repoOwner", itemName)
	}
	return nil
}

// validateGiteaTraits checks each discovery trait. A trait entry sets exactly
// one key, and each kind appears at most once.
func validateGiteaTraits(itemName string, traits []GiteaTrait) error {
	seen := map[string]bool{}
	for _, t := range traits {
		set := 0
		kind := ""
		if b := t.GiteaBranchDiscovery; b != nil {
			set++
			kind = "giteaBranchDiscovery"
			if err := validateGiteaStrategy(itemName, kind, b.StrategyID); err != nil {
				return err
			}
		}
		if p := t.GiteaPullRequestDiscovery; p != nil {
			set++
			kind = "giteaPullRequestDiscovery"
			if err := validateGiteaStrategy(itemName, kind, p.StrategyID); err != nil {
				return err
			}
		}
		if f := t.GiteaForkDiscovery; f != nil {
			set++
			kind = "giteaForkDiscovery"
			if err := validateGiteaStrategy(itemName, kind, f.StrategyID); err != nil {
				return err
			}
			switch f.Trust {
			case "nobody", "contributors", "everyone":
			default:
				return fmt.Errorf("item %q: giteaForkDiscovery trust %q must be nobody, contributors, or everyone", itemName, f.Trust)
			}
		}
		if t.GiteaTagDiscovery != nil {
			set++
			kind = "giteaTagDiscovery"
		}
		if set != 1 {
			return fmt.Errorf("item %q: each gitea trait entry must set exactly one trait", itemName)
		}
		if seen[kind] {
			return fmt.Errorf("item %q: gitea trait %s is declared more than once", itemName, kind)
		}
		seen[kind] = true
	}
	return nil
}

// validateGiteaStrategy checks the strategy id of a Gitea discovery trait.
func validateGiteaStrategy(itemName, kind string, id int) error {
	if id < 1 || id > 3 {
		return fmt.Errorf("item %q: %s strategyId %d must be 1, 2, or 3", itemName, kind, id)
	}
	return nil
}

// EffectiveGiteaTraits returns the discovery traits a Gitea source or
// navigator renders. An empty list selects the defaults: branch discovery
// (1), origin pull request discovery (1), and fork pull request discovery
// (1) that trusts contributors.
func EffectiveGiteaTraits(t []GiteaTrait) []GiteaTrait {
	if len(t) > 0 {
		return t
	}
	return []GiteaTrait{
		{GiteaBranchDiscovery: &GiteaStrategyTrait{StrategyID: 1}},
		{GiteaPullRequestDiscovery: &GiteaStrategyTrait{StrategyID: 1}},
		{GiteaForkDiscovery: &GiteaForkTrait{StrategyID: 1, Trust: "contributors"}},
	}
}

// validateFolderLibraries checks every folder library of an item before any
// renderer runs. A malformed retriever then holds the tenant with a reason.
func validateFolderLibraries(i Item) error {
	for _, p := range i.Properties {
		if p.FolderLibraries == nil {
			continue
		}
		if i.Kind != "folder" {
			return fmt.Errorf("item %q: folderLibraries is only valid on kind=folder, not kind=%s", i.Name, i.Kind)
		}
		for _, lc := range p.FolderLibraries.Libraries {
			if err := validateLibrary(i.Name, lc.LibraryConfiguration); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateLibrary checks one folder library. Every message names the item
// and the library.
func validateLibrary(itemName string, c LibraryConfiguration) error {
	prefix := fmt.Sprintf("item %q: folder library %q: ", itemName, c.Name)
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("item %q: a folder library has no name", itemName)
	}
	r := c.Retriever
	if (r.ModernSCM == nil) == (r.LegacySCM == nil) {
		return fmt.Errorf("%sretriever must set exactly one of modernSCM and legacySCM", prefix)
	}
	if m := r.ModernSCM; m != nil {
		s := m.SCM
		if (s.GitHub == nil) == (s.Git == nil) {
			return fmt.Errorf("%smodernSCM.scm must set exactly one of github and git", prefix)
		}
		if gh := s.GitHub; gh != nil {
			if gh.ConfiguredByURL {
				if gh.RepositoryURL == "" {
					return fmt.Errorf("%smodernSCM.scm.github with configuredByUrl requires repositoryUrl", prefix)
				}
			} else if gh.RepoOwner == "" || gh.Repository == "" {
				return fmt.Errorf("%smodernSCM.scm.github requires repoOwner and repository", prefix)
			}
		}
		if g := s.Git; g != nil && g.Remote == "" {
			return fmt.Errorf("%smodernSCM.scm.git requires remote", prefix)
		}
	}
	if l := r.LegacySCM; l != nil {
		if l.SCM.GitSCM == nil || len(l.SCM.GitSCM.UserRemoteConfigs) == 0 {
			return fmt.Errorf("%slegacySCM.scm requires gitSCM with at least one userRemoteConfig", prefix)
		}
		for _, urc := range l.SCM.GitSCM.UserRemoteConfigs {
			if urc.UserRemoteConfig.URL == "" {
				return fmt.Errorf("%slegacySCM.scm.gitSCM userRemoteConfig requires url", prefix)
			}
		}
	}
	if c.DefaultVersion == "" && !c.AllowVersionOverride {
		return fmt.Errorf("%ssets neither defaultVersion nor allowVersionOverride, so no @Library line can load it", prefix)
	}
	return nil
}

// IsFolder returns true if the item is a folder (can contain nested items).
func (i *Item) IsFolder() bool {
	return i.Kind == "folder" || i.Kind == "organizationFolder"
}

// JenkinsClass returns the Java class name for creating this item type.
func (i *Item) JenkinsClass() string {
	switch i.Kind {
	case "folder":
		return "com.cloudbees.hudson.plugins.folder.Folder"
	case "freeStyle":
		return "hudson.model.FreeStyleProject"
	case "pipeline":
		return "org.jenkinsci.plugins.workflow.job.WorkflowJob"
	case "multibranch":
		return "org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject"
	case "organizationFolder":
		return "jenkins.branch.OrganizationFolder"
	default:
		return ""
	}
}
