package rehydrate

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"sort"
)

// Artifact manager element constants, from design.md D2 and verified
// against artifact-manager-s3's on-disk classes.
const (
	artifactManagerClass  = "io.jenkins.plugins.artifact_manager_jclouds.JCloudsArtifactManager"
	artifactManagerPlugin = "artifact-manager-s3"
	s3BlobStoreClass      = "io.jenkins.plugins.artifact_manager_jclouds.s3.S3BlobStore"
)

// Checkout element constants, from design.md D1a.
const (
	varroaRehydratedSCMClass = "org.jenkinsci.plugins.varroamiteauth.rehydrate.VarroaRehydratedSCM"
	varroaPluginArtifactID   = "varroa-mite-auth"
	persistedListClass       = "hudson.util.PersistedList"
)

// Options carries the per-controller facts RenderBuildXML needs beyond
// one Record.
type Options struct {
	// HasArtifactManagerS3 is whether the controller's resolved plugin
	// set (coreSetForCr) pins artifact-manager-s3. RenderBuildXML
	// writes the <artifactManager> element only when this is true
	// (design.md D2).
	HasArtifactManagerS3 bool
}

// flowBuildXML is the <flow-build> document design.md D1 defines.
// Field order matches the D1 element table, which also matches the
// hudson.model.Run field declaration order javap confirms.
type flowBuildXML struct {
	XMLName         xml.Name            `xml:"flow-build"`
	Actions         actionsXML          `xml:"actions"`
	QueueID         int64               `xml:"queueId"`
	Timestamp       int64               `xml:"timestamp"`
	StartTime       int64               `xml:"startTime"`
	Result          string              `xml:"result"`
	Description     string              `xml:"description"`
	Duration        int64               `xml:"duration"`
	Charset         string              `xml:"charset"`
	KeepLog         bool                `xml:"keepLog"`
	ArtifactManager *artifactManagerXML `xml:"artifactManager,omitempty"`
	Checkouts       *checkoutsXML       `xml:"checkouts,omitempty"`
	Completed       bool                `xml:"completed"`
}

// actionsXML is <actions>: empty when the run recorded no unmasked
// parameter.
type actionsXML struct {
	ParametersAction *parametersActionXML `xml:"hudson.model.ParametersAction"`
}

// parametersActionXML is one hudson.model.ParametersAction. Field
// order matches javap's field-declaration order on
// jenkins-core-2.555.3.jar: safeParameters, parameters,
// parameterDefinitionNames.
type parametersActionXML struct {
	SafeParameters           emptyElementXML           `xml:"safeParameters"`
	Parameters               []stringParameterValueXML `xml:"parameters>hudson.model.StringParameterValue"`
	ParameterDefinitionNames []string                  `xml:"parameterDefinitionNames>string"`
}

// emptyElementXML renders as an empty element. safeParameters is
// always empty: the operator marks no parameter safe.
type emptyElementXML struct{}

type stringParameterValueXML struct {
	Name  string `xml:"name"`
	Value string `xml:"value"`
}

// artifactManagerXML is design.md D2's element.
type artifactManagerXML struct {
	Class    string      `xml:"class,attr"`
	Plugin   string      `xml:"plugin,attr"`
	Provider providerXML `xml:"provider"`
}

type providerXML struct {
	Class string `xml:"class,attr"`
}

// checkoutsXML is design.md D1a's <checkouts> element.
type checkoutsXML struct {
	Class    string         `xml:"class,attr"`
	Checkout scmCheckoutXML `xml:"org.jenkinsci.plugins.workflow.job.WorkflowRun_-SCMCheckout"`
}

type scmCheckoutXML struct {
	SCM           scmRefXML `xml:"scm"`
	Node          string    `xml:"node"`
	Workspace     string    `xml:"workspace"`
	ChangelogFile string    `xml:"changelogFile"`
}

type scmRefXML struct {
	Class  string `xml:"class,attr"`
	Plugin string `xml:"plugin,attr"`
}

// RenderBuildXML renders one build.xml document for rec, per
// design.md D1/D1a/D2. changelogAbsPath is the absolute path
// RenderBuildXML writes into <checkouts> when rec carries a change;
// the caller (the job-seed builder) computes it with
// ChangelogAbsolutePath. RenderBuildXML fails when rec carries a
// change but changelogAbsPath is empty: a build.xml that names a
// change must always carry a path for it.
func RenderBuildXML(rec Record, opts Options, changelogAbsPath string) ([]byte, error) {
	if len(rec.Changes) > 0 && changelogAbsPath == "" {
		return nil, fmt.Errorf("record for build %d carries %d changes but no changelog path was supplied", rec.BuildNumber, len(rec.Changes))
	}

	doc := flowBuildXML{
		QueueID:     -1,
		Timestamp:   millis(rec.QueuedAt),
		StartTime:   millis(rec.StartedAt),
		Result:      jenkinsResult(rec.Result),
		Description: rec.CauseDescription,
		Duration:    durationMillis(rec.DurationMs),
		Charset:     "UTF-8",
		KeepLog:     false,
		Completed:   true,
	}

	if names := unmaskedParamNames(rec.Params, rec.MaskedParameterNames); len(names) > 0 {
		doc.Actions.ParametersAction = &parametersActionXML{
			Parameters:               stringParameterValues(rec.Params, names),
			ParameterDefinitionNames: names,
		}
	}

	if opts.HasArtifactManagerS3 {
		doc.ArtifactManager = &artifactManagerXML{
			Class:    artifactManagerClass,
			Plugin:   artifactManagerPlugin,
			Provider: providerXML{Class: s3BlobStoreClass},
		}
	}

	if len(rec.Changes) > 0 {
		doc.Checkouts = &checkoutsXML{
			Class: persistedListClass,
			Checkout: scmCheckoutXML{
				SCM: scmRefXML{
					Class:  varroaRehydratedSCMClass,
					Plugin: varroaPluginArtifactID,
				},
				ChangelogFile: changelogAbsPath,
			},
		}
	}

	return marshalXML(doc)
}

// unmaskedParamNames answers params' keys that maskedNames does not
// list, sorted, so the rendered document is deterministic.
func unmaskedParamNames(params map[string]string, maskedNames []string) []string {
	masked := make(map[string]bool, len(maskedNames))
	for _, n := range maskedNames {
		masked[n] = true
	}
	names := make([]string, 0, len(params))
	for name := range params {
		if masked[name] {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func stringParameterValues(params map[string]string, names []string) []stringParameterValueXML {
	values := make([]stringParameterValueXML, 0, len(names))
	for _, name := range names {
		values = append(values, stringParameterValueXML{Name: name, Value: params[name]})
	}
	return values
}

// marshalXML encodes doc with the xml.Header and two-space indent, the
// convention internal/jenkins/items/configxml.go also follows.
func marshalXML(doc interface{}) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("render xml: %w", err)
	}
	buf.WriteString("\n")
	return buf.Bytes(), nil
}
