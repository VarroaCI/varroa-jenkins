package controller

import (
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
)

const multibranchItemsYAML = `items:
  - kind: multibranch
    name: mb
    sourcesList:
      - branchSource:
          source:
            git:
              remote: https://github.com/fake-org/mb.git
    projectFactory:
      workflowBranchProjectFactory:
        scriptPath: Jenkinsfile
`

const pipelineOnlyItemsYAML = `items:
  - kind: pipeline
    name: build
    definition:
      script: "echo hi"
`

// TestAssembleItemsRefusesMissingBranchBuildStrategiesPlugin is task 8.5's
// refusal scenario: a manifest with a multibranch item and a resolved
// plugin set that omits basic-branch-build-strategies fails the assembly,
// naming the plugin and the item path.
func TestAssembleItemsRefusesMissingBranchBuildStrategiesPlugin(t *testing.T) {
	err := checkBranchBuildStrategiesPlugin(multibranchItemsYAML, nil)
	if err == nil {
		t.Fatal("expected the refusal for a multibranch item with no pinned plugin")
	}
	if !strings.Contains(err.Error(), "basic-branch-build-strategies") {
		t.Errorf("expected the error to name the plugin, got: %v", err)
	}
	if !strings.Contains(err.Error(), "mb") {
		t.Errorf("expected the error to name the item path, got: %v", err)
	}
}

// TestAssembleItemsPassesWithNoMultibranchOrOrgFolderItem is task 8.5's
// second scenario: a manifest with no multibranch or organizationFolder
// item is unaffected by a plugin set that omits
// basic-branch-build-strategies.
func TestAssembleItemsPassesWithNoMultibranchOrOrgFolderItem(t *testing.T) {
	if err := checkBranchBuildStrategiesPlugin(pipelineOnlyItemsYAML, nil); err != nil {
		t.Errorf("expected no refusal for a manifest with no multibranch or organizationFolder item, got: %v", err)
	}
}

// TestAssembleItemsPassesForSeededProfile is task 8.5's third scenario: a
// resolved plugin set that carries basic-branch-build-strategies passes.
func TestAssembleItemsPassesForSeededProfile(t *testing.T) {
	coreSet := []pluginlock.PluginEntry{
		{ArtifactID: "basic-branch-build-strategies", Version: "1.0.0"},
	}
	if err := checkBranchBuildStrategiesPlugin(multibranchItemsYAML, coreSet); err != nil {
		t.Errorf("expected no refusal when the resolved plugin set holds the plugin, got: %v", err)
	}
}
