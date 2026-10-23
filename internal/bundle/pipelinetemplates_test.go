package bundle

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const oneTemplateYAML = `templates:
  - apiVersion: varroa.dev/v1
    kind: PipelineTemplate
    metadata:
      name: go-service
    spec:
      version: 1.0.0
      parameters:
        type: object
        additionalProperties: false
      jenkinsfile: "pipeline { stages { stage('build') { steps { echo '${env.BRANCH_NAME}' } } } }"
`

// otherTemplateYAML delivers a different name+version than
// oneTemplateYAML, so injecting both across two calls carries no
// identity collision.
const otherTemplateYAML = `templates:
  - apiVersion: varroa.dev/v1
    kind: PipelineTemplate
    metadata:
      name: node-service
    spec:
      version: 2.0.0
      parameters:
        type: object
        additionalProperties: false
      jenkinsfile: "pipeline {}"
`

// oneTemplateDifferentBodyYAML delivers the same name+version as
// oneTemplateYAML but a different jenkinsfile body, for the
// identity-collision tests below.
const oneTemplateDifferentBodyYAML = `templates:
  - apiVersion: varroa.dev/v1
    kind: PipelineTemplate
    metadata:
      name: go-service
    spec:
      version: 1.0.0
      parameters:
        type: object
        additionalProperties: false
      jenkinsfile: "pipeline { stages { stage('build') { steps { echo 'different' } } } }"
`

func TestInjectPipelineTemplates_EmptyTemplatesYAMLIsNoop(t *testing.T) {
	out, err := InjectPipelineTemplates("jenkins:\n  systemMessage: hi\n", "", "")
	if err != nil {
		t.Fatalf("InjectPipelineTemplates: %v", err)
	}
	if out != "jenkins:\n  systemMessage: hi\n" {
		t.Errorf("expected jenkinsYAML unchanged, got: %q", out)
	}
}

func TestInjectPipelineTemplates_JenkinsfileBodySurvives(t *testing.T) {
	out, err := InjectPipelineTemplates("", oneTemplateYAML, "")
	if err != nil {
		t.Fatalf("InjectPipelineTemplates: %v", err)
	}
	if !strings.Contains(out, "${env.BRANCH_NAME}") {
		t.Errorf("expected the template body to survive injection, got: %s", out)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	unclassified, _ := doc["unclassified"].(map[string]any)
	store, _ := unclassified["varroaPipelineTemplates"].(map[string]any)
	templates, _ := store["templates"].([]any)
	if len(templates) != 1 {
		t.Fatalf("expected exactly one template, got %+v", templates)
	}
	entry := templates[0].(map[string]any)
	if entry["name"] != "go-service" {
		t.Errorf("name = %v, want go-service", entry["name"])
	}
	if entry["version"] != "1.0.0" {
		t.Errorf("version = %v, want 1.0.0", entry["version"])
	}
	if entry["folder"] != "" {
		t.Errorf("folder = %v, want empty", entry["folder"])
	}
	if _, isString := entry["parameters"].(string); !isString {
		t.Errorf("parameters must be carried as a string, got %T", entry["parameters"])
	}
}

// TestInjectPipelineTemplates_AppendsAcrossCalls proves the injector never
// replaces an earlier call's entries: the controller's own bundle (folder
// "") and each placed tenant's fragment (folder <tenant>) can each call this
// once, in turn, and every entry survives. The two calls deliver different
// identities (name+version), so this proves append behavior only; the
// identity-collision and dedup rules across calls have their own tests
// below.
func TestInjectPipelineTemplates_AppendsAcrossCalls(t *testing.T) {
	out, err := InjectPipelineTemplates("", oneTemplateYAML, "")
	if err != nil {
		t.Fatalf("first InjectPipelineTemplates: %v", err)
	}
	out, err = InjectPipelineTemplates(out, otherTemplateYAML, "team-a")
	if err != nil {
		t.Fatalf("second InjectPipelineTemplates: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	unclassified, _ := doc["unclassified"].(map[string]any)
	store, _ := unclassified["varroaPipelineTemplates"].(map[string]any)
	templates, _ := store["templates"].([]any)
	if len(templates) != 2 {
		t.Fatalf("expected two entries after two calls, got %+v", templates)
	}
	folders := map[string]bool{}
	for _, tpl := range templates {
		entry := tpl.(map[string]any)
		folders[entry["folder"].(string)] = true
	}
	if !folders[""] || !folders["team-a"] {
		t.Errorf("expected one entry with folder \"\" and one with folder team-a, got %+v", templates)
	}
}

// TestInjectPipelineTemplates_IdentityCollisionSameFolderIsError proves
// 🟡1 of review round 1, narrowed by new 🔴1 of review round 2: design
// D5's identity rule applies across fragments that share one folder, not
// only within one bundle's own inputs. Two fragments for the controller's
// own bundle (folder "") deliver go-service@1.0.0 with different
// jenkinsfile bodies, which is an error naming both sources.
func TestInjectPipelineTemplates_IdentityCollisionSameFolderIsError(t *testing.T) {
	out, err := InjectPipelineTemplates("", oneTemplateYAML, "")
	if err != nil {
		t.Fatalf("first InjectPipelineTemplates: %v", err)
	}
	_, err = InjectPipelineTemplates(out, oneTemplateDifferentBodyYAML, "")
	if err == nil {
		t.Fatal("expected an error for a folder+name+version collision with different content")
	}
	msg := err.Error()
	if !strings.Contains(msg, "go-service@1.0.0") {
		t.Errorf("expected the error to name the colliding identity, got: %v", err)
	}
	if !strings.Contains(msg, "controller's own bundle") {
		t.Errorf("expected the error to name both sources, got: %v", err)
	}
}

// TestInjectPipelineTemplates_DifferentContentAcrossSiblingFoldersNoCollision
// proves the disjoint half of round 3's overlap rule: two different
// non-empty folders never overlap (design D7), so the same name+version
// with different content in two sibling tenant folders is never a
// collision — their content is never even compared. Two sibling tenant
// fragments (folder "team-a" and "team-b") deliver go-service@1.0.0 with
// different jenkinsfile bodies; both entries survive with no error.
func TestInjectPipelineTemplates_DifferentContentAcrossSiblingFoldersNoCollision(t *testing.T) {
	out, err := InjectPipelineTemplates("", oneTemplateYAML, "team-a")
	if err != nil {
		t.Fatalf("first InjectPipelineTemplates: %v", err)
	}
	out, err = InjectPipelineTemplates(out, oneTemplateDifferentBodyYAML, "team-b")
	if err != nil {
		t.Fatalf("expected no error for a name+version match across sibling folders, got: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	unclassified, _ := doc["unclassified"].(map[string]any)
	store, _ := unclassified["varroaPipelineTemplates"].(map[string]any)
	templates, _ := store["templates"].([]any)
	if len(templates) != 2 {
		t.Fatalf("expected both sibling folders to keep their own entry, got %+v", templates)
	}
}

// TestInjectPipelineTemplates_DifferentContentControllerAndTenantCollides
// proves the overlapping half of round 3's overlap rule: an empty folder
// is controller-wide and overlaps every tenant folder (design D7), so
// different content at the same name+version between the controller's
// own bundle (folder "") and a tenant's fragment (folder "team-a") is a
// collision, naming both sources, the same as two fragments sharing one
// folder.
func TestInjectPipelineTemplates_DifferentContentControllerAndTenantCollides(t *testing.T) {
	out, err := InjectPipelineTemplates("", oneTemplateYAML, "")
	if err != nil {
		t.Fatalf("first InjectPipelineTemplates: %v", err)
	}
	_, err = InjectPipelineTemplates(out, oneTemplateDifferentBodyYAML, "team-a")
	if err == nil {
		t.Fatal("expected an error for an empty-folder and tenant-folder collision with different content")
	}
	msg := err.Error()
	if !strings.Contains(msg, "go-service@1.0.0") {
		t.Errorf("expected the error to name the colliding identity, got: %v", err)
	}
	if !strings.Contains(msg, "controller's own bundle") || !strings.Contains(msg, `tenant "team-a"`) {
		t.Errorf("expected the error to name both sources, got: %v", err)
	}
}

// TestInjectPipelineTemplates_IdenticalContentSameFolderDedups proves the
// other half of design D5's identity rule: the same folder, name, version,
// and content, delivered by two fragments, is a harmless duplicate rather
// than an error. This can happen when the same fragment is injected twice
// for one folder.
func TestInjectPipelineTemplates_IdenticalContentSameFolderDedups(t *testing.T) {
	out, err := InjectPipelineTemplates("", oneTemplateYAML, "")
	if err != nil {
		t.Fatalf("first InjectPipelineTemplates: %v", err)
	}
	out, err = InjectPipelineTemplates(out, oneTemplateYAML, "")
	if err != nil {
		t.Fatalf("second InjectPipelineTemplates: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	unclassified, _ := doc["unclassified"].(map[string]any)
	store, _ := unclassified["varroaPipelineTemplates"].(map[string]any)
	templates, _ := store["templates"].([]any)
	if len(templates) != 1 {
		t.Fatalf("expected the identical duplicate within one folder to be de-duplicated into one entry, got %+v", templates)
	}
}

// TestInjectPipelineTemplates_SameContentAcrossTenantFoldersKeepsBoth
// proves design D7's folder scoping (new 🔴1 of review round 2): two
// sibling tenant folders that both deliver the identical go-service@1.0.0
// document each keep their own entry. Folder is part of a template
// entry's identity, so this is never treated as a duplicate — the plugin
// permits only the requesting tenant's own folder, so collapsing the two
// entries into one would make the template unresolvable from the second
// tenant's jobs.
func TestInjectPipelineTemplates_SameContentAcrossTenantFoldersKeepsBoth(t *testing.T) {
	out, err := InjectPipelineTemplates("", oneTemplateYAML, "team-a")
	if err != nil {
		t.Fatalf("first InjectPipelineTemplates: %v", err)
	}
	out, err = InjectPipelineTemplates(out, oneTemplateYAML, "team-b")
	if err != nil {
		t.Fatalf("second InjectPipelineTemplates: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	unclassified, _ := doc["unclassified"].(map[string]any)
	store, _ := unclassified["varroaPipelineTemplates"].(map[string]any)
	templates, _ := store["templates"].([]any)
	if len(templates) != 2 {
		t.Fatalf("expected both tenant folders to keep their own entry, got %+v", templates)
	}
	folders := map[string]int{}
	for _, tpl := range templates {
		entry := tpl.(map[string]any)
		if entry["name"] != "go-service" || entry["version"] != "1.0.0" {
			t.Errorf("expected both entries to be go-service@1.0.0, got %+v", entry)
		}
		folders[entry["folder"].(string)]++
	}
	if folders["team-a"] != 1 || folders["team-b"] != 1 {
		t.Errorf("expected exactly one entry each for team-a and team-b, got %+v", folders)
	}
}

// TestInjectPipelineTemplates_SameContentControllerAndTenantDedups proves
// round 3's overlap rule: an empty folder is controller-wide and overlaps
// every tenant folder (design D7), so a controller-level fragment (folder
// "") and a placed tenant's fragment (folder "team-a") delivering the
// identical document is a harmless duplicate — exactly one entry
// survives, the controller-wide one, not two.
func TestInjectPipelineTemplates_SameContentControllerAndTenantDedups(t *testing.T) {
	out, err := InjectPipelineTemplates("", oneTemplateYAML, "")
	if err != nil {
		t.Fatalf("first InjectPipelineTemplates: %v", err)
	}
	out, err = InjectPipelineTemplates(out, oneTemplateYAML, "team-a")
	if err != nil {
		t.Fatalf("second InjectPipelineTemplates: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	unclassified, _ := doc["unclassified"].(map[string]any)
	store, _ := unclassified["varroaPipelineTemplates"].(map[string]any)
	templates, _ := store["templates"].([]any)
	if len(templates) != 1 {
		t.Fatalf("expected the overlapping duplicate to collapse into one entry, got %+v", templates)
	}
	entry := templates[0].(map[string]any)
	if entry["folder"] != "" {
		t.Errorf("expected the surviving entry to be the controller-wide one, got folder %v", entry["folder"])
	}
}

func TestInjectPipelineTemplates_SCMDefinitionCarriedNested(t *testing.T) {
	const scmTemplateYAML = `templates:
  - apiVersion: varroa.dev/v1
    kind: PipelineTemplate
    metadata:
      name: go-service
    spec:
      version: 1.0.0
      parameters:
        type: object
        additionalProperties: false
      scmDefinition:
        remote: https://example.invalid/templates.git
        credentialsId: template-repo-creds
        scriptPath: Jenkinsfile
        revision: main
`
	out, err := InjectPipelineTemplates("", scmTemplateYAML, "")
	if err != nil {
		t.Fatalf("InjectPipelineTemplates: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	unclassified, _ := doc["unclassified"].(map[string]any)
	store, _ := unclassified["varroaPipelineTemplates"].(map[string]any)
	templates, _ := store["templates"].([]any)
	entry := templates[0].(map[string]any)
	if _, hasJenkinsfile := entry["jenkinsfile"]; hasJenkinsfile {
		t.Errorf("expected no jenkinsfile key when scmDefinition is set, got %+v", entry)
	}
	scm, ok := entry["scmDefinition"].(map[string]any)
	if !ok {
		t.Fatalf("expected scmDefinition to be a nested map, got %+v", entry["scmDefinition"])
	}
	if scm["remote"] != "https://example.invalid/templates.git" {
		t.Errorf("scmDefinition.remote = %v, want the template repo remote", scm["remote"])
	}
}

// TestEscapeJCasCVariables asserts a body's "${...}" reaches Jenkins as
// itself: JCasC would otherwise substitute it and drop an unresolved
// name, so the injector writes the "^${" literal escape once.
func TestEscapeJCasCVariables(t *testing.T) {
	cases := map[string]string{
		"echo \"${varroaParams.greeting}\"": "echo \"^${varroaParams.greeting}\"",
		"plain text":                        "plain text",
		"already ^${escaped} and ${not}":    "already ^${escaped} and ^${not}",
		"$ alone and {brace}":               "$ alone and {brace}",
	}
	for in, want := range cases {
		if got := escapeJCasCVariables(in); got != want {
			t.Errorf("escapeJCasCVariables(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePipelineTemplates_BlankIsEmpty(t *testing.T) {
	docs, err := ParsePipelineTemplates("  \n")
	if err != nil {
		t.Fatalf("blank input: %v", err)
	}
	if docs != nil {
		t.Errorf("expected nil, got %+v", docs)
	}
}

func TestRenderPipelineTemplates_RoundTrips(t *testing.T) {
	docs, err := ParsePipelineTemplates(oneTemplateYAML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := RenderPipelineTemplates(docs)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	again, err := ParsePipelineTemplates(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if len(again) != len(docs) {
		t.Fatalf("expected %d documents, got %d", len(docs), len(again))
	}
	for i := range docs {
		if again[i].Metadata != docs[i].Metadata {
			t.Errorf("metadata: got %+v, want %+v", again[i].Metadata, docs[i].Metadata)
		}
		if again[i].Spec.Version != docs[i].Spec.Version {
			t.Errorf("version: got %q, want %q", again[i].Spec.Version, docs[i].Spec.Version)
		}
		if again[i].Spec.Jenkinsfile != docs[i].Spec.Jenkinsfile {
			t.Errorf("jenkinsfile: got %q, want %q", again[i].Spec.Jenkinsfile, docs[i].Spec.Jenkinsfile)
		}
	}
	if empty, err := RenderPipelineTemplates(nil); err != nil || empty != "" {
		t.Errorf("empty render: got %q, %v", empty, err)
	}
}
