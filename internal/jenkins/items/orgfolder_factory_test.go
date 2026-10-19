package items

import (
	"strings"
	"testing"
)

func orgFolderWithGitea(factories ...ProjectFactory) Item {
	return Item{
		Kind: "organizationFolder",
		Name: "repos",
		Navigators: []SCMNavigator{{Gitea: &GiteaNavigator{
			ServerURL:     "https://git.example.org",
			RepoOwner:     "acme",
			CredentialsID: "forgejo-ci",
		}}},
		ProjectFactories: factories,
	}
}

func renderOrgFolder(t *testing.T, item Item) string {
	t.Helper()
	xml, err := GenerateConfigXML(item)
	if err != nil {
		t.Fatalf("GenerateConfigXML: %v", err)
	}
	return xml
}

func TestOrgFolderConfigXMLDefaultsFactoryWhenAbsent(t *testing.T) {
	xml := renderOrgFolder(t, orgFolderWithGitea())
	if !strings.Contains(xml, "<projectFactories>") {
		t.Fatalf("projectFactories element missing:\n%s", xml)
	}
	if got := strings.Count(xml, "<scriptPath>"); got != 1 {
		t.Fatalf("want exactly one factory, got %d scriptPath elements:\n%s", got, xml)
	}
	if !strings.Contains(xml, "<scriptPath>Jenkinsfile</scriptPath>") {
		t.Fatalf("default scriptPath missing:\n%s", xml)
	}
}

func TestOrgFolderConfigXMLRendersWorkflowBranchFactory(t *testing.T) {
	item := orgFolderWithGitea(ProjectFactory{
		WorkflowBranch: &WorkflowBranchProjectFactory{ScriptPath: "ci/Jenkinsfile"},
	})
	xml := renderOrgFolder(t, item)
	if !strings.Contains(xml, "WorkflowMultiBranchProjectFactory") {
		t.Fatalf("workflow branch factory not rendered as multibranch class:\n%s", xml)
	}
	if !strings.Contains(xml, "<scriptPath>ci/Jenkinsfile</scriptPath>") {
		t.Fatalf("scriptPath not rendered:\n%s", xml)
	}
}

func TestOrgFolderConfigXMLEmptyScriptPathDefaults(t *testing.T) {
	item := orgFolderWithGitea(ProjectFactory{MultiBranch: &MultiBranchProjectFactory{}})
	xml := renderOrgFolder(t, item)
	if !strings.Contains(xml, "<scriptPath>Jenkinsfile</scriptPath>") {
		t.Fatalf("empty scriptPath did not default to Jenkinsfile:\n%s", xml)
	}
}

func TestOrgFolderValidateRejectsVarroaMarkerFactory(t *testing.T) {
	item := orgFolderWithGitea(ProjectFactory{VarroaMarker: &VarroaMarkerProjectFactory{}})
	err := item.Validate()
	if err == nil || !strings.Contains(err.Error(), "varroaMarkerProjectFactory") {
		t.Fatalf("want varroaMarkerProjectFactory error, got %v", err)
	}
}

func TestOrgFolderValidateRejectsEmptyFactoryEntry(t *testing.T) {
	item := orgFolderWithGitea(ProjectFactory{})
	err := item.Validate()
	if err == nil || !strings.Contains(err.Error(), "must set exactly one") {
		t.Fatalf("want exactly-one error, got %v", err)
	}
}

func TestOrgFolderValidateRejectsTwoFactoryKeys(t *testing.T) {
	item := orgFolderWithGitea(ProjectFactory{
		MultiBranch:    &MultiBranchProjectFactory{ScriptPath: "Jenkinsfile"},
		WorkflowBranch: &WorkflowBranchProjectFactory{ScriptPath: "Jenkinsfile"},
	})
	err := item.Validate()
	if err == nil || !strings.Contains(err.Error(), "must set exactly one") {
		t.Fatalf("want exactly-one error, got %v", err)
	}
}

func TestDocsGiteaOrgFolderExampleWithoutFactoriesRenders(t *testing.T) {
	const yamlContent = `items:
  - kind: organizationFolder
    name: repos
    navigators:
      - giteaNavigator:
          serverUrl: https://git.example.org
          repoOwner: sbsoftware
          credentialsId: forgejo-ci
`
	m, err := Parse(yamlContent)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Items) != 1 {
		t.Fatalf("want one item, got %d", len(m.Items))
	}
	xml := renderOrgFolder(t, m.Items[0])
	if !strings.Contains(xml, "<projectFactories>") || !strings.Contains(xml, "<scriptPath>Jenkinsfile</scriptPath>") {
		t.Fatalf("issue example did not render a default factory:\n%s", xml)
	}
}
