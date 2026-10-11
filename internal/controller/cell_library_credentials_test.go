package controller

import (
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
)

// libraryFolderManifest returns a manifest with a tenant folder that holds
// one child folder "apps". The child carries the library. The tenant folder
// carries tenantProps.
func libraryFolderManifest(tenantProps []items.Property, lib items.LibraryConfiguration) *items.Manifest {
	childProps := []items.Property{{
		FolderLibraries: &items.FolderLibrariesProperty{Libraries: []items.LibraryConfig{{LibraryConfiguration: lib}}},
	}}
	return &items.Manifest{Items: []items.Item{{
		Kind: "folder", Name: "payments", Properties: tenantProps,
		Items: []items.Item{{Kind: "folder", Name: "apps", Properties: childProps}},
	}}}
}

func gitLibraryFor(credentialID string) items.LibraryConfiguration {
	return items.LibraryConfiguration{
		Name: "platform-lib", DefaultVersion: "v1.0.0",
		Retriever: items.LibraryRetriever{ModernSCM: &items.ModernSCMRetriever{
			SCM: items.LibrarySCM{Git: &items.GitSCMSource{
				Remote: "https://gitlab.example.com/platform/jenkins-lib.git", CredentialsID: credentialID,
			}},
		}},
	}
}

func userPassEntries(id string) []items.FolderCredentialEntry {
	return []items.FolderCredentialEntry{{Credentials: []items.FolderCredential{{
		UsernamePassword: &items.UsernamePasswordCredential{ID: id, Username: "u", Password: "p"},
	}}}}
}

func secretTextEntries(id string) []items.FolderCredentialEntry {
	return []items.FolderCredentialEntry{{Credentials: []items.FolderCredential{{
		SecretText: &items.SecretTextCredential{ID: id, Secret: "s"},
	}}}}
}

func TestCheckFolderLibraryCredentials_ProjectedIDPasses(t *testing.T) {
	m := libraryFolderManifest(nil, gitLibraryFor("gitlab-lib"))
	if err := checkFolderLibraryCredentials(m, userPassEntries("gitlab-lib")); err != nil {
		t.Fatalf("expected a projected credential to pass, got %v", err)
	}
}

func TestCheckFolderLibraryCredentials_AncestorFolderCredentialPasses(t *testing.T) {
	tenantProps := []items.Property{{FolderCredentialsProperty: &items.FolderCredentialsProperty{
		FolderCredentials: userPassEntries("gitlab-lib"),
	}}}
	m := libraryFolderManifest(tenantProps, gitLibraryFor("gitlab-lib"))
	if err := checkFolderLibraryCredentials(m, nil); err != nil {
		t.Fatalf("expected an ancestor folder credential to pass, got %v", err)
	}
}

func TestCheckFolderLibraryCredentials_SiblingFolderCredentialFails(t *testing.T) {
	sibling := &items.Manifest{Items: []items.Item{{
		Kind: "folder", Name: "payments",
		Items: []items.Item{
			{Kind: "folder", Name: "other", Properties: []items.Property{{
				FolderCredentialsProperty: &items.FolderCredentialsProperty{FolderCredentials: userPassEntries("gitlab-lib")},
			}}},
			{Kind: "folder", Name: "apps", Properties: []items.Property{{
				FolderLibraries: &items.FolderLibrariesProperty{Libraries: []items.LibraryConfig{{
					LibraryConfiguration: gitLibraryFor("gitlab-lib"),
				}}},
			}}},
		},
	}}}
	err := checkFolderLibraryCredentials(sibling, nil)
	if err == nil || !strings.Contains(err.Error(), "gitlab-lib") {
		t.Fatalf("expected a refusal naming the credential, got %v", err)
	}
}

func TestCheckFolderLibraryCredentials_UnknownIDFails(t *testing.T) {
	m := libraryFolderManifest(nil, gitLibraryFor("missing-cred"))
	err := checkFolderLibraryCredentials(m, nil)
	if err == nil {
		t.Fatal("expected a refusal for an unknown credential")
	}
	for _, want := range []string{"missing-cred", `"payments/apps"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestCheckFolderLibraryCredentials_SecretTextKindFails(t *testing.T) {
	m := libraryFolderManifest(nil, gitLibraryFor("gitlab-lib"))
	err := checkFolderLibraryCredentials(m, secretTextEntries("gitlab-lib"))
	if err == nil || !strings.Contains(err.Error(), "is kind secretText") {
		t.Fatalf("expected a kind refusal, got %v", err)
	}
}

func TestCheckFolderLibraryCredentials_GitHubRejectsSSHKey(t *testing.T) {
	sshEntries := []items.FolderCredentialEntry{{Credentials: []items.FolderCredential{{
		BasicSSHUserPrivateKey: &items.BasicSSHUserPrivateKeyCredential{ID: "gh-key", Username: "git"},
	}}}}
	lib := items.LibraryConfiguration{
		Name: "gh-lib", DefaultVersion: "main",
		Retriever: items.LibraryRetriever{ModernSCM: &items.ModernSCMRetriever{
			SCM: items.LibrarySCM{GitHub: &items.GitHubSCMSource{RepoOwner: "o", Repository: "r", CredentialsID: "gh-key"}},
		}},
	}
	m := libraryFolderManifest(nil, lib)
	err := checkFolderLibraryCredentials(m, sshEntries)
	if err == nil || !strings.Contains(err.Error(), "this source needs usernamePassword") {
		t.Fatalf("expected a GitHub kind refusal, got %v", err)
	}
}

func TestCheckFolderLibraryCredentials_LegacyUserRemoteConfigChecked(t *testing.T) {
	lib := items.LibraryConfiguration{
		Name: "legacy-lib", DefaultVersion: "main",
		Retriever: items.LibraryRetriever{LegacySCM: &items.LegacySCMRetriever{SCM: items.SCM{GitSCM: &items.GitSCM{
			UserRemoteConfigs: []items.UserRemoteConfig{{UserRemoteConfig: items.RemoteConfig{
				URL: "https://git.example.com/l.git", CredentialsID: "legacy-cred",
			}}},
		}}}},
	}
	m := libraryFolderManifest(nil, lib)
	err := checkFolderLibraryCredentials(m, nil)
	if err == nil || !strings.Contains(err.Error(), "legacy-cred") {
		t.Fatalf("expected the legacy remote credential to be checked, got %v", err)
	}
	if err := checkFolderLibraryCredentials(m, userPassEntries("legacy-cred")); err != nil {
		t.Fatalf("expected a usernamePassword legacy credential to pass, got %v", err)
	}
}

func TestFolderCredentialIDKind(t *testing.T) {
	id, kind := folderCredentialIDKind(items.FolderCredential{SecretText: &items.SecretTextCredential{ID: "st"}})
	if id != "st" || kind != credentialKindSecretText {
		t.Fatalf("folderCredentialIDKind(secretText) = %q, %q", id, kind)
	}
}
