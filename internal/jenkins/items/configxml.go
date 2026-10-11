package items

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// GenerateConfigXML converts an Item into its Jenkins config.xml representation.
func GenerateConfigXML(item Item) (string, error) {
	var cfg xmlConfig
	var err error
	switch item.Kind {
	case "folder":
		cfg, err = folderConfigXML(item)
	case "freeStyle":
		cfg = freeStyleConfigXML(item)
	case "pipeline":
		cfg = pipelineConfigXML(item)
	case "multibranch":
		cfg = multibranchConfigXML(item)
	case "organizationFolder":
		cfg = orgFolderConfigXML(item)
	default:
		return "", fmt.Errorf("unknown kind %q", item.Kind)
	}

	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString("\n")
	enc := xml.NewEncoder(&b)
	enc.Indent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		return "", fmt.Errorf("encode config.xml for %s: %w", item.Name, err)
	}
	return b.String(), nil
}

type xmlConfig interface{}

// varroaMiteAuthPlugin is the short name of the plugin providing every
// varroa-prefixed class this file renders.
const varroaMiteAuthPlugin = "varroa-mite-auth"

// varroaTemplateValuesJSON renders template values as compact JSON text for
// the values element of a rendered VarroaTemplateFlowDefinition. Values
// decodes from YAML into plain Go values, so it always encodes; a nil map
// renders as an empty object.
func varroaTemplateValuesJSON(values map[string]any) string {
	if values == nil {
		values = map[string]any{}
	}
	b, err := json.Marshal(values)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ---- Folder ----

type folderXML struct {
	XMLName     xml.Name             `xml:"com.cloudbees.hudson.plugins.folder.Folder"`
	Plugin      string               `xml:"plugin,attr,omitempty"`
	DisplayName string               `xml:"displayName,omitempty"`
	Description string               `xml:"description,omitempty"`
	Properties  *folderPropertiesXML `xml:"properties"`
}

func folderConfigXML(item Item) (xmlConfig, error) {
	props, err := buildFolderProperties(item)
	if err != nil {
		return nil, err
	}
	return &folderXML{
		Plugin:      "cloudbees-folder",
		DisplayName: item.DisplayName,
		Description: item.Description,
		Properties:  props,
	}, nil
}

// ---- FreeStyle ----

type freeStyleXML struct {
	XMLName         xml.Name              `xml:"project"`
	DisplayName     string                `xml:"displayName,omitempty"`
	Description     string                `xml:"description,omitempty"`
	Disabled        bool                  `xml:"disabled,omitempty"`
	ConcurrentBuild bool                  `xml:"concurrentBuild,omitempty"`
	CustomWorkspace string                `xml:"customWorkspace,omitempty"`
	QuietPeriod     int                   `xml:"quietPeriod,omitempty"`
	AssignedNode    string                `xml:"assignedNode,omitempty"`
	SCM             *jenkinsScmXML        `xml:"scm,omitempty"`
	Builders        *jenkinsBuildersXML   `xml:"builders,omitempty"`
	Publishers      *jenkinsPublishersXML `xml:"publishers,omitempty"`
	BuildDiscarder  *jenkinsDiscarderXML  `xml:"buildDiscarder,omitempty"`
	Triggers        *jenkinsTriggersXML   `xml:"triggers,omitempty"`
	Properties      *jenkinsParamsPropXML `xml:"properties,omitempty"`
}

type jenkinsScmXML struct {
	Class                             string                `xml:"class,attr"`
	Plugin                            string                `xml:"plugin,attr,omitempty"`
	ConfigVersion                     int                   `xml:"configVersion,omitempty"`
	UserRemoteConfigs                 *userRemoteConfigsXML `xml:"userRemoteConfigs,omitempty"`
	Branches                          *branchesXML          `xml:"branches,omitempty"`
	DoGenerateSubmoduleConfigurations bool                  `xml:"doGenerateSubmoduleConfigurations,omitempty"`
	Extensions                        *scmExtensionsXML     `xml:"extensions,omitempty"`
}

type userRemoteConfigsXML struct {
	Configs []*userRemoteConfigXML `xml:"hudson.plugins.git.UserRemoteConfig"`
}

type userRemoteConfigXML struct {
	URL           string `xml:"url"`
	Name          string `xml:"name,omitempty"`
	CredentialsID string `xml:"credentialsId,omitempty"`
}

type branchesXML struct {
	Specs []*branchSpecXML `xml:"hudson.plugins.git.BranchSpec"`
}

type branchSpecXML struct {
	Name string `xml:"name"`
}

type scmExtensionsXML struct {
	CleanBeforeCheckout *cleanBeforeXML       `xml:"hudson.plugins.git.extensions.impl.CleanBeforeCheckout,omitempty"`
	CleanCheckout       *cleanCheckoutXML     `xml:"hudson.plugins.git.extensions.impl.CleanCheckout,omitempty"`
	CheckoutOption      *checkoutOptionXML    `xml:"hudson.plugins.git.extensions.impl.CheckoutOption,omitempty"`
	CloneOption         *cloneOptionXML       `xml:"hudson.plugins.git.extensions.impl.CloneOption,omitempty"`
	RelativeTargetDir   *relativeTargetDirXML `xml:"hudson.plugins.git.extensions.impl.RelativeTargetDirectory,omitempty"`
	PruneStaleBranch    *pruneStaleXML        `xml:"hudson.plugins.git.extensions.impl.PruneStaleBranch,omitempty"`
	WipeWorkspace       *wipeWorkspaceXML     `xml:"hudson.plugins.git.extensions.impl.WipeWorkspace,omitempty"`
}

type cleanBeforeXML struct {
	DeleteUntrackedNestedRepositories bool `xml:"deleteUntrackedNestedRepositories"`
}

type cleanCheckoutXML struct {
	DeleteUntrackedNestedRepositories bool `xml:"deleteUntrackedNestedRepositories"`
}

type checkoutOptionXML struct {
	Timeout int `xml:"timeout"`
}

type cloneOptionXML struct {
	Reference    string `xml:"reference,omitempty"`
	NoTags       bool   `xml:"noTags"`
	HonorRefspec bool   `xml:"honorRefspec"`
	Shallow      bool   `xml:"shallow"`
	Timeout      int    `xml:"timeout,omitempty"`
}

type relativeTargetDirXML struct {
	RelativeTargetDir string `xml:"relativeTargetDir"`
}

type pruneStaleXML struct{}

type wipeWorkspaceXML struct{}

type jenkinsBuildersXML struct {
	Shell []*jenkinsShellXML `xml:"hudson.tasks.Shell,omitempty"`
	Maven []*jenkinsMavenXML `xml:"hudson.tasks.Maven,omitempty"`
}

type jenkinsShellXML struct {
	Command string `xml:"command"`
}

type jenkinsMavenXML struct {
	Targets              string `xml:"targets"`
	UsePrivateRepository bool   `xml:"usePrivateRepository,omitempty"`
}

type jenkinsPublishersXML struct {
	ArchiveArtifacts    []*jenkinsArchiveArtifactsXML `xml:"hudson.tasks.ArtifactArchiver,omitempty"`
	JUnitResultArchiver []*jenkinsJUnitXML            `xml:"hudson.tasks.junit.JUnitResultArchiver,omitempty"`
	Mailer              []*jenkinsMailerXML           `xml:"hudson.tasks.Mailer,omitempty"`
}

type jenkinsArchiveArtifactsXML struct {
	Artifacts         string `xml:"artifacts"`
	AllowEmptyArchive bool   `xml:"allowEmptyArchive,omitempty"`
	OnlyIfSuccessful  bool   `xml:"onlyIfSuccessful,omitempty"`
	Fingerprint       bool   `xml:"fingerprint,omitempty"`
	DefaultExcludes   bool   `xml:"defaultExcludes,omitempty"`
}

type jenkinsJUnitXML struct {
	TestResults       string  `xml:"testResults"`
	AllowEmptyResults bool    `xml:"allowEmptyResults,omitempty"`
	KeepLongStdio     bool    `xml:"keepLongStdio,omitempty"`
	HealthScaleFactor float64 `xml:"healthScaleFactor,omitempty"`
}

type jenkinsMailerXML struct {
	Recipients               string `xml:"recipients"`
	NotifyEveryUnstableBuild bool   `xml:"notifyEveryUnstableBuild,omitempty"`
	SendToIndividuals        bool   `xml:"sendToIndividuals,omitempty"`
}

type jenkinsDiscarderXML struct {
	Class              string `xml:"class,attr"`
	DaysToKeep         int    `xml:"daysToKeep,omitempty"`
	NumToKeep          int    `xml:"numToKeep,omitempty"`
	ArtifactDaysToKeep int    `xml:"artifactDaysToKeep,omitempty"`
	ArtifactNumToKeep  int    `xml:"artifactNumToKeep,omitempty"`
}

// jenkinsTriggersXML is the triggers list of a freeStyle job and of a
// pipeline job's PipelineTriggersJobProperty. Jenkins reads the same
// element names in both places.
type jenkinsTriggersXML struct {
	PollSCM        []*jenkinsPollSCMXML        `xml:"hudson.triggers.SCMTrigger,omitempty"`
	Cron           []*jenkinsCronXML           `xml:"hudson.triggers.TimerTrigger,omitempty"`
	GitHubPush     []*jenkinsGitHubPushXML     `xml:"com.cloudbees.jenkins.GitHubPushTrigger,omitempty"`
	VarroaUpstream []*jenkinsVarroaUpstreamXML `xml:"org.jenkinsci.plugins.varroamiteauth.VarroaUpstreamTrigger,omitempty"`
}

type jenkinsPollSCMXML struct {
	Spec                  string `xml:"spec"`
	IgnorePostCommitHooks bool   `xml:"ignorePostCommitHooks"`
}

type jenkinsCronXML struct {
	Spec string `xml:"spec"`
}

// jenkinsGitHubPushXML renders a githubPush trigger. Jenkins writes an
// empty spec element for this trigger, so the renderer writes one too.
type jenkinsGitHubPushXML struct {
	Plugin string `xml:"plugin,attr"`
	Spec   string `xml:"spec"`
}

// githubPlugin is the plugin that owns GitHubPushTrigger.
const githubPlugin = "github"

// buildTriggersXML converts trigger declarations into the triggers list.
// It returns nil for an empty list, so the caller omits the element.
func buildTriggersXML(triggers []Trigger) *jenkinsTriggersXML {
	if len(triggers) == 0 {
		return nil
	}
	t := &jenkinsTriggersXML{}
	for _, tr := range triggers {
		if tr.PollSCM != nil {
			t.PollSCM = append(t.PollSCM, &jenkinsPollSCMXML{
				Spec:                  tr.PollSCM.EffectiveSpec(),
				IgnorePostCommitHooks: tr.PollSCM.IgnorePostCommitHooks,
			})
		}
		if tr.Cron != nil {
			t.Cron = append(t.Cron, &jenkinsCronXML{Spec: tr.Cron.Spec})
		}
		if tr.GitHubPush != nil {
			t.GitHubPush = append(t.GitHubPush, &jenkinsGitHubPushXML{Plugin: githubPlugin})
		}
		if tr.VarroaUpstream != nil {
			result := tr.VarroaUpstream.Result
			if result == "" {
				result = varroaUpstreamDefaultResult
			}
			t.VarroaUpstream = append(t.VarroaUpstream, &jenkinsVarroaUpstreamXML{
				Plugin: varroaMiteAuthPlugin,
				Tenant: tr.VarroaUpstream.Tenant,
				Job:    tr.VarroaUpstream.Job,
				Result: result,
			})
		}
	}
	return t
}

// jenkinsVarroaUpstreamXML renders a varroaUpstream trigger. Its element
// name is the trigger's full class name, design.md D9. Result always
// carries an explicit value: the renderer applies the SUCCESS default
// before it reaches this struct. Spec is always an empty element: Jenkins
// core's Trigger.readResolve parses spec with no null check, so Jenkins
// drops a trigger that has no spec element when it loads the job.
type jenkinsVarroaUpstreamXML struct {
	Plugin string `xml:"plugin,attr"`
	Spec   string `xml:"spec"`
	Tenant string `xml:"tenant"`
	Job    string `xml:"job"`
	Result string `xml:"result"`
}

type jenkinsParamsPropXML struct {
	ParamsDefProp *paramsDefPropXML `xml:"hudson.model.ParametersDefinitionProperty,omitempty"`
}

type paramsDefPropXML struct {
	ParameterDefs []jenkinsParamXML `xml:"parameterDefinitions"`
}

// MarshalXML writes the parameterDefinitions wrapper Jenkins core expects,
// then each parameter definition under its own class name. A plain struct
// tag cannot express this: a definition's XMLName value, set to its class
// name, takes precedence over the wrapping field's own tag.
func (p paramsDefPropXML) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	wrap := xml.StartElement{Name: xml.Name{Local: "parameterDefinitions"}}
	if err := e.EncodeToken(wrap); err != nil {
		return err
	}
	for _, def := range p.ParameterDefs {
		if err := e.Encode(def); err != nil {
			return err
		}
	}
	if err := e.EncodeToken(wrap.End()); err != nil {
		return err
	}
	return e.EncodeToken(start.End())
}

type jenkinsParamXML struct {
	XMLName      xml.Name
	Name         string           `xml:"name"`
	Description  string           `xml:"description,omitempty"`
	DefaultValue string           `xml:"defaultValue,omitempty"`
	Trim         bool             `xml:"trim,omitempty"`
	Choices      *paramChoicesXML `xml:"choices,omitempty"`
}

// paramChoicesXML mirrors the java.util.Arrays$ArrayList shape XStream
// gives a choice parameter's choices field, so a config.xml push round
// trips through Jenkins core the same way a UI-saved job does.
type paramChoicesXML struct {
	Class string          `xml:"class,attr"`
	Array *stringArrayXML `xml:"a"`
}

type stringArrayXML struct {
	Class   string   `xml:"class,attr"`
	Strings []string `xml:"string"`
}

// Jenkins core class names for the three parameter kinds this DSL models.
const (
	stringParamClass  = "hudson.model.StringParameterDefinition"
	choiceParamClass  = "hudson.model.ChoiceParameterDefinition"
	booleanParamClass = "hudson.model.BooleanParameterDefinition"
)

// collectParameters gathers an item's build parameters from both spellings:
// the item's own flat list and its parameters property. A renderer folds
// both sources into one ParametersDefinitionProperty, the same merge the
// pipelines API applies when it lists a job's parameters.
func collectParameters(item Item) []Parameter {
	params := append([]Parameter(nil), item.Parameters...)
	for _, prop := range item.Properties {
		if prop.Parameters != nil {
			params = append(params, prop.Parameters.ParameterDefinitions...)
		}
	}
	return params
}

// convertParameters renders a collected parameter list into the
// ParameterDefinition elements a ParametersDefinitionProperty carries. A
// caller has already run validateParameters, so every entry sets exactly
// one of String, Choice, and BooleanParam.
func convertParameters(params []Parameter) []jenkinsParamXML {
	out := make([]jenkinsParamXML, 0, len(params))
	for _, p := range params {
		switch {
		case p.String != nil:
			out = append(out, jenkinsParamXML{
				XMLName:      xml.Name{Local: stringParamClass},
				Name:         p.String.Name,
				Description:  p.String.Description,
				DefaultValue: p.String.DefaultValue,
				Trim:         p.String.Trim,
			})
		case p.Choice != nil:
			out = append(out, jenkinsParamXML{
				XMLName:     xml.Name{Local: choiceParamClass},
				Name:        p.Choice.Name,
				Description: p.Choice.Description,
				Choices: &paramChoicesXML{
					Class: "java.util.Arrays$ArrayList",
					Array: &stringArrayXML{Class: "string-array", Strings: p.Choice.Choices},
				},
			})
		case p.BooleanParam != nil:
			out = append(out, jenkinsParamXML{
				XMLName:      xml.Name{Local: booleanParamClass},
				Name:         p.BooleanParam.Name,
				Description:  p.BooleanParam.Description,
				DefaultValue: strconv.FormatBool(p.BooleanParam.DefaultValue),
			})
		}
	}
	return out
}

// ---- Conversion helpers ----

func freeStyleConfigXML(item Item) xmlConfig {
	f := &freeStyleXML{
		DisplayName:     item.DisplayName,
		Description:     item.Description,
		Disabled:        item.Disabled,
		ConcurrentBuild: item.ConcurrentBuild,
		CustomWorkspace: item.CustomWorkspace,
		QuietPeriod:     item.QuietPeriod,
		AssignedNode:    item.Label,
	}
	if item.SCM != nil && item.SCM.GitSCM != nil {
		f.SCM = convertGitSCM(item.SCM.GitSCM)
	}
	if len(item.Builders) > 0 {
		b := &jenkinsBuildersXML{}
		for _, builder := range item.Builders {
			if builder.Shell != nil {
				b.Shell = append(b.Shell, &jenkinsShellXML{Command: builder.Shell.Command})
			}
			if builder.Maven != nil {
				b.Maven = append(b.Maven, &jenkinsMavenXML{
					Targets:              builder.Maven.Targets,
					UsePrivateRepository: builder.Maven.UsePrivateRepository,
				})
			}
		}
		f.Builders = b
	}
	if len(item.PublishersList) > 0 {
		p := &jenkinsPublishersXML{}
		for _, pub := range item.PublishersList {
			if pub.ArchiveArtifacts != nil {
				p.ArchiveArtifacts = append(p.ArchiveArtifacts, &jenkinsArchiveArtifactsXML{
					Artifacts:         pub.ArchiveArtifacts.Artifacts,
					AllowEmptyArchive: pub.ArchiveArtifacts.AllowEmptyArchive,
					OnlyIfSuccessful:  pub.ArchiveArtifacts.OnlyIfSuccessful,
					Fingerprint:       pub.ArchiveArtifacts.Fingerprint,
					DefaultExcludes:   pub.ArchiveArtifacts.DefaultExcludes,
				})
			}
			if pub.JUnitResultArchiver != nil {
				p.JUnitResultArchiver = append(p.JUnitResultArchiver, &jenkinsJUnitXML{
					TestResults:       pub.JUnitResultArchiver.TestResults,
					AllowEmptyResults: pub.JUnitResultArchiver.AllowEmptyResults,
					KeepLongStdio:     pub.JUnitResultArchiver.KeepLongStdio,
					HealthScaleFactor: pub.JUnitResultArchiver.HealthScaleFactor,
				})
			}
			if pub.Mailer != nil {
				p.Mailer = append(p.Mailer, &jenkinsMailerXML{
					Recipients:               pub.Mailer.Recipients,
					NotifyEveryUnstableBuild: pub.Mailer.NotifyEveryUnstableBuild,
					SendToIndividuals:        pub.Mailer.SendToIndividuals,
				})
			}
		}
		f.Publishers = p
	}
	if item.BuildDiscarder != nil {
		f.BuildDiscarder = &jenkinsDiscarderXML{
			Class:              "hudson.tasks.LogRotator",
			DaysToKeep:         item.BuildDiscarder.LogRotator.DaysToKeep,
			NumToKeep:          item.BuildDiscarder.LogRotator.NumToKeep,
			ArtifactDaysToKeep: item.BuildDiscarder.LogRotator.ArtifactDaysToKeep,
			ArtifactNumToKeep:  item.BuildDiscarder.LogRotator.ArtifactNumToKeep,
		}
	}
	f.Triggers = buildTriggersXML(item.Triggers)
	if params := collectParameters(item); len(params) > 0 {
		f.Properties = &jenkinsParamsPropXML{
			ParamsDefProp: &paramsDefPropXML{ParameterDefs: convertParameters(params)},
		}
	}
	return f
}

// convertGitSCM converts the CloudBees-style GitSCM to Jenkins config.xml format.
func convertGitSCM(git *GitSCM) *jenkinsScmXML {
	if git == nil {
		return nil
	}
	scm := &jenkinsScmXML{
		Class:                             "hudson.plugins.git.GitSCM",
		Plugin:                            "git",
		ConfigVersion:                     2,
		DoGenerateSubmoduleConfigurations: git.DoGenerateSubmoduleConfigurations,
	}
	if len(git.UserRemoteConfigs) > 0 {
		var configs []*userRemoteConfigXML
		for _, urc := range git.UserRemoteConfigs {
			configs = append(configs, &userRemoteConfigXML{
				URL:           urc.UserRemoteConfig.URL,
				Name:          urc.UserRemoteConfig.Name,
				CredentialsID: urc.UserRemoteConfig.CredentialsID,
			})
		}
		scm.UserRemoteConfigs = &userRemoteConfigsXML{Configs: configs}
	}
	if len(git.Branches) > 0 {
		var specs []*branchSpecXML
		for _, b := range git.Branches {
			specs = append(specs, &branchSpecXML{Name: b.BranchSpec.Name})
		}
		scm.Branches = &branchesXML{Specs: specs}
	}
	if len(git.Extensions) > 0 {
		ext := &scmExtensionsXML{}
		for _, e := range git.Extensions {
			switch {
			case e.CleanBeforeCheckout != nil:
				ext.CleanBeforeCheckout = &cleanBeforeXML{
					DeleteUntrackedNestedRepositories: e.CleanBeforeCheckout.DeleteUntrackedNestedRepositories,
				}
			case e.CleanCheckout != nil:
				ext.CleanCheckout = &cleanCheckoutXML{
					DeleteUntrackedNestedRepositories: e.CleanCheckout.DeleteUntrackedNestedRepositories,
				}
			case e.CheckoutOption != nil:
				ext.CheckoutOption = &checkoutOptionXML{Timeout: e.CheckoutOption.Timeout}
			case e.CloneOption != nil:
				ext.CloneOption = &cloneOptionXML{
					Reference:    e.CloneOption.Reference,
					NoTags:       e.CloneOption.NoTags,
					HonorRefspec: e.CloneOption.HonorRefspec,
					Shallow:      e.CloneOption.Shallow,
					Timeout:      e.CloneOption.Timeout,
				}
			case e.RelativeTargetDirectory != nil:
				ext.RelativeTargetDir = &relativeTargetDirXML{
					RelativeTargetDir: e.RelativeTargetDirectory.RelativeTargetDir,
				}
			case e.PruneStaleBranch != nil:
				ext.PruneStaleBranch = &pruneStaleXML{}
			case e.WipeWorkspace != nil:
				ext.WipeWorkspace = &wipeWorkspaceXML{}
			}
		}
		scm.Extensions = ext
	}
	return scm
}

// ---- Pipeline ----

type pipelineXML struct {
	XMLName         xml.Name               `xml:"flow-definition"`
	Plugin          string                 `xml:"plugin,attr"`
	DisplayName     string                 `xml:"displayName,omitempty"`
	Description     string                 `xml:"description,omitempty"`
	Definition      *cpsFlowDefXML         `xml:"definition"`
	Disabled        bool                   `xml:"disabled,omitempty"`
	ConcurrentBuild bool                   `xml:"concurrentBuild,omitempty"`
	QuietPeriod     int                    `xml:"quietPeriod,omitempty"`
	Properties      *pipelinePropertiesXML `xml:"properties,omitempty"`
}

type cpsFlowDefXML struct {
	Class           string         `xml:"class,attr"`
	Plugin          string         `xml:"plugin,attr"`
	SCM             *jenkinsScmXML `xml:"scm,omitempty"`
	ScriptPath      string         `xml:"scriptPath,omitempty"`
	Lightweight     bool           `xml:"lightweight,omitempty"`
	Script          string         `xml:"script,omitempty"`
	Sandbox         bool           `xml:"sandbox,omitempty"`
	Template        string         `xml:"template,omitempty"`
	TemplateVersion string         `xml:"templateVersion,omitempty"`
	Values          string         `xml:"values,omitempty"`
}

type pipelinePropertiesXML struct {
	BuildDiscarder          *pipelineBuildDiscarderXML  `xml:"jenkins.model.BuildDiscarderProperty,omitempty"`
	DisableConcurrentBuilds *disableConcurrentBuildsXML `xml:"org.jenkinsci.plugins.workflow.job.properties.DisableConcurrentBuildsJobProperty,omitempty"`
	DisableResume           *disableResumeXML           `xml:"org.jenkinsci.plugins.workflow.job.properties.DisableResumeJobProperty,omitempty"`
	DurabilityHint          *durabilityHintXML          `xml:"org.jenkinsci.plugins.workflow.job.properties.DurabilityHintJobProperty,omitempty"`
	ParamsDefProp           *paramsDefPropXML           `xml:"hudson.model.ParametersDefinitionProperty,omitempty"`
	PipelineTriggers        *pipelineTriggersPropXML    `xml:"org.jenkinsci.plugins.workflow.job.properties.PipelineTriggersJobProperty,omitempty"`
}

// pipelineTriggersPropXML holds a pipeline job's triggers. A pipeline job
// has no top-level triggers element: Jenkins starts only the triggers in
// this property.
type pipelineTriggersPropXML struct {
	Triggers *jenkinsTriggersXML `xml:"triggers"`
}

type pipelineBuildDiscarderXML struct {
	Strategy *logRotatorStrategyXML `xml:"strategy"`
}

type logRotatorStrategyXML struct {
	Class              string `xml:"class,attr"`
	DaysToKeep         int    `xml:"daysToKeep,omitempty"`
	NumToKeep          int    `xml:"numToKeep,omitempty"`
	ArtifactDaysToKeep int    `xml:"artifactDaysToKeep,omitempty"`
	ArtifactNumToKeep  int    `xml:"artifactNumToKeep,omitempty"`
}

type disableConcurrentBuildsXML struct{}

type disableResumeXML struct{}

type durabilityHintXML struct {
	Hint string `xml:"hint"`
}

func pipelineConfigXML(item Item) xmlConfig {
	p := &pipelineXML{
		Plugin:          "workflow-job",
		DisplayName:     item.DisplayName,
		Description:     item.Description,
		Disabled:        item.Disabled,
		ConcurrentBuild: item.ConcurrentBuild,
		QuietPeriod:     item.QuietPeriod,
	}
	if item.Definition != nil {
		if item.Definition.CpsScmFlowDefinition != nil {
			def := item.Definition.CpsScmFlowDefinition
			sp := def.ScriptPath
			if sp == "" {
				sp = "Jenkinsfile"
			}
			p.Definition = &cpsFlowDefXML{
				Class:       "org.jenkinsci.plugins.workflow.cps.CpsScmFlowDefinition",
				Plugin:      "workflow-cps",
				SCM:         convertGitSCM(def.SCM.GitSCM),
				ScriptPath:  sp,
				Lightweight: def.Lightweight,
			}
		} else if item.Definition.Script != "" {
			p.Definition = &cpsFlowDefXML{
				Class:   "org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition",
				Plugin:  "workflow-cps",
				Script:  item.Definition.Script,
				Sandbox: true,
			}
		} else if item.Definition.VarroaTemplate != nil {
			vt := item.Definition.VarroaTemplate
			p.Definition = &cpsFlowDefXML{
				Class:           "org.jenkinsci.plugins.varroamiteauth.template.VarroaTemplateFlowDefinition",
				Plugin:          varroaMiteAuthPlugin,
				Template:        vt.Template,
				TemplateVersion: vt.TemplateVersion,
				Values:          varroaTemplateValuesJSON(vt.Values),
			}
		}
	}
	for _, prop := range item.Properties {
		if prop.BuildDiscarder != nil {
			if p.Properties == nil {
				p.Properties = &pipelinePropertiesXML{}
			}
			p.Properties.BuildDiscarder = &pipelineBuildDiscarderXML{
				Strategy: &logRotatorStrategyXML{
					Class:              "hudson.tasks.LogRotator",
					DaysToKeep:         prop.BuildDiscarder.Strategy.LogRotator.DaysToKeep,
					NumToKeep:          prop.BuildDiscarder.Strategy.LogRotator.NumToKeep,
					ArtifactDaysToKeep: prop.BuildDiscarder.Strategy.LogRotator.ArtifactDaysToKeep,
					ArtifactNumToKeep:  prop.BuildDiscarder.Strategy.LogRotator.ArtifactNumToKeep,
				},
			}
		}
		if prop.DisableConcurrentBuilds != nil {
			if p.Properties == nil {
				p.Properties = &pipelinePropertiesXML{}
			}
			p.Properties.DisableConcurrentBuilds = &disableConcurrentBuildsXML{}
		}
		if prop.DisableResume != nil {
			if p.Properties == nil {
				p.Properties = &pipelinePropertiesXML{}
			}
			p.Properties.DisableResume = &disableResumeXML{}
		}
		if prop.DurabilityHint != nil {
			if p.Properties == nil {
				p.Properties = &pipelinePropertiesXML{}
			}
			p.Properties.DurabilityHint = &durabilityHintXML{Hint: prop.DurabilityHint.Hint}
		}
	}
	// resumeBlocked and a disableResume property render the same single
	// job property. The WorkflowJob field of that name is deprecated.
	if item.ResumeBlocked {
		if p.Properties == nil {
			p.Properties = &pipelinePropertiesXML{}
		}
		p.Properties.DisableResume = &disableResumeXML{}
	}
	if params := collectParameters(item); len(params) > 0 {
		if p.Properties == nil {
			p.Properties = &pipelinePropertiesXML{}
		}
		p.Properties.ParamsDefProp = &paramsDefPropXML{ParameterDefs: convertParameters(params)}
	}
	if t := buildTriggersXML(collectTriggers(item)); t != nil {
		if p.Properties == nil {
			p.Properties = &pipelinePropertiesXML{}
		}
		p.Properties.PipelineTriggers = &pipelineTriggersPropXML{Triggers: t}
	}
	return p
}

// ---- Multibranch ----

type multibranchXML struct {
	XMLName     xml.Name          `xml:"org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject"`
	Plugin      string            `xml:"plugin,attr"`
	DisplayName string            `xml:"displayName,omitempty"`
	Description string            `xml:"description,omitempty"`
	Sources     *branchSourcesXML `xml:"sources"`
	Factory     *mbFactoryXML     `xml:"factory"`
}

type branchSourcesXML struct {
	Class string            `xml:"class,attr"`
	Data  *branchSourceData `xml:"data"`
}

type branchSourceData struct {
	BranchSource []*branchSourceItemXML `xml:"jenkins.branch.BranchSource"`
}

type branchSourceItemXML struct {
	Source          *branchSourceSourceXML `xml:"source"`
	BuildStrategies *buildStrategiesXML    `xml:"buildStrategies"`
}

// buildStrategiesXML renders the skip-initial-build strategy every
// multibranch branch source and every organization folder carries (design
// D12). The rendering does not depend on the item's own YAML: a scan
// discovers a branch, a webhook builds it.
type buildStrategiesXML struct {
	SkipInitialBuildOnFirstBranchIndexing *skipInitialBuildStrategyXML `xml:"jenkins.branch.buildstrategies.basic.SkipInitialBuildOnFirstBranchIndexing"`
}

// skipInitialBuildStrategyXML has a no-argument data-bound constructor, so
// it carries no child element.
type skipInitialBuildStrategyXML struct {
	Plugin string `xml:"plugin,attr,omitempty"`
}

func skipInitialBuildStrategies() *buildStrategiesXML {
	return &buildStrategiesXML{
		SkipInitialBuildOnFirstBranchIndexing: &skipInitialBuildStrategyXML{
			Plugin: "basic-branch-build-strategies",
		},
	}
}

// branchSourceSourceXML holds either a git or github SCM source.
// Only one is populated; omitempty ensures clean XML.
type branchSourceSourceXML struct {
	Class           string               `xml:"class,attr"`
	Plugin          string               `xml:"plugin,attr,omitempty"`
	Remote          string               `xml:"remote,omitempty"`
	CredentialsID   string               `xml:"credentialsId,omitempty"`
	RepoOwner       string               `xml:"repoOwner,omitempty"`
	Repository      string               `xml:"repository,omitempty"`
	RepositoryURL   string               `xml:"repositoryUrl,omitempty"`
	ConfiguredByURL bool                 `xml:"configuredByUrl,omitempty"`
	Traits          *gitLibraryTraitsXML `xml:"traits,omitempty"`
}

type mbFactoryXML struct {
	Class      string `xml:"class,attr"`
	Plugin     string `xml:"plugin,attr,omitempty"`
	ScriptPath string `xml:"scriptPath,omitempty"`
}

func multibranchConfigXML(item Item) xmlConfig {
	m := &multibranchXML{
		Plugin:      "workflow-multibranch",
		DisplayName: item.DisplayName,
		Description: item.Description,
	}
	// Build Sources struct once before the loop
	m.Sources = &branchSourcesXML{
		Class: "jenkins.branch.MultiBranchProject$BranchSourceList",
		Data:  &branchSourceData{},
	}
	for _, entry := range item.SourcesList {
		// Official spec: branchSource.source.github
		if entry.BranchSource.Source.GitHub != nil {
			m.Sources.Data.BranchSource = append(m.Sources.Data.BranchSource, &branchSourceItemXML{
				Source: &branchSourceSourceXML{
					Class:           "org.jenkinsci.plugins.github_branch_source.GitHubSCMSource",
					Plugin:          "github-branch-source",
					RepoOwner:       entry.BranchSource.Source.GitHub.RepoOwner,
					Repository:      entry.BranchSource.Source.GitHub.Repository,
					RepositoryURL:   entry.BranchSource.Source.GitHub.RepositoryURL,
					ConfiguredByURL: entry.BranchSource.Source.GitHub.ConfiguredByURL,
					CredentialsID:   entry.BranchSource.Source.GitHub.CredentialsID,
				},
				BuildStrategies: skipInitialBuildStrategies(),
			})
			continue
		}
		// Official spec: branchSource.source.git
		if entry.BranchSource.Source.Git != nil {
			m.Sources.Data.BranchSource = append(m.Sources.Data.BranchSource, &branchSourceItemXML{
				Source: &branchSourceSourceXML{
					Class:         "jenkins.plugins.git.GitSCMSource",
					Plugin:        "git",
					Remote:        entry.BranchSource.Source.Git.Remote,
					CredentialsID: entry.BranchSource.Source.Git.CredentialsID,
				},
				BuildStrategies: skipInitialBuildStrategies(),
			})
			continue
		}
		// Legacy format: bare git at top level
		if entry.Git != nil {
			m.Sources.Data.BranchSource = append(m.Sources.Data.BranchSource, &branchSourceItemXML{
				Source: &branchSourceSourceXML{
					Class:         "jenkins.plugins.git.GitSCMSource",
					Plugin:        "git",
					Remote:        entry.Git.Remote,
					CredentialsID: entry.Git.CredentialsID,
				},
				BuildStrategies: skipInitialBuildStrategies(),
			})
			continue
		}
		// Legacy format: bare github at top level
		if entry.GitHub != nil {
			m.Sources.Data.BranchSource = append(m.Sources.Data.BranchSource, &branchSourceItemXML{
				Source: &branchSourceSourceXML{
					Class:     "org.jenkinsci.plugins.github_branch_source.GitHubSCMSource",
					Plugin:    "github-branch-source",
					RepoOwner: entry.GitHub.RepoOwner,
				},
				BuildStrategies: skipInitialBuildStrategies(),
			})
			continue
		}
	}
	// Project factory: WorkflowBranchProjectFactory (official spec name),
	// MultiBranchProjectFactory, or the plugin's marker factory.
	if item.ProjectFactory != nil {
		if item.ProjectFactory.VarroaMarker != nil {
			m.Factory = &mbFactoryXML{
				Class:  "org.jenkinsci.plugins.varroamiteauth.template.VarroaTemplateBranchProjectFactory",
				Plugin: varroaMiteAuthPlugin,
			}
		} else {
			sp := ""
			if item.ProjectFactory.WorkflowBranch != nil {
				sp = item.ProjectFactory.WorkflowBranch.ScriptPath
			} else if item.ProjectFactory.MultiBranch != nil {
				sp = item.ProjectFactory.MultiBranch.ScriptPath
			}
			if sp != "" {
				m.Factory = &mbFactoryXML{
					Class:      "org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProjectFactory",
					ScriptPath: sp,
				}
			}
		}
	}
	return m
}

// ---- Folder Properties XML ----

type folderPropertiesXML struct {
	EnvVars           *envVarsFolderXML     `xml:"org.jenkinsci.plugins.envinject.EnvInjectFolderProperty,omitempty"`
	FolderCredentials *folderCredentialsXML `xml:"com.cloudbees.hudson.plugins.folder.properties.FolderCredentialsProvider_-FolderCredentialsProperty,omitempty"`
	FolderLibraries   *folderLibrariesXML   `xml:"org.jenkinsci.plugins.workflow.libs.FolderLibraries,omitempty"`
	ItemRestrictions  *itemRestrictionsXML  `xml:"com.cloudbees.hudson.plugins.folder.properties.RestrictedProjectsProperty,omitempty"`
	Kubernetes        *kubernetesFolderXML  `xml:"org.csanchez.jenkins.plugins.kubernetes.KubernetesFolderProperty,omitempty"`
}

type envVarsFolderXML struct {
	Vars []envVarEntryXML `xml:"properties>EnvInjectJobProperty>info>propertiesEntry"`
}

type envVarEntryXML struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

type folderCredentialsXML struct {
	DomainCredentials []folderDomainCredentialEntryXML `xml:"domainCredentialsMap>entry"`
}

type folderDomainCredentialEntryXML struct {
	Domain      domainXML         `xml:"com.cloudbees.plugins.credentials.domains.Domain"`
	Credentials credentialListXML `xml:"java.util.concurrent.CopyOnWriteArrayList"`
}

type domainXML struct {
	Specifications *domainSpecificationsXML `xml:"specifications"`
}

type domainSpecificationsXML struct{}

type credentialListXML struct {
	UsernamePassword       []*usernamePasswordCredXML       `xml:"com.cloudbees.plugins.credentials.impl.UsernamePasswordCredentialsImpl,omitempty"`
	SecretText             []*secretTextCredXML             `xml:"org.jenkinsci.plugins.plaincredentials.impl.StringCredentialsImpl,omitempty"`
	BasicSSHUserPrivateKey []*basicSSHUserPrivateKeyCredXML `xml:"com.cloudbees.jenkins.plugins.sshcredentials.impl.BasicSSHUserPrivateKey,omitempty"`
	File                   []*fileCredXML                   `xml:"org.jenkinsci.plugins.plaincredentials.impl.FileCredentialsImpl,omitempty"`
	Certificate            []*certificateCredXML            `xml:"com.cloudbees.plugins.credentials.impl.CertificateCredentialsImpl,omitempty"`
}

// uploadedKeyStoreSourceClass is the XStream class of an uploaded keystore
// source. Jenkins cannot read the keystore source without it.
const uploadedKeyStoreSourceClass = "com.cloudbees.plugins.credentials.impl.CertificateCredentialsImpl$UploadedKeyStoreSource"

type fileCredXML struct {
	Scope       string `xml:"scope"`
	ID          string `xml:"id"`
	Description string `xml:"description,omitempty"`
	FileName    string `xml:"fileName"`
	SecretBytes string `xml:"secretBytes"`
}

// certificateCredXML always renders the password element, so XStream reads
// an empty Secret and never a null field.
type certificateCredXML struct {
	Scope          string            `xml:"scope"`
	ID             string            `xml:"id"`
	Description    string            `xml:"description,omitempty"`
	KeyStoreSource keyStoreSourceXML `xml:"keyStoreSource"`
	Password       string            `xml:"password"`
}

type keyStoreSourceXML struct {
	Class                 string `xml:"class,attr"`
	UploadedKeystoreBytes string `xml:"uploadedKeystoreBytes"`
}

// directEntryPrivateKeySourceClass is the XStream class of a direct-entry
// key source. Jenkins cannot read the key source without it.
const directEntryPrivateKeySourceClass = "com.cloudbees.jenkins.plugins.sshcredentials.impl.BasicSSHUserPrivateKey$DirectEntryPrivateKeySource"

// basicSSHUserPrivateKeyCredXML always renders the passphrase element, so
// XStream reads an empty Secret and never a null field.
type basicSSHUserPrivateKeyCredXML struct {
	Scope            string              `xml:"scope"`
	ID               string              `xml:"id"`
	Description      string              `xml:"description,omitempty"`
	Username         string              `xml:"username"`
	PrivateKeySource privateKeySourceXML `xml:"privateKeySource"`
	Passphrase       string              `xml:"passphrase"`
}

type privateKeySourceXML struct {
	Class      string `xml:"class,attr"`
	PrivateKey string `xml:"privateKey"`
}

type usernamePasswordCredXML struct {
	Scope       string `xml:"scope"`
	ID          string `xml:"id"`
	Description string `xml:"description,omitempty"`
	Username    string `xml:"username"`
	Password    string `xml:"password"`
}

type secretTextCredXML struct {
	Scope       string `xml:"scope"`
	ID          string `xml:"id"`
	Description string `xml:"description,omitempty"`
	Secret      string `xml:"secret"`
}

type folderLibrariesXML struct {
	Libraries []libraryEntryXML `xml:"libraries>org.jenkinsci.plugins.workflow.libs.LibraryConfiguration"`
}

// libraryEntryXML renders one folder library. AllowVersionOverride has no
// omitempty so that false is written: XStream does not run the Java field
// initialiser for a missing element.
type libraryEntryXML struct {
	Name                 string        `xml:"name"`
	Retriever            *retrieverXML `xml:"retriever,omitempty"`
	DefaultVersion       string        `xml:"defaultVersion,omitempty"`
	Implicit             bool          `xml:"implicit,omitempty"`
	AllowVersionOverride bool          `xml:"allowVersionOverride"`
	IncludeInChangesets  bool          `xml:"includeInChangesets,omitempty"`
}

// retrieverXML renders a shared-library retriever. The class selects the
// retriever: SCMSourceRetriever for a modern source (scm holds a branch
// source), or SCMRetriever for the legacy form (scm holds a git SCM block).
type retrieverXML struct {
	Class string `xml:"class,attr"`
	SCM   any    `xml:"scm"`
}

// gitLibraryTraitsXML holds the discovery traits of a git library source.
// Both traits are fixed so that every branch and tag resolves as a version.
type gitLibraryTraitsXML struct {
	Branch *struct{} `xml:"jenkins.plugins.git.traits.BranchDiscoveryTrait"`
	Tag    *struct{} `xml:"jenkins.plugins.git.traits.TagDiscoveryTrait"`
}

type itemRestrictionsXML struct {
	AllowedTypes []string `xml:"allowedTypes>string"`
	Filter       bool     `xml:"filter"`
}

type kubernetesFolderXML struct {
	PermittedClouds []string `xml:"permittedClouds>string"`
}

func buildFolderProperties(item Item) (*folderPropertiesXML, error) {
	props := &folderPropertiesXML{}
	if len(item.Properties) == 0 {
		return props, nil
	}
	for _, p := range item.Properties {
		if p.EnvVars != nil {
			var entries []envVarEntryXML
			// Sort keys for deterministic output.
			keys := make([]string, 0, len(p.EnvVars.Vars))
			for k := range p.EnvVars.Vars {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				entries = append(entries, envVarEntryXML{Key: k, Value: p.EnvVars.Vars[k]})
			}
			props.EnvVars = &envVarsFolderXML{Vars: entries}
		}
		if p.ItemRestrictions != nil {
			props.ItemRestrictions = &itemRestrictionsXML{
				AllowedTypes: p.ItemRestrictions.AllowedTypes,
				Filter:       p.ItemRestrictions.Filter,
			}
		}
		if p.FolderCredentialsProperty != nil {
			var entries []folderDomainCredentialEntryXML
			for _, fc := range p.FolderCredentialsProperty.FolderCredentials {
				var creds credentialListXML
				for _, c := range fc.Credentials {
					switch {
					case c.UsernamePassword != nil:
						creds.UsernamePassword = append(creds.UsernamePassword, &usernamePasswordCredXML{
							Scope:       c.UsernamePassword.Scope,
							ID:          c.UsernamePassword.ID,
							Description: c.UsernamePassword.Description,
							Username:    c.UsernamePassword.Username,
							Password:    c.UsernamePassword.Password,
						})
					case c.SecretText != nil:
						creds.SecretText = append(creds.SecretText, &secretTextCredXML{
							Scope:       c.SecretText.Scope,
							ID:          c.SecretText.ID,
							Description: c.SecretText.Description,
							Secret:      c.SecretText.Secret,
						})
					case c.BasicSSHUserPrivateKey != nil:
						k := c.BasicSSHUserPrivateKey
						if k.PrivateKeySource.DirectEntry == nil {
							return nil, fmt.Errorf("basicSSHUserPrivateKey credential %q has no directEntry private key source", k.ID)
						}
						creds.BasicSSHUserPrivateKey = append(creds.BasicSSHUserPrivateKey, &basicSSHUserPrivateKeyCredXML{
							Scope:       k.Scope,
							ID:          k.ID,
							Description: k.Description,
							Username:    k.Username,
							PrivateKeySource: privateKeySourceXML{
								Class:      directEntryPrivateKeySourceClass,
								PrivateKey: k.PrivateKeySource.DirectEntry.PrivateKey,
							},
							Passphrase: k.Passphrase,
						})
					case c.File != nil:
						creds.File = append(creds.File, &fileCredXML{
							Scope:       c.File.Scope,
							ID:          c.File.ID,
							Description: c.File.Description,
							FileName:    c.File.FileName,
							SecretBytes: c.File.SecretBytes,
						})
					case c.Certificate != nil:
						k := c.Certificate
						if k.KeyStoreSource.Uploaded == nil {
							return nil, fmt.Errorf("certificate credential %q has no uploaded keystore source", k.ID)
						}
						creds.Certificate = append(creds.Certificate, &certificateCredXML{
							Scope:       k.Scope,
							ID:          k.ID,
							Description: k.Description,
							KeyStoreSource: keyStoreSourceXML{
								Class:                 uploadedKeyStoreSourceClass,
								UploadedKeystoreBytes: k.KeyStoreSource.Uploaded.UploadedKeystore,
							},
							Password: k.Password,
						})
					default:
						return nil, fmt.Errorf("folder credential entry has no recognized credential type")
					}
				}
				entries = append(entries, folderDomainCredentialEntryXML{
					Domain:      domainXML{Specifications: &domainSpecificationsXML{}},
					Credentials: creds,
				})
			}
			props.FolderCredentials = &folderCredentialsXML{DomainCredentials: entries}
		}
		if p.FolderLibraries != nil {
			var entries []libraryEntryXML
			for _, lc := range p.FolderLibraries.Libraries {
				cfg := lc.LibraryConfiguration
				retriever, err := libraryRetrieverXML(cfg.Retriever)
				if err != nil {
					return nil, fmt.Errorf("folder library %q: %w", cfg.Name, err)
				}
				entries = append(entries, libraryEntryXML{
					Name:                 cfg.Name,
					Retriever:            retriever,
					DefaultVersion:       cfg.DefaultVersion,
					Implicit:             cfg.Implicit,
					AllowVersionOverride: cfg.AllowVersionOverride,
					IncludeInChangesets:  cfg.IncludeInChangesets,
				})
			}
			props.FolderLibraries = &folderLibrariesXML{Libraries: entries}
		}
		if p.Kubernetes != nil {
			props.Kubernetes = &kubernetesFolderXML{PermittedClouds: p.Kubernetes.PermittedClouds}
		}
	}
	return props, nil
}

// libraryRetrieverXML renders a shared-library retriever. A GitHub or git
// modern source uses SCMSourceRetriever with a branch-source element. A
// legacy source uses SCMRetriever with a git SCM element. The validator
// rejects every other shape before this runs; the errors remain as a backstop.
func libraryRetrieverXML(r LibraryRetriever) (*retrieverXML, error) {
	switch {
	case r.ModernSCM != nil && r.LegacySCM != nil:
		return nil, fmt.Errorf("retriever must set exactly one of modernSCM and legacySCM")
	case r.LegacySCM != nil:
		git := r.LegacySCM.SCM.GitSCM
		if git == nil {
			return nil, fmt.Errorf("legacySCM retriever names no gitSCM")
		}
		return &retrieverXML{
			Class: "org.jenkinsci.plugins.workflow.libs.SCMRetriever",
			SCM:   convertGitSCM(git),
		}, nil
	case r.ModernSCM == nil:
		return nil, fmt.Errorf("retriever names no source (set modernSCM or legacySCM)")
	}
	s := r.ModernSCM.SCM
	const retrieverClass = "org.jenkinsci.plugins.workflow.libs.SCMSourceRetriever"
	switch {
	case s.GitHub != nil && s.Git != nil:
		return nil, fmt.Errorf("modernSCM.scm must set exactly one of github and git")
	case s.GitHub != nil:
		gh := s.GitHub
		return &retrieverXML{
			Class: retrieverClass,
			SCM: &branchSourceSourceXML{
				Class:           "org.jenkinsci.plugins.github_branch_source.GitHubSCMSource",
				Plugin:          "github-branch-source",
				RepoOwner:       gh.RepoOwner,
				Repository:      gh.Repository,
				RepositoryURL:   gh.RepositoryURL,
				ConfiguredByURL: gh.ConfiguredByURL,
				CredentialsID:   gh.CredentialsID,
			},
		}, nil
	case s.Git != nil:
		g := s.Git
		return &retrieverXML{
			Class: retrieverClass,
			SCM: &branchSourceSourceXML{
				Class:         "jenkins.plugins.git.GitSCMSource",
				Plugin:        "git",
				Remote:        g.Remote,
				CredentialsID: g.CredentialsID,
				Traits:        &gitLibraryTraitsXML{Branch: &struct{}{}, Tag: &struct{}{}},
			},
		}, nil
	default:
		return nil, fmt.Errorf("modernSCM.scm must set exactly one of github and git")
	}
}

// ---- Organization Folder ----

type orgFolderXML struct {
	XMLName          xml.Name             `xml:"jenkins.branch.OrganizationFolder"`
	Plugin           string               `xml:"plugin,attr"`
	DisplayName      string               `xml:"displayName,omitempty"`
	Description      string               `xml:"description,omitempty"`
	Navigators       *navigatorsXML       `xml:"navigators"`
	ProjectFactories *projectFactoriesXML `xml:"projectFactories"`
	BuildStrategies  *buildStrategiesXML  `xml:"buildStrategies"`
}

type navigatorsXML struct {
	GitHub []*githubNavigatorXML `xml:"org.jenkinsci.plugins.github_branch_source.GitHubSCMNavigator"`
	Git    []*gitNavigatorXML    `xml:"jenkins.plugins.git.GitSCMNavigator"`
}

type githubNavigatorXML struct {
	Plugin        string `xml:"plugin,attr,omitempty"`
	RepoOwner     string `xml:"repoOwner"`
	CredentialsID string `xml:"credentialsId,omitempty"`
}

type gitNavigatorXML struct {
	Plugin        string `xml:"plugin,attr,omitempty"`
	Remote        string `xml:"remote"`
	CredentialsID string `xml:"credentialsId,omitempty"`
}

type projectFactoriesXML struct {
	Factory []*mbFactoryXML `xml:"org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProjectFactory"`
}

func orgFolderConfigXML(item Item) xmlConfig {
	o := &orgFolderXML{
		Plugin:          "branch-api",
		DisplayName:     item.DisplayName,
		Description:     item.Description,
		BuildStrategies: skipInitialBuildStrategies(),
	}
	if len(item.Navigators) > 0 {
		o.Navigators = &navigatorsXML{}
		for _, nav := range item.Navigators {
			if nav.GitHub != nil {
				o.Navigators.GitHub = append(o.Navigators.GitHub, &githubNavigatorXML{
					Plugin:        "github-branch-source",
					RepoOwner:     nav.GitHub.RepoOwner,
					CredentialsID: nav.GitHub.CredentialsID,
				})
			}
			if nav.Git != nil {
				o.Navigators.Git = append(o.Navigators.Git, &gitNavigatorXML{
					Plugin:        "git",
					Remote:        nav.Git.Remote,
					CredentialsID: nav.Git.CredentialsID,
				})
			}
		}
	}
	if len(item.ProjectFactories) > 0 {
		o.ProjectFactories = &projectFactoriesXML{}
		for _, pf := range item.ProjectFactories {
			if pf.MultiBranch != nil {
				sp := pf.MultiBranch.ScriptPath
				if sp == "" {
					sp = "Jenkinsfile"
				}
				o.ProjectFactories.Factory = append(o.ProjectFactories.Factory, &mbFactoryXML{
					Class:      "org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProjectFactory",
					ScriptPath: sp,
				})
			}
		}
	}
	return o
}
