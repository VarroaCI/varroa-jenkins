package items

import (
	"fmt"
	"strings"
)

// ---- Top-level manifest ----

// Manifest is the top-level structure of an items.yaml file.
type Manifest struct {
	RemoveStrategy *RemoveStrategy `yaml:"removeStrategy,omitempty"`
	Root           string          `yaml:"root,omitempty"`
	Items          []Item          `yaml:"items"`
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

	// Shared across multiple kinds
	Properties     []Property      `yaml:"properties,omitempty"`
	BuildDiscarder *BuildDiscarder `yaml:"buildDiscarder,omitempty"`

	// Pipeline-specific
	Definition      *PipelineDefinition `yaml:"definition,omitempty"`
	ConcurrentBuild bool                `yaml:"concurrentBuild,omitempty"`
	ResumeBlocked   bool                `yaml:"resumeBlocked,omitempty"`
	QuietPeriod     int                 `yaml:"quietPeriod,omitempty"`

	// FreeStyle-specific
	SCM                              *SCM        `yaml:"scm,omitempty"`
	Builders                         []Builder   `yaml:"builders,omitempty"`
	PublishersList                   []Publisher `yaml:"publishersList,omitempty"`
	Triggers                         []Trigger   `yaml:"triggers,omitempty"`
	Parameters                       []Parameter `yaml:"parameters,omitempty"`
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
	CpsScmFlowDefinition *CpsScmFlowDefinition `yaml:"cpsScmFlowDefinition,omitempty"`
	Script               string                `yaml:"script,omitempty"`
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

// Trigger defines a build trigger.
type Trigger struct {
	PollSCM *PollSCMTrigger `yaml:"pollSCM,omitempty"`
	Cron    *CronTrigger    `yaml:"cron,omitempty"`
}

// PollSCMTrigger polls SCM on a schedule.
type PollSCMTrigger struct {
	IgnorePostCommitHooks bool   `yaml:"ignorePostCommitHooks,omitempty"`
	ScmpollSpec           string `yaml:"scmpoll_spec,omitempty"`
}

// CronTrigger triggers builds on a cron schedule.
type CronTrigger struct {
	Spec string `yaml:"spec"`
}

// ---- Parameters ----

// Parameter defines a build parameter.
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

// ParametersProperty defines build parameters as a property.
type ParametersProperty struct {
	ParameterDefinitions []Parameter `yaml:"parameterDefinitions,omitempty"`
}

// GithubProjectProperty links a job to a GitHub project.
type GithubProjectProperty struct {
	ProjectURLStr string `yaml:"projectUrlStr"`
}

// DurabilityHint sets pipeline durability.
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

// PipelineTriggers holds triggers as a pipeline property.
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
	UsernamePassword *UsernamePasswordCredential `yaml:"usernamePassword,omitempty"`
	SecretText       *SecretTextCredential       `yaml:"secretText,omitempty"`
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
	IncludeInChangesets  bool             `yaml:"includeInChangesets,omitempty"`
}

// LibraryRetriever defines how the library is retrieved.
type LibraryRetriever struct {
	ModernSCM *ModernSCMRetriever `yaml:"modernSCM,omitempty"`
}

// ModernSCMRetriever retrieves a library from SCM.
type ModernSCMRetriever struct {
	SCM LibrarySCM `yaml:"scm"`
}

// LibrarySCM defines the SCM for a shared library.
type LibrarySCM struct {
	GitHub *LibraryGitHubSCM `yaml:"github,omitempty"`
}

// LibraryGitHubSCM for library retrieval from GitHub.
type LibraryGitHubSCM struct {
	RepoOwner       string        `yaml:"repoOwner"`
	ID              string        `yaml:"id,omitempty"`
	Repository      string        `yaml:"repository"`
	ConfiguredByURL bool          `yaml:"configuredByUrl,omitempty"`
	RepositoryURL   string        `yaml:"repositoryUrl,omitempty"`
	Traits          []GitHubTrait `yaml:"traits,omitempty"`
}

// ItemRestrictionsProperty restricts which item types can be created in a folder.
type ItemRestrictionsProperty struct {
	AllowedTypes []string `yaml:"allowedTypes"`
	Filter       bool     `yaml:"filter,omitempty"`
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
}

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
	switch i.Kind {
	case "":
		return fmt.Errorf("item %q: kind is required", i.Name)
	case "folder", "freeStyle":
		return nil
	case "pipeline":
		if i.Definition == nil || (i.Definition.CpsScmFlowDefinition == nil && i.Definition.Script == "") {
			return fmt.Errorf("item %q: kind=pipeline requires definition.cpsScmFlowDefinition or definition.script", i.Name)
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
		return nil
	case "organizationFolder":
		if len(i.Navigators) == 0 {
			return fmt.Errorf("item %q: kind=organizationFolder requires at least one navigator", i.Name)
		}
		return nil
	default:
		return fmt.Errorf("item %q: unknown kind %q", i.Name, i.Kind)
	}
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
