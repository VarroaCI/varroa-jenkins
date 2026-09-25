package controller

import (
	"context"
	"fmt"
	"sort"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
)

// Tenant-credential Secret label, annotations, and the three credential kinds.
const (
	tenantCredentialLabelSelector = "varroa.dev/tenant-credential=true"

	annotationCredentialID          = "varroa.dev/credential-id"
	annotationCredentialKind        = "varroa.dev/credential-kind"
	annotationCredentialDescription = "varroa.dev/credential-description"

	credentialKindUsernamePassword = "usernamePassword"
	credentialKindSecretText       = "secretText"
	credentialKindSSHPrivateKey    = "sshPrivateKey"
)

// projectTenantCredentials lists agentNamespace's tenant-credential Secrets
// and appends each as a folder-credentials entry on folder, with scope
// GLOBAL (design D8). folder must be the tenant's re-rooted folder item. A
// malformed, unknown-kind, or duplicate-id Secret is skipped, and reported
// through the returned CredentialSkipped warnings, sorted by Secret name for
// a deterministic order.
func (r *Reconciler) projectTenantCredentials(ctx context.Context, tenant, agentNamespace string, folder *items.Item) []v1alpha1.TenantWarning {
	records, err := r.client.ListSecrets(ctx, agentNamespace, tenantCredentialLabelSelector)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("cell content: list tenant credential secrets failed",
				"tenant", tenant, "namespace", agentNamespace, "error", err)
		}
		return nil
	}
	names := make([]string, 0, len(records))
	byName := make(map[string]map[string][]byte, len(records))
	for _, rec := range records {
		name := string(rec["_name"])
		if name == "" {
			continue
		}
		names = append(names, name)
		byName[name] = rec
	}
	sort.Strings(names)

	var creds []items.FolderCredential
	var warnings []v1alpha1.TenantWarning
	seenIDs := make(map[string]bool, len(names))
	for _, name := range names {
		cred, warning := r.buildTenantCredential(ctx, tenant, agentNamespace, name, byName[name], seenIDs)
		if warning != nil {
			warnings = append(warnings, *warning)
			continue
		}
		if cred != nil {
			creds = append(creds, *cred)
		}
	}
	if len(creds) == 0 {
		return warnings
	}

	prop := folderCredentialsPropertyOf(folder)
	prop.FolderCredentials = append(prop.FolderCredentials, items.FolderCredentialEntry{Credentials: creds})
	return warnings
}

// buildTenantCredential validates one labelled Secret and returns either the
// FolderCredential it projects, or the CredentialSkipped warning explaining
// why it was skipped. seenIDs is mutated on acceptance, so a later duplicate
// id in the same call is skipped.
func (r *Reconciler) buildTenantCredential(ctx context.Context, tenant, agentNamespace, name string, data map[string][]byte, seenIDs map[string]bool) (*items.FolderCredential, *v1alpha1.TenantWarning) {
	skip := func(reason string) *v1alpha1.TenantWarning {
		return &v1alpha1.TenantWarning{
			Tenant:  tenant,
			Reason:  v1alpha1.TeamReasonCredentialSkipped,
			Message: fmt.Sprintf("secret %q: %s", name, reason),
		}
	}

	ann, err := r.client.GetSecretAnnotations(ctx, name, agentNamespace)
	if err != nil {
		return nil, skip(fmt.Sprintf("failed to read annotations: %v", err))
	}
	id := ann[annotationCredentialID]
	kind := ann[annotationCredentialKind]
	if id == "" || kind == "" {
		return nil, skip("missing the credential-id or credential-kind annotation")
	}

	var cred items.FolderCredential
	switch kind {
	case credentialKindUsernamePassword:
		username, password := data["username"], data["password"]
		if len(username) == 0 || len(password) == 0 {
			return nil, skip("usernamePassword secret missing the username or password data key")
		}
		cred.UsernamePassword = &items.UsernamePasswordCredential{
			ID:          id,
			Scope:       "GLOBAL",
			Description: ann[annotationCredentialDescription],
			Username:    string(username),
			Password:    string(password),
		}
	case credentialKindSecretText:
		secret := data["secret"]
		if len(secret) == 0 {
			return nil, skip("secretText secret missing the secret data key")
		}
		cred.SecretText = &items.SecretTextCredential{
			ID:          id,
			Scope:       "GLOBAL",
			Description: ann[annotationCredentialDescription],
			Secret:      string(secret),
		}
	case credentialKindSSHPrivateKey:
		privateKey, username := data["privateKey"], data["username"]
		if len(privateKey) == 0 {
			return nil, skip("sshPrivateKey secret missing the privateKey data key")
		}
		if len(username) == 0 {
			return nil, skip("sshPrivateKey secret missing the username data key")
		}
		// An empty or missing passphrase means a key with no passphrase.
		cred.BasicSSHUserPrivateKey = &items.BasicSSHUserPrivateKeyCredential{
			ID:          id,
			Scope:       "GLOBAL",
			Description: ann[annotationCredentialDescription],
			Username:    string(username),
			Passphrase:  string(data["passphrase"]),
			PrivateKeySource: items.PrivateKeySource{
				DirectEntry: &items.DirectEntryPrivateKeySource{PrivateKey: string(privateKey)},
			},
		}
	default:
		return nil, skip(fmt.Sprintf("unknown credential-kind %q", kind))
	}

	if seenIDs[id] {
		return nil, skip(fmt.Sprintf("duplicate credential id %q", id))
	}
	seenIDs[id] = true
	return &cred, nil
}

// folderCredentialsPropertyOf returns folder's FolderCredentialsProperty,
// creating and attaching an empty one first when folder has none.
func folderCredentialsPropertyOf(folder *items.Item) *items.FolderCredentialsProperty {
	for i := range folder.Properties {
		if folder.Properties[i].FolderCredentialsProperty != nil {
			return folder.Properties[i].FolderCredentialsProperty
		}
	}
	prop := &items.FolderCredentialsProperty{}
	folder.Properties = append(folder.Properties, items.Property{FolderCredentialsProperty: prop})
	return prop
}
