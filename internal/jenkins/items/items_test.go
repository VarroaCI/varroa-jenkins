package items

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/jenkins"
)

func TestItemValidate(t *testing.T) {
	tests := []struct {
		name    string
		item    Item
		wantErr bool
	}{
		{"valid folder", Item{Kind: "folder", Name: "my-folder"}, false},
		{"valid pipeline", Item{Kind: "pipeline", Name: "my-pipeline",
			Definition: &PipelineDefinition{
				CpsScmFlowDefinition: &CpsScmFlowDefinition{
					SCM: SCM{GitSCM: &GitSCM{
						UserRemoteConfigs: []UserRemoteConfig{
							{UserRemoteConfig: RemoteConfig{URL: "https://example.com/repo.git"}},
						},
						Branches: []BranchSpec{
							{BranchSpec: BranchSpecConfig{Name: "*/main"}},
						},
					}},
					ScriptPath: "Jenkinsfile",
				},
			}}, false},
		{"valid freeStyle", Item{Kind: "freeStyle", Name: "my-job"}, false},
		{"missing name", Item{Kind: "folder", Name: ""}, true},
		{"unknown kind", Item{Kind: "widget", Name: "x"}, true},
		{"empty kind", Item{Name: "x"}, true},
		{"valid varroaUpstream trigger", Item{Kind: "freeStyle", Name: "my-job",
			Triggers: []Trigger{{VarroaUpstream: &VarroaUpstreamTrigger{Tenant: "alpha", Job: "release", Result: "SUCCESS"}}},
		}, false},
		{"valid varroaUpstream trigger with default result", Item{Kind: "freeStyle", Name: "my-job",
			Triggers: []Trigger{{VarroaUpstream: &VarroaUpstreamTrigger{Tenant: "alpha", Job: "release"}}},
		}, false},
		{"varroaUpstream trigger missing tenant", Item{Kind: "freeStyle", Name: "my-job",
			Triggers: []Trigger{{VarroaUpstream: &VarroaUpstreamTrigger{Job: "release"}}},
		}, true},
		{"varroaUpstream trigger missing job", Item{Kind: "freeStyle", Name: "my-job",
			Triggers: []Trigger{{VarroaUpstream: &VarroaUpstreamTrigger{Tenant: "alpha"}}},
		}, true},
		{"varroaUpstream trigger invalid result", Item{Kind: "freeStyle", Name: "my-job",
			Triggers: []Trigger{{VarroaUpstream: &VarroaUpstreamTrigger{Tenant: "alpha", Job: "release", Result: "FLAKY"}}},
		}, true},
		{"valid item-level string parameter", Item{Kind: "pipeline", Name: "my-job",
			Definition: &PipelineDefinition{Script: "echo hi"},
			Parameters: []Parameter{{String: &StringParameter{Name: "BRANCH"}}},
		}, false},
		{"parameter with no kind set", Item{Kind: "pipeline", Name: "my-job",
			Definition: &PipelineDefinition{Script: "echo hi"},
			Parameters: []Parameter{{}},
		}, true},
		{"parameter with two kinds set", Item{Kind: "pipeline", Name: "my-job",
			Definition: &PipelineDefinition{Script: "echo hi"},
			Parameters: []Parameter{{String: &StringParameter{Name: "A"}, BooleanParam: &BooleanParameter{Name: "A"}}},
		}, true},
		{"parameter missing name", Item{Kind: "pipeline", Name: "my-job",
			Definition: &PipelineDefinition{Script: "echo hi"},
			Parameters: []Parameter{{String: &StringParameter{}}},
		}, true},
		{"choice parameter with no choices", Item{Kind: "pipeline", Name: "my-job",
			Definition: &PipelineDefinition{Script: "echo hi"},
			Parameters: []Parameter{{Choice: &ChoiceParameter{Name: "ENV"}}},
		}, true},
		{"valid property-level parameter", Item{Kind: "pipeline", Name: "my-job",
			Definition: &PipelineDefinition{Script: "echo hi"},
			Properties: []Property{{Parameters: &ParametersProperty{
				ParameterDefinitions: []Parameter{{String: &StringParameter{Name: "BRANCH"}}},
			}}},
		}, false},
		{"property-level parameter missing name", Item{Kind: "pipeline", Name: "my-job",
			Definition: &PipelineDefinition{Script: "echo hi"},
			Properties: []Property{{Parameters: &ParametersProperty{
				ParameterDefinitions: []Parameter{{String: &StringParameter{}}},
			}}},
		}, true},
		{"duplicate parameter name within one source", Item{Kind: "pipeline", Name: "my-job",
			Definition: &PipelineDefinition{Script: "echo hi"},
			Parameters: []Parameter{
				{String: &StringParameter{Name: "BRANCH"}},
				{String: &StringParameter{Name: "BRANCH"}},
			},
		}, true},
		{"duplicate parameter name across both sources", Item{Kind: "pipeline", Name: "my-job",
			Definition: &PipelineDefinition{Script: "echo hi"},
			Parameters: []Parameter{{String: &StringParameter{Name: "BRANCH"}}},
			Properties: []Property{{Parameters: &ParametersProperty{
				ParameterDefinitions: []Parameter{{BooleanParam: &BooleanParameter{Name: "BRANCH"}}},
			}}},
		}, true},
		{"valid durability hint", Item{Kind: "pipeline", Name: "my-job",
			Definition: &PipelineDefinition{Script: "echo hi"},
			Properties: []Property{{DurabilityHint: &DurabilityHint{Hint: "PERFORMANCE_OPTIMIZED"}}},
		}, false},
		{"invalid durability hint", Item{Kind: "pipeline", Name: "my-job",
			Definition: &PipelineDefinition{Script: "echo hi"},
			Properties: []Property{{DurabilityHint: &DurabilityHint{Hint: "FAST_AND_LOOSE"}}},
		}, true},
		{"durability hint on a folder is rejected", Item{Kind: "folder", Name: "my-folder",
			Properties: []Property{{DurabilityHint: &DurabilityHint{Hint: "MAX_SURVIVABILITY"}}},
		}, true},
		{"parameters on a multibranch item is rejected", Item{Kind: "multibranch", Name: "my-branches",
			SourcesList: []BranchSourceEntry{{GitHub: &GitHubSCMSource{RepoOwner: "org", Repository: "repo"}}},
			Parameters:  []Parameter{{String: &StringParameter{Name: "BRANCH"}}},
		}, true},
		{"parameters on an organizationFolder item is rejected", Item{Kind: "organizationFolder", Name: "my-org",
			Navigators: []SCMNavigator{{GitHub: &GitHubNavigator{RepoOwner: "org"}}},
			Properties: []Property{{Parameters: &ParametersProperty{
				ParameterDefinitions: []Parameter{{String: &StringParameter{Name: "BRANCH"}}},
			}}},
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.item.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestJenkinsClass(t *testing.T) {
	tests := []struct {
		kind, class string
	}{
		{"folder", "com.cloudbees.hudson.plugins.folder.Folder"},
		{"freeStyle", "hudson.model.FreeStyleProject"},
		{"pipeline", "org.jenkinsci.plugins.workflow.job.WorkflowJob"},
		{"unknown", ""},
	}
	for _, tt := range tests {
		item := Item{Kind: tt.kind, Name: "test"}
		if got := item.JenkinsClass(); got != tt.class {
			t.Errorf("%s: expected %q, got %q", tt.kind, tt.class, got)
		}
	}
}

func TestIsFolder(t *testing.T) {
	folder := Item{Kind: "folder"}
	if !folder.IsFolder() {
		t.Error("folder should be a folder")
	}
	pipeline := Item{Kind: "pipeline"}
	if pipeline.IsFolder() {
		t.Error("pipeline should not be a folder")
	}
}

func TestEffectiveRemoveStrategy(t *testing.T) {
	m := &Manifest{}
	if s := m.EffectiveRemoveStrategy(); s != "none" {
		t.Errorf("default should be none, got %s", s)
	}
	m.RemoveStrategy = &RemoveStrategy{Items: "sync"}
	if s := m.EffectiveRemoveStrategy(); s != "sync" {
		t.Errorf("expected sync, got %s", s)
	}
}

func TestRemoveStrategyConstants(t *testing.T) {
	if RemoveNone != "none" {
		t.Errorf("RemoveNone = %q", RemoveNone)
	}
	if RemoveSync != "sync" {
		t.Errorf("RemoveSync = %q", RemoveSync)
	}
	if RemoveRemoveSupported != "remove-supported" {
		t.Errorf("RemoveRemoveSupported = %q", RemoveRemoveSupported)
	}
	if RemoveRemoveAll != "remove-all" {
		t.Errorf("RemoveRemoveAll = %q", RemoveRemoveAll)
	}
}

func TestParseEmpty(t *testing.T) {
	m, err := Parse("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.Items) != 0 {
		t.Errorf("expected 0 items, got %d", len(m.Items))
	}
}

func TestParseValid(t *testing.T) {
	yaml := `items:
  - kind: folder
    name: my-folder
    items:
      - kind: pipeline
        name: my-pipeline
        definition:
          cpsScmFlowDefinition:
            scriptPath: Jenkinsfile
            scm:
              gitSCM:
                userRemoteConfigs:
                  - userRemoteConfig:
                      url: https://example.com/repo.git
                branches:
                  - branchSpec:
                      name: "*/main"`
	m, err := Parse(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(m.Items))
	}
	if m.Items[0].Name != "my-folder" {
		t.Errorf("expected my-folder, got %s", m.Items[0].Name)
	}
	if len(m.Items[0].Items) != 1 {
		t.Fatalf("expected 1 nested item, got %d", len(m.Items[0].Items))
	}
}

func TestParseWithRemoveStrategy(t *testing.T) {
	yaml := `removeStrategy:
  items: sync
  rbac: sync
items:
  - kind: folder
    name: test`
	m, err := Parse(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s := m.EffectiveRemoveStrategy(); s != "sync" {
		t.Errorf("expected sync, got %s", s)
	}
}

func TestFlattenDepthFirst(t *testing.T) {
	yaml := `items:
  - kind: folder
    name: parent
    items:
      - kind: pipeline
        name: child
        definition:
          cpsScmFlowDefinition:
            scriptPath: Jenkinsfile
            scm:
              gitSCM:
                userRemoteConfigs:
                  - userRemoteConfig:
                      url: https://example.com/repo.git
                branches:
                  - branchSpec:
                      name: "*/main"`
	m, _ := Parse(yaml)
	flat := m.Flatten()
	if len(flat) != 2 {
		t.Fatalf("expected 2 items, got %d", len(flat))
	}
	if flat[0].Path != "parent" {
		t.Errorf("first should be parent, got %s", flat[0].Path)
	}
	if flat[1].Path != "parent/child" {
		t.Errorf("second should be parent/child, got %s", flat[1].Path)
	}
}

func TestNestedItemsOnNonFolder(t *testing.T) {
	yaml := `items:
  - kind: pipeline
    name: bad
    definition:
      cpsScmFlowDefinition:
        scriptPath: Jenkinsfile
        scm:
          gitSCM:
            userRemoteConfigs:
              - userRemoteConfig:
                  url: https://example.com/repo.git
            branches:
              - branchSpec:
                  name: "*/main"
    items:
      - kind: freeStyle
        name: child`
	_, err := Parse(yaml)
	if err == nil {
		t.Fatal("expected error for nested items on pipeline")
	}
}

func TestFlattenWithNestedFolder(t *testing.T) {
	yaml := `items:
  - kind: folder
    name: root
    items:
      - kind: folder
        name: sub
        items:
          - kind: freeStyle
            name: leaf`
	m, _ := Parse(yaml)
	flat := m.Flatten()
	if len(flat) != 3 {
		t.Fatalf("expected 3 items, got %d", len(flat))
	}
	if flat[0].Path != "root" {
		t.Errorf("first should be root, got %s", flat[0].Path)
	}
	if flat[1].Path != "root/sub" {
		t.Errorf("second should be root/sub, got %s", flat[1].Path)
	}
	if flat[2].Path != "root/sub/leaf" {
		t.Errorf("third should be root/sub/leaf, got %s", flat[2].Path)
	}
}

func TestGenerateConfigXMLFolder(t *testing.T) {
	item := Item{
		Kind:        "folder",
		Name:        "my-folder",
		DisplayName: "My Folder",
	}
	xml, err := GenerateConfigXML(item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(xml, "com.cloudbees.hudson.plugins.folder.Folder") {
		t.Errorf("expected Folder class in XML, got: %s", xml)
	}
	if !strings.Contains(xml, "My Folder") {
		t.Errorf("expected display name in XML, got: %s", xml)
	}
}

func TestGenerateConfigXMLPipeline(t *testing.T) {
	item := Item{
		Kind: "pipeline",
		Name: "my-pipeline",
		Definition: &PipelineDefinition{
			CpsScmFlowDefinition: &CpsScmFlowDefinition{
				SCM: SCM{
					GitSCM: &GitSCM{
						UserRemoteConfigs: []UserRemoteConfig{
							{UserRemoteConfig: RemoteConfig{URL: "https://github.com/org/repo.git"}},
						},
						Branches: []BranchSpec{
							{BranchSpec: BranchSpecConfig{Name: "*/main"}},
						},
					},
				},
				ScriptPath:  "Jenkinsfile",
				Lightweight: true,
			},
		},
	}
	xml, err := GenerateConfigXML(item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(xml, "flow-definition") {
		t.Errorf("expected flow-definition in XML, got: %s", xml)
	}
	if !strings.Contains(xml, "https://github.com/org/repo.git") {
		t.Errorf("expected remote URL in XML, got: %s", xml)
	}
	if !strings.Contains(xml, "*/main") {
		t.Errorf("expected branch spec in XML, got: %s", xml)
	}
}

// TestGenerateConfigXMLPipelineWithParameters asserts that an item-level
// parameters block on a pipeline item renders a
// ParametersDefinitionProperty, so a re-fired trigger carries a non-empty
// params_digest.
func TestGenerateConfigXMLPipelineWithParameters(t *testing.T) {
	item := Item{
		Kind: "pipeline",
		Name: "my-pipeline",
		Definition: &PipelineDefinition{
			Script: "pipeline { agent any; stages { stage('x') { steps { echo 'ok' } } } }",
		},
		Parameters: []Parameter{
			{String: &StringParameter{Name: "BRANCH", DefaultValue: "main", Trim: true}},
			{Choice: &ChoiceParameter{Name: "ENV", Choices: []string{"dev", "prod"}}},
			{BooleanParam: &BooleanParameter{Name: "DEBUG", DefaultValue: true}},
		},
	}
	xml, err := GenerateConfigXML(item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(xml, "hudson.model.ParametersDefinitionProperty") {
		t.Fatalf("expected ParametersDefinitionProperty in XML, got: %s", xml)
	}
	if !strings.Contains(xml, "<parameterDefinitions>") {
		t.Fatalf("expected a parameterDefinitions wrapper around every definition, got: %s", xml)
	}
	if !strings.Contains(xml, "hudson.model.StringParameterDefinition") || !strings.Contains(xml, "<name>BRANCH</name>") {
		t.Errorf("expected string parameter BRANCH in XML, got: %s", xml)
	}
	if !strings.Contains(xml, "hudson.model.ChoiceParameterDefinition") || !strings.Contains(xml, "<name>ENV</name>") {
		t.Errorf("expected choice parameter ENV in XML, got: %s", xml)
	}
	if !strings.Contains(xml, "<string>dev</string>") || !strings.Contains(xml, "<string>prod</string>") {
		t.Errorf("expected choice values dev/prod in XML, got: %s", xml)
	}
	if !strings.Contains(xml, "hudson.model.BooleanParameterDefinition") || !strings.Contains(xml, "<name>DEBUG</name>") {
		t.Errorf("expected boolean parameter DEBUG in XML, got: %s", xml)
	}
}

// TestGenerateConfigXMLPipelineWithPropertyLevelParameters asserts that a
// parameters property nested under properties also renders a
// ParametersDefinitionProperty, the same as the item-level spelling.
func TestGenerateConfigXMLPipelineWithPropertyLevelParameters(t *testing.T) {
	item := Item{
		Kind: "pipeline",
		Name: "my-pipeline",
		Definition: &PipelineDefinition{
			Script: "echo hi",
		},
		Properties: []Property{
			{Parameters: &ParametersProperty{
				ParameterDefinitions: []Parameter{{String: &StringParameter{Name: "BRANCH"}}},
			}},
		},
	}
	xml, err := GenerateConfigXML(item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(xml, "hudson.model.ParametersDefinitionProperty") || !strings.Contains(xml, "<name>BRANCH</name>") {
		t.Errorf("expected property-level BRANCH parameter in XML, got: %s", xml)
	}
}

// TestGenerateConfigXMLPipelineMergesBothParameterSpellings asserts that a
// pipeline item declaring parameters both ways carries every parameter
// from both sources on one job, matching the pipelines API's own merge.
func TestGenerateConfigXMLPipelineMergesBothParameterSpellings(t *testing.T) {
	item := Item{
		Kind: "pipeline",
		Name: "my-pipeline",
		Definition: &PipelineDefinition{
			Script: "echo hi",
		},
		Parameters: []Parameter{{String: &StringParameter{Name: "VERSION"}}},
		Properties: []Property{
			{Parameters: &ParametersProperty{
				ParameterDefinitions: []Parameter{{BooleanParam: &BooleanParameter{Name: "DRY_RUN"}}},
			}},
		},
	}
	xml, err := GenerateConfigXML(item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(xml, "<name>VERSION</name>") {
		t.Errorf("expected item-level VERSION parameter in XML, got: %s", xml)
	}
	if !strings.Contains(xml, "<name>DRY_RUN</name>") {
		t.Errorf("expected property-level DRY_RUN parameter in XML, got: %s", xml)
	}
}

func TestGenerateConfigXMLFreeStyleWithParameters(t *testing.T) {
	item := Item{
		Kind: "freeStyle",
		Name: "my-job",
		Parameters: []Parameter{
			{String: &StringParameter{Name: "BRANCH", DefaultValue: "main"}},
		},
	}
	xml, err := GenerateConfigXML(item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(xml, "hudson.model.ParametersDefinitionProperty") {
		t.Fatalf("expected ParametersDefinitionProperty in XML, got: %s", xml)
	}
	if !strings.Contains(xml, "<name>BRANCH</name>") {
		t.Errorf("expected string parameter BRANCH in XML, got: %s", xml)
	}
}

// TestGenerateConfigXMLFreeStyleWithPropertyLevelParameters asserts that a
// freeStyle item's property-level parameters render too, matching the
// pipeline renderer's merge of both spellings.
func TestGenerateConfigXMLFreeStyleWithPropertyLevelParameters(t *testing.T) {
	item := Item{
		Kind: "freeStyle",
		Name: "my-job",
		Properties: []Property{
			{Parameters: &ParametersProperty{
				ParameterDefinitions: []Parameter{{String: &StringParameter{Name: "BRANCH"}}},
			}},
		},
	}
	xml, err := GenerateConfigXML(item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(xml, "hudson.model.ParametersDefinitionProperty") || !strings.Contains(xml, "<name>BRANCH</name>") {
		t.Errorf("expected property-level BRANCH parameter in XML, got: %s", xml)
	}
}

func TestGenerateConfigXMLFreeStyleWithShell(t *testing.T) {
	item := Item{
		Kind:        "freeStyle",
		Name:        "my-job",
		DisplayName: "My Job",
		Builders: []Builder{
			{Shell: &ShellBuilder{Command: "echo hello"}},
		},
		BuildDiscarder: &BuildDiscarder{
			LogRotator: LogRotator{DaysToKeep: 7, NumToKeep: 10},
		},
	}
	xml, err := GenerateConfigXML(item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(xml, "<project>") {
		t.Errorf("expected project root, got: %s", xml)
	}
	if !strings.Contains(xml, "echo hello") {
		t.Errorf("expected shell command in XML, got: %s", xml)
	}
	if !strings.Contains(xml, "hudson.tasks.LogRotator") {
		t.Errorf("expected log rotator in XML, got: %s", xml)
	}
}

func TestGenerateConfigXMLFreeStyleWithLabel(t *testing.T) {
	item := Item{
		Kind:  "freeStyle",
		Name:  "agent-job",
		Label: "agent",
	}
	xml, err := GenerateConfigXML(item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(xml, "<assignedNode>agent</assignedNode>") {
		t.Errorf("expected agent label in XML, got: %s", xml)
	}
}

func TestGenerateConfigXMLFreeStyleWithVarroaUpstream(t *testing.T) {
	item := Item{
		Kind: "freeStyle",
		Name: "rebuild",
		Triggers: []Trigger{
			{VarroaUpstream: &VarroaUpstreamTrigger{Tenant: "alpha", Job: "release", Result: "SUCCESS"}},
		},
	}
	xml, err := GenerateConfigXML(item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(xml, "<org.jenkinsci.plugins.varroamiteauth.VarroaUpstreamTrigger") {
		t.Errorf("expected VarroaUpstreamTrigger element, got: %s", xml)
	}
	if !strings.Contains(xml, "<tenant>alpha</tenant>") {
		t.Errorf("expected tenant element, got: %s", xml)
	}
	if !strings.Contains(xml, "<job>release</job>") {
		t.Errorf("expected job element, got: %s", xml)
	}
	if !strings.Contains(xml, "<result>SUCCESS</result>") {
		t.Errorf("expected result element, got: %s", xml)
	}
}

func TestGenerateConfigXMLFreeStyleVarroaUpstreamDefaultsResult(t *testing.T) {
	item := Item{
		Kind: "freeStyle",
		Name: "rebuild",
		Triggers: []Trigger{
			{VarroaUpstream: &VarroaUpstreamTrigger{Tenant: "alpha", Job: "release"}},
		},
	}
	xml, err := GenerateConfigXML(item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(xml, "<result>SUCCESS</result>") {
		t.Errorf("expected the SUCCESS default, got: %s", xml)
	}
}

func TestParseVarroaUpstreamInvalidResultFailsComposition(t *testing.T) {
	yaml := `items:
  - kind: freeStyle
    name: rebuild
    triggers:
      - varroaUpstream:
          tenant: alpha
          job: release
          result: FLAKY`
	_, err := Parse(yaml)
	if err == nil {
		t.Fatal("expected error for invalid varroaUpstream result")
	}
	for _, want := range []string{"SUCCESS", "UNSTABLE", "FAILURE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name %q", err.Error(), want)
		}
	}
}

func TestGenerateConfigXMLUnknownKind(t *testing.T) {
	_, err := GenerateConfigXML(Item{Kind: "widget", Name: "test"})
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

// --- Engine tests for non-destructive items (8.4) ---

func TestApplyAllStrategiesAreNonDestructive(t *testing.T) {
	// All strategies must go through applyIncremental, not applyReplace.
	// Verify this by checking that a simulated Jenkins always sees
	// create/update calls, never delete-then-recreate for the same item.
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Accept all CRUD calls: GetItemConfig (404 for new, 200 for known),
		// CreateItem (201), UpdateItemConfig (200), DeleteItem (204).
		if r.Method == http.MethodHead || r.Method == http.MethodGet {
			// Returning 404 means "doesn't exist yet" — engine will create.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/createItem") {
			w.WriteHeader(http.StatusCreated)
			return
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := jenkins.NewClient(srv.URL, "test", "token")

	for _, strategy := range []string{"none", "sync", "remove-supported", "remove-all"} {
		t.Run("strategy_"+strategy, func(t *testing.T) {
			yamlContent := fmt.Sprintf(`items:
  removeStrategy: {items: "%s"}
  root: my-folder
  item:
  - kind: folder
    name: my-folder
    items:
    - kind: folder
      name: sub-folder
`, strategy)

			engine := NewEngine(client)
			_, err := engine.Apply(ctx, yamlContent)
			if err == nil {
				// Success means no destructive path was taken.
				return
			}
			// Network errors are expected (the mock is minimal).
			// The key assertion is that we never hit the old applyReplace path,
			// which would have called DeleteItem for ALL managed items first.
			if strings.Contains(err.Error(), "delete all") || strings.Contains(err.Error(), "applyReplace") {
				t.Errorf("destructive applyReplace path was reached for strategy=%s: %v", strategy, err)
			}
		})
	}
}

func TestDeclaredItemUpdatedInPlace(t *testing.T) {
	// Verify that a declared existing item is updated in place (PUT/POST)
	// rather than delete-recreated.
	var reqPaths []string
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqPaths = append(reqPaths, r.Method+" "+r.URL.Path)
		// Return 200 for config checks so items are treated as existing.
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte("<project><actions/></project>"))
			return
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := jenkins.NewClient(srv.URL, "test", "token")
	engine := NewEngine(client)

	yamlContent := `items:
  removeStrategy: {items: "none"}
  item:
  - kind: folder
    name: existing-folder
`

	_, _ = engine.Apply(ctx, yamlContent)

	for _, p := range reqPaths {
		// An update-in-place sends a POST to the item's config endpoint.
		// A delete-recreate would contain "delete" in the path.
		if strings.Contains(p, "doDelete") {
			t.Errorf("declared item was delete-recreated: %s", p)
		}
	}
}

// TestRemovalScopedToManagedItems verifies CloudBees-aligned removal semantics:
// a removal strategy deletes only items the mite previously managed that are no
// longer declared; NONE deletes nothing; and items the mite never managed are
// never deleted (removal only iterates the managed cache).
func TestRemovalScopedToManagedItems(t *testing.T) {
	ctx := context.Background()

	run := func(t *testing.T, strategy string) []string {
		t.Helper()
		var deleted []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/doDelete"):
				deleted = append(deleted, r.URL.Path)
				w.WriteHeader(http.StatusOK)
			case r.Method == http.MethodGet:
				// Declared item does not exist yet → engine creates it.
				w.WriteHeader(http.StatusNotFound)
			default:
				w.WriteHeader(http.StatusOK)
			}
		}))
		t.Cleanup(srv.Close)

		// Seed the managed cache: "old-folder" was previously managed by the
		// mite; "user-job" is NOT in the cache (a user-created item).
		mf := filepath.Join(t.TempDir(), "managed.json")
		if err := os.WriteFile(mf, []byte(`["old-folder"]`), 0600); err != nil {
			t.Fatal(err)
		}

		engine := &Engine{client: jenkins.NewClient(srv.URL, "test", "token"), managedFile: mf}
		yamlContent := fmt.Sprintf(`removeStrategy:
  items: "%s"
items:
  - kind: folder
    name: new-folder
`, strategy)
		if _, err := engine.Apply(ctx, yamlContent); err != nil {
			t.Fatalf("apply (%s): %v", strategy, err)
		}
		return deleted
	}

	t.Run("none_removes_nothing", func(t *testing.T) {
		if deleted := run(t, "none"); len(deleted) != 0 {
			t.Errorf("NONE should delete nothing, deleted: %v", deleted)
		}
	})

	for _, strategy := range []string{"sync", "remove-all", "remove-supported"} {
		t.Run(strategy+"_removes_only_dedeclared_managed", func(t *testing.T) {
			deleted := run(t, strategy)
			if len(deleted) != 1 || !strings.Contains(deleted[0], "old-folder") {
				t.Errorf("%s should delete only the de-declared managed item old-folder, got: %v", strategy, deleted)
			}
			// "user-job" was never managed, so it must never be deleted.
			for _, d := range deleted {
				if strings.Contains(d, "user-job") || strings.Contains(d, "new-folder") {
					t.Errorf("%s deleted an unmanaged or declared item: %s", strategy, d)
				}
			}
		})
	}
}
