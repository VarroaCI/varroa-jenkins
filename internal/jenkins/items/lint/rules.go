package lint

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
)

// NoPollSCMRule rejects a pollSCM trigger. A rehydrated job has no revision
// baseline, so polling either rebuilds everything or nothing, and polling
// scales worst across a thousand tenants. Webhooks build; scans discover.
type NoPollSCMRule struct{}

// ID returns the rule id.
func (NoPollSCMRule) ID() string { return "no-poll-scm" }

// Check reports a finding for a pollSCM trigger, whether it appears in an
// item's triggers or inside the pipeline-triggers property.
func (r NoPollSCMRule) Check(item items.Item, path string) []Finding {
	var findings []Finding
	for _, t := range item.Triggers {
		if t.PollSCM != nil {
			findings = append(findings, Finding{Rule: r.ID(), Path: path, Message: "a pollSCM trigger is not allowed on a cell"})
			break
		}
	}
	for _, p := range item.Properties {
		if p.PipelineTriggers == nil {
			continue
		}
		for _, t := range p.PipelineTriggers.Triggers {
			if t.PollSCM != nil {
				findings = append(findings, Finding{Rule: r.ID(), Path: path, Message: "a pollSCM trigger is not allowed on a cell"})
				break
			}
		}
	}
	return findings
}

// NoGlobalCredentialsRule rejects a credential declared anywhere other than
// a tenant folder's folder-credentials property, and a folder credential
// whose scope is set to any value other than GLOBAL, which in a folder
// store means "this folder and its children". A controller-level
// credential must stay operator-owned and SYSTEM-scoped, so a build can
// never read it.
type NoGlobalCredentialsRule struct{}

// ID returns the rule id.
func (NoGlobalCredentialsRule) ID() string { return "no-global-credentials" }

// Check reports a finding for a credential outside a tenant folder store,
// or a scope other than GLOBAL.
func (r NoGlobalCredentialsRule) Check(item items.Item, path string) []Finding {
	var findings []Finding
	for _, p := range item.Properties {
		if p.FolderCredentialsProperty == nil {
			continue
		}
		if !item.IsFolder() {
			findings = append(findings, Finding{Rule: r.ID(), Path: path, Message: "a credential must be declared on a tenant folder, not a job"})
			continue
		}
		for _, entry := range p.FolderCredentialsProperty.FolderCredentials {
			for _, c := range entry.Credentials {
				scope := credentialScope(c)
				if scope != "" && scope != "GLOBAL" {
					findings = append(findings, Finding{Rule: r.ID(), Path: path, Message: fmt.Sprintf("folder credential scope %q must be GLOBAL", scope)})
				}
			}
		}
	}
	return findings
}

// credentialScope returns the scope of whichever credential kind is set, or
// the empty string when no kind is set. A kind missing here passes the
// no-global-credentials check with any scope.
func credentialScope(c items.FolderCredential) string {
	switch {
	case c.UsernamePassword != nil:
		return c.UsernamePassword.Scope
	case c.SecretText != nil:
		return c.SecretText.Scope
	case c.BasicSSHUserPrivateKey != nil:
		return c.BasicSSHUserPrivateKey.Scope
	case c.File != nil:
		return c.File.Scope
	case c.Certificate != nil:
		return c.Certificate.Scope
	case c.GitHubApp != nil:
		return c.GitHubApp.Scope
	default:
		return ""
	}
}

// jobDSLStep matches a jobDsl step call in a Groovy script: a bare or
// qualified "jobDsl" name followed by "(" or by a named argument after
// whitespace. It also matches the step's builder class and package name.
// A key or label that only starts with the name, such as jobDslVersion:,
// does not match.
var jobDSLStep = regexp.MustCompile(`(?:^|[^\w$])jobDsl\s*(?:\(|\s\w+\s*:)|\bExecuteDslScripts\b|\bjavaposse\.jobdsl\.`)

// NoJobDSLRule rejects an inline pipeline script that runs a Job DSL step.
// A cell home is temporary, so jobs a seed job creates are lost when the
// cell restarts, exist on only one member of the tenant, and can name a
// path outside the tenant folder. Every job comes from items.yaml. The lint
// reads an inline script and the inline jenkinsfile of a pipeline template.
// A Jenkinsfile in SCM is not visible here. The plugin blocks the step at run
// time on a cell, for every script source.
type NoJobDSLRule struct{}

// ID returns the rule id.
func (NoJobDSLRule) ID() string { return "no-job-dsl" }

// Check reports a finding for an inline script that calls a Job DSL step.
func (r NoJobDSLRule) Check(item items.Item, path string) []Finding {
	if item.Definition == nil {
		return nil
	}
	return r.CheckScript(item.Definition.Script, path)
}

// CheckScript reports a finding for a Groovy body that calls a Job DSL step.
func (r NoJobDSLRule) CheckScript(script, path string) []Finding {
	if !jobDSLStep.MatchString(script) {
		return nil
	}
	return []Finding{{Rule: r.ID(), Path: path, Message: "a Job DSL step is not allowed on a cell; declare each job in items.yaml"}}
}

// allowedPropertyFields is the complete set of items.Property fields a
// tenant item may set. Every allowed property is folder-scoped or
// job-scoped. A property outside this list reaches controller-level plugin
// configuration, which one tenant must never change for the others.
var allowedPropertyFields = map[string]bool{
	"buildDiscarder":            true,
	"disableConcurrentBuilds":   true,
	"disableResume":             true,
	"parameters":                true,
	"githubProjectProperty":     true,
	"durabilityHint":            true,
	"preserveStashes":           true,
	"rateLimitBuilds":           true,
	"pipelineTriggers":          true,
	"envVars":                   true,
	"folderCredentialsProperty": true,
	"folderLibraries":           true,
	"itemRestrictions":          true,
	"kubernetes":                true,
}

// NoControllerConfigDependencyRule rejects an item property outside the
// complete allow-list. The rule rejects nothing until someone adds a
// property to items.Property without adding it to the allow-list: that is
// deliberate, and it is the gate that forces a decision when a new
// controller-scoped property appears.
type NoControllerConfigDependencyRule struct{}

// ID returns the rule id.
func (NoControllerConfigDependencyRule) ID() string { return "no-controller-config-dependency" }

// Check reports a finding for each set Property field outside the
// allow-list.
func (r NoControllerConfigDependencyRule) Check(item items.Item, path string) []Finding {
	var findings []Finding
	for _, p := range item.Properties {
		for _, name := range disallowedFieldNames(populatedFieldNames(p)) {
			findings = append(findings, Finding{Rule: r.ID(), Path: path, Message: fmt.Sprintf("property %q is not permitted on a tenant item", name)})
		}
	}
	return findings
}

// populatedFieldNames returns the yaml tag name of every non-nil field on
// p, reflecting over items.Property so a newly added field is picked up
// without a matching code change here.
func populatedFieldNames(p items.Property) []string {
	var names []string
	v := reflect.ValueOf(p)
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := v.Field(i)
		if field.Kind() != reflect.Pointer || field.IsNil() {
			continue
		}
		tag := t.Field(i).Tag.Get("yaml")
		name := strings.SplitN(tag, ",", 2)[0]
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// disallowedFieldNames returns the subset of names not present in
// allowedPropertyFields. It is a pure function so the allow-list check can
// be exercised directly with a synthetic name, without constructing a
// items.Property that today can never carry a disallowed field.
func disallowedFieldNames(names []string) []string {
	var out []string
	for _, name := range names {
		if !allowedPropertyFields[name] {
			out = append(out, name)
		}
	}
	return out
}
