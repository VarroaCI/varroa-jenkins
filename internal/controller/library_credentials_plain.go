package controller

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
)

// checkPlainFolderLibraryCredentials refuses a non-cell controller whose
// folder libraries name a credential that the library's folder cannot see.
// A folder sees the GLOBAL-scope credentials of the system store in
// jenkins.yaml and the folder credentials of itself and of every ancestor.
// Jenkins hides SYSTEM-scope credentials from items.
//
// The check reads jenkins.yaml after ResolveVars, so an ID that a variable
// supplies is already resolved. A system credential ID that still holds a
// variable token is left out of the visible and SYSTEM-scope sets. A library
// reference to an ID that is neither visible nor SYSTEM-scope is then not
// refused as missing, because it may be the unresolved ID. A SYSTEM-scope
// refusal and a kind refusal still apply to every resolved ID.
//
// A parse failure returns no error. The caller reports it on its own path.
func checkPlainFolderLibraryCredentials(itemsYAML, jenkinsYAML string) error {
	manifest, err := items.Parse(itemsYAML)
	if err != nil {
		return nil //nolint:nilerr // a parse failure is reported through the caller's own error path, not this check
	}
	visible, systemOnly, unresolved, err := systemStoreCredentials(jenkinsYAML)
	if err != nil {
		return nil //nolint:nilerr // a parse failure is reported through the caller's own error path, not this check
	}
	return checkLibraryCredentials(manifest, visible, libraryCredentialOrigin{
		missing:       "which no GLOBAL-scope credential in jenkins.yaml or folder credential provides",
		systemOnly:    systemOnly,
		unresolvedIDs: unresolved,
	})
}

// systemStoreCredentials reads the system credential store of jenkins.yaml.
// It returns the GLOBAL-scope credentials with their kinds. It returns the
// SYSTEM-scope IDs that no GLOBAL-scope entry also names. An ID that holds a
// variable token enters neither set, and unresolved reports it. A missing
// scope counts as GLOBAL. Every domain counts.
func systemStoreCredentials(jenkinsYAML string) (visible map[string]string, systemOnly map[string]bool, unresolved bool, err error) {
	visible = make(map[string]string)
	systemOnly = make(map[string]bool)
	var doc struct {
		Credentials struct {
			System struct {
				Domains []struct {
					Credentials []map[string]map[string]any `yaml:"credentials"`
				} `yaml:"domainCredentials"`
			} `yaml:"system"`
		} `yaml:"credentials"`
	}
	if err := yaml.Unmarshal([]byte(jenkinsYAML), &doc); err != nil {
		return nil, nil, false, fmt.Errorf("parse jenkins.yaml credentials: %w", err)
	}
	for _, domain := range doc.Credentials.System.Domains {
		for _, entry := range domain.Credentials {
			for typeKey, body := range entry {
				id, _ := body["id"].(string)
				if id == "" {
					continue
				}
				if strings.Contains(id, unresolvedVarToken) {
					unresolved = true
					continue
				}
				if scope, _ := body["scope"].(string); strings.EqualFold(scope, "SYSTEM") {
					systemOnly[id] = true
					continue
				}
				visible[id] = systemCredentialKind(typeKey)
			}
		}
	}
	for id := range visible {
		delete(systemOnly, id)
	}
	return visible, systemOnly, unresolved, nil
}

// systemCredentialKind maps a JCasC credential type key to a credentialKind
// constant. A type key the walker does not recognise maps to credentialKindAny.
func systemCredentialKind(typeKey string) string {
	switch typeKey {
	case "usernamePassword":
		return credentialKindUsernamePassword
	case "string":
		return credentialKindSecretText
	case "basicSSHUserPrivateKey":
		return credentialKindSSHPrivateKey
	case "file":
		return credentialKindSecretFile
	case "certificate":
		return credentialKindCertificate
	case "gitHubApp":
		return credentialKindGitHubApp
	}
	return credentialKindAny
}
