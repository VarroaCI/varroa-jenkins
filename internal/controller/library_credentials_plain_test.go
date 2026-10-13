package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// plainLibraryItem returns one folder item that holds a folder library. The
// library retrieves from the source named by scmSource, which uses credentialID.
func plainLibraryItem(scmSource, credentialID string) string {
	return "items:\n" +
		"- kind: folder\n" +
		"  name: apps\n" +
		"  properties:\n" +
		"  - folderLibraries:\n" +
		"      libraries:\n" +
		"      - libraryConfiguration:\n" +
		"          name: platform-lib\n" +
		"          defaultVersion: v1.0.0\n" +
		"          retriever:\n" +
		"            modernSCM:\n" +
		"              scm:\n" +
		"                " + scmSource + ":\n" +
		"                  remote: https://gitlab.example.com/platform/jenkins-lib.git\n" +
		"                  credentialsId: " + credentialID + "\n"
}

// systemCredentialsJenkinsYAML returns a jenkins.yaml whose system store
// holds the given entries in one domain.
func systemCredentialsJenkinsYAML(entries string) string {
	return "credentials:\n  system:\n    domainCredentials:\n    - credentials:\n" + entries
}

func TestPlainLibraryCredentials_GlobalSystemCredentialPasses(t *testing.T) {
	jcasc := systemCredentialsJenkinsYAML("      - usernamePassword:\n          id: gitlab-lib\n          username: u\n          password: p\n")
	if err := checkPlainFolderLibraryCredentials(plainLibraryItem("git", "gitlab-lib"), jcasc); err != nil {
		t.Fatalf("GLOBAL usernamePassword: %v", err)
	}
}

func TestPlainLibraryCredentials_MissingCredentialRefused(t *testing.T) {
	err := checkPlainFolderLibraryCredentials(plainLibraryItem("git", "missing-cred"), "")
	if err == nil {
		t.Fatal("missing credential passed")
	}
	if !strings.Contains(err.Error(), "missing-cred") || !strings.Contains(err.Error(), "GLOBAL-scope") {
		t.Fatalf("error = %v, want the ID and GLOBAL-scope", err)
	}
}

func TestPlainLibraryCredentials_SystemScopeRefused(t *testing.T) {
	jcasc := systemCredentialsJenkinsYAML("      - usernamePassword:\n          id: gitlab-lib\n          scope: SYSTEM\n          username: u\n          password: p\n")
	err := checkPlainFolderLibraryCredentials(plainLibraryItem("git", "gitlab-lib"), jcasc)
	if err == nil || !strings.Contains(err.Error(), "SYSTEM scope") {
		t.Fatalf("error = %v, want SYSTEM scope refusal", err)
	}
}

func TestPlainLibraryCredentials_WrongKindRefused(t *testing.T) {
	jcasc := systemCredentialsJenkinsYAML("      - string:\n          id: gitlab-lib\n          secret: s\n")
	err := checkPlainFolderLibraryCredentials(plainLibraryItem("git", "gitlab-lib"), jcasc)
	if err == nil || !strings.Contains(err.Error(), "secretText") {
		t.Fatalf("error = %v, want a secretText kind refusal", err)
	}
}

func TestPlainLibraryCredentials_GitHubAppPasses(t *testing.T) {
	jcasc := systemCredentialsJenkinsYAML("      - gitHubApp:\n          id: app-cred\n          appID: \"1\"\n          privateKey: k\n")
	if err := checkPlainFolderLibraryCredentials(plainLibraryItem("github", "app-cred"), jcasc); err != nil {
		t.Fatalf("githubApp on a github source: %v", err)
	}
}

func TestPlainLibraryCredentials_FolderCredentialPasses(t *testing.T) {
	itemsYAML := "items:\n" +
		"- kind: folder\n" +
		"  name: payments\n" +
		"  properties:\n" +
		"  - folderCredentialsProperty:\n" +
		"      folderCredentials:\n" +
		"      - credentials:\n" +
		"        - usernamePassword:\n" +
		"            id: gitlab-lib\n" +
		"            username: u\n" +
		"            password: p\n" +
		"  items:\n" +
		"  - kind: folder\n" +
		"    name: apps\n" +
		"    properties:\n" +
		"    - folderLibraries:\n" +
		"        libraries:\n" +
		"        - libraryConfiguration:\n" +
		"            name: platform-lib\n" +
		"            defaultVersion: v1.0.0\n" +
		"            retriever:\n" +
		"              modernSCM:\n" +
		"                scm:\n" +
		"                  git:\n" +
		"                    remote: https://gitlab.example.com/x.git\n" +
		"                    credentialsId: gitlab-lib\n"
	if err := checkPlainFolderLibraryCredentials(itemsYAML, ""); err != nil {
		t.Fatalf("folder credential of an ancestor: %v", err)
	}
}

func TestPlainLibraryCredentials_NonGlobalDomainCounts(t *testing.T) {
	jcasc := "credentials:\n  system:\n    domainCredentials:\n" +
		"    - domain:\n        name: other\n      credentials:\n" +
		"      - usernamePassword:\n          id: gitlab-lib\n          username: u\n          password: p\n"
	if err := checkPlainFolderLibraryCredentials(plainLibraryItem("git", "gitlab-lib"), jcasc); err != nil {
		t.Fatalf("credential in a second domain: %v", err)
	}
}

func TestPlainLibraryCredentials_UnknownTypeAcceptsAnyKind(t *testing.T) {
	jcasc := systemCredentialsJenkinsYAML("      - sshUserPrivateKey:\n          id: gitlab-lib\n          username: u\n")
	if err := checkPlainFolderLibraryCredentials(plainLibraryItem("git", "gitlab-lib"), jcasc); err != nil {
		t.Fatalf("unrecognised type: %v", err)
	}
}

const unresolvedSystemEntry = "      - usernamePassword:\n          id: \"${varroa_controller_name}-lib\"\n          username: u\n"

func TestPlainLibraryCredentials_UnresolvedIDAllowsUnknownReference(t *testing.T) {
	jcasc := systemCredentialsJenkinsYAML(unresolvedSystemEntry)
	if err := checkPlainFolderLibraryCredentials(plainLibraryItem("git", "missing-cred"), jcasc); err != nil {
		t.Fatalf("unknown ID with an unresolved system ID: %v", err)
	}
}

func TestPlainLibraryCredentials_UnresolvedIDStillChecksKind(t *testing.T) {
	jcasc := systemCredentialsJenkinsYAML(unresolvedSystemEntry + "      - string:\n          id: lib\n          secret: s\n")
	err := checkPlainFolderLibraryCredentials(plainLibraryItem("git", "lib"), jcasc)
	if err == nil || !strings.Contains(err.Error(), "secretText") {
		t.Fatalf("error = %v, want a secretText kind refusal", err)
	}
}

func TestPlainLibraryCredentials_UnresolvedIDStillChecksSystemScope(t *testing.T) {
	jcasc := systemCredentialsJenkinsYAML(unresolvedSystemEntry +
		"      - usernamePassword:\n          id: sys-lib\n          scope: SYSTEM\n          username: u\n          password: p\n")
	err := checkPlainFolderLibraryCredentials(plainLibraryItem("git", "sys-lib"), jcasc)
	if err == nil || !strings.Contains(err.Error(), "SYSTEM scope") {
		t.Fatalf("error = %v, want SYSTEM scope refusal", err)
	}
}

func TestPlainLibraryCredentials_NoLibrariesPasses(t *testing.T) {
	if err := checkPlainFolderLibraryCredentials("", ""); err != nil {
		t.Fatalf("no items: %v", err)
	}
}

func TestResolveBundleRefusesPlainLibraryWithMissingCredential(t *testing.T) {
	client := newTestClientWithBundle()
	client.configMapData["test-bundle-content"]["items.yaml"] = plainLibraryItem("git", "missing-cred")
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	_, _, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err == nil || !strings.Contains(err.Error(), "assemble items") || !strings.Contains(err.Error(), "missing-cred") {
		t.Fatalf("resolveBundleForController error = %v, want assemble items and missing-cred", err)
	}
}
