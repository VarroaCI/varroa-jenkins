package controller

import (
	"fmt"
	"slices"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
)

// checkFolderLibraryCredentials refuses a tenant manifest whose folder
// libraries name a credential that the library's folder cannot see, or a
// credential of a kind the library source does not accept. A folder sees
// the projected tenant credentials and the folder credentials of itself and
// of every ancestor. A sibling folder's credentials are not visible.
//
// Jenkins resolves a library credential by ID from the folder chain only.
// An unknown ID, or a wrong kind, otherwise fails later on the mite with no
// hold reason.
func checkFolderLibraryCredentials(manifest *items.Manifest, projected []items.FolderCredentialEntry) error {
	visible := make(map[string]string)
	addFolderCredentials(visible, projected)
	return checkLibraryCredentialsIn(manifest.Items, "", visible)
}

// checkLibraryCredentialsIn walks one level of the item tree. visible holds
// the credential IDs and kinds that every item of this level can see.
func checkLibraryCredentialsIn(list []items.Item, parent string, visible map[string]string) error {
	for _, it := range list {
		path := it.Name
		if parent != "" {
			path = parent + "/" + it.Name
		}
		scope := make(map[string]string, len(visible))
		for id, kind := range visible {
			scope[id] = kind
		}
		for _, p := range it.Properties {
			if p.FolderCredentialsProperty != nil {
				addFolderCredentials(scope, p.FolderCredentialsProperty.FolderCredentials)
			}
		}
		for _, p := range it.Properties {
			if p.FolderLibraries == nil {
				continue
			}
			for _, lc := range p.FolderLibraries.Libraries {
				if err := checkLibraryCredentialRefs(lc.LibraryConfiguration, path, scope); err != nil {
					return err
				}
			}
		}
		if err := checkLibraryCredentialsIn(it.Items, path, scope); err != nil {
			return err
		}
	}
	return nil
}

// checkLibraryCredentialRefs checks each credential that one library names
// against the credentials visible at its folder.
func checkLibraryCredentialRefs(c items.LibraryConfiguration, path string, scope map[string]string) error {
	for _, ref := range c.CredentialRefs() {
		kind, ok := scope[ref.ID]
		if !ok {
			return fmt.Errorf("folder library %q at %q names credential %q, which no tenant credential Secret or folder credential provides", c.Name, path, ref.ID)
		}
		if !slices.Contains(ref.Kinds, kind) {
			return fmt.Errorf("folder library %q at %q: credential %q is kind %s; this source needs %s",
				c.Name, path, ref.ID, kind, strings.Join(ref.Kinds, " or "))
		}
	}
	return nil
}

// addFolderCredentials records the ID and kind of each credential in entries.
func addFolderCredentials(visible map[string]string, entries []items.FolderCredentialEntry) {
	for _, e := range entries {
		for _, c := range e.Credentials {
			if id, kind := folderCredentialIDKind(c); id != "" {
				visible[id] = kind
			}
		}
	}
}

// folderCredentialIDKind returns the ID and the credential kind of one
// folder credential. The kinds match the credentialKind constants. An
// entry with no recognised type returns an empty ID.
func folderCredentialIDKind(c items.FolderCredential) (id, kind string) {
	switch {
	case c.UsernamePassword != nil:
		return c.UsernamePassword.ID, credentialKindUsernamePassword
	case c.SecretText != nil:
		return c.SecretText.ID, credentialKindSecretText
	case c.BasicSSHUserPrivateKey != nil:
		return c.BasicSSHUserPrivateKey.ID, credentialKindSSHPrivateKey
	}
	return "", ""
}
