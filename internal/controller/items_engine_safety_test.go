package controller

import (
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
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

const folderLibraryItemsYAML = `items:
  - kind: folder
    name: apps
    properties:
      - folderLibraries:
          libraries:
            - libraryConfiguration:
                name: platform-lib
                defaultVersion: v1.0.0
                retriever:
                  modernSCM:
                    scm:
                      git:
                        remote: https://gitlab.example.com/platform/jenkins-lib.git
`

func TestFolderLibraryPlugins_RefusesMissingPlugin(t *testing.T) {
	err := checkFolderLibraryPlugins(folderLibraryItemsYAML, nil)
	if err == nil {
		t.Fatal("expected a refusal for a library with no plugins pinned")
	}
	for _, want := range []string{"pipeline-groovy-lib", `"apps"`, "platform-lib"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestFolderLibraryPlugins_RefusesMissingGitPlugin(t *testing.T) {
	coreSet := []pluginlock.PluginEntry{{ArtifactID: "pipeline-groovy-lib", Version: "1"}}
	err := checkFolderLibraryPlugins(folderLibraryItemsYAML, coreSet)
	if err == nil {
		t.Fatal("expected a refusal for a git library without the git plugin")
	}
	if !strings.Contains(err.Error(), `"git"`) {
		t.Errorf("error %q does not name the git plugin", err)
	}
}

func TestFolderLibraryPlugins_PassesWhenPinned(t *testing.T) {
	coreSet := []pluginlock.PluginEntry{
		{ArtifactID: "pipeline-groovy-lib", Version: "1"},
		{ArtifactID: "git", Version: "1"},
	}
	if err := checkFolderLibraryPlugins(folderLibraryItemsYAML, coreSet); err != nil {
		t.Fatalf("expected no refusal, got %v", err)
	}
}

const giteaSourceItemsYAML = `items:
  - kind: multibranch
    name: mb
    sourcesList:
      - branchSource:
          source:
            gitea:
              serverUrl: https://git.example.org
              repoOwner: acme
              repository: app
    projectFactory:
      workflowBranchProjectFactory:
        scriptPath: Jenkinsfile
`

// TestCheckGiteaPluginManifest_RefusesWhenUnpinned covers a gitea source
// whose resolved plugin set omits the gitea plugin.
func TestCheckGiteaPluginManifest_RefusesWhenUnpinned(t *testing.T) {
	coreSet := []pluginlock.PluginEntry{{ArtifactID: "git", Version: "1"}}
	err := checkGiteaPlugin(giteaSourceItemsYAML, coreSet)
	if err == nil {
		t.Fatal("expected a refusal for a gitea source without the gitea plugin")
	}
	if !strings.Contains(err.Error(), "gitea") {
		t.Fatalf("refusal should name the plugin, got %v", err)
	}
}

// TestCheckGiteaPluginManifest_PassesWhenPinned covers a gitea navigator and
// a gitea source when the gitea plugin is pinned.
func TestCheckGiteaPluginManifest_PassesWhenPinned(t *testing.T) {
	coreSet := []pluginlock.PluginEntry{{ArtifactID: "gitea", Version: "1"}}
	if err := checkGiteaPlugin(giteaSourceItemsYAML, coreSet); err != nil {
		t.Fatalf("expected no refusal, got %v", err)
	}
	manifest, err := items.Parse(`items:
  - kind: organizationFolder
    name: org
    navigators:
      - giteaNavigator:
          serverUrl: https://git.example.org
          repoOwner: acme
`)
	if err != nil {
		t.Fatalf("parse navigator manifest: %v", err)
	}
	if err := checkGiteaPluginManifest(manifest, coreSet); err != nil {
		t.Fatalf("expected no refusal for a pinned navigator, got %v", err)
	}
	unpinned := []pluginlock.PluginEntry{{ArtifactID: "git", Version: "1"}}
	if err := checkGiteaPluginManifest(manifest, unpinned); err == nil {
		t.Fatal("expected a refusal for a gitea navigator without the gitea plugin")
	}
}
