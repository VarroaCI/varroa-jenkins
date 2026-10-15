package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/controller/rehydrate"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
)

// Tenant-credential Secret label, annotations, and the six credential kinds.
const (
	tenantCredentialLabelSelector = "varroa.dev/tenant-credential=true"

	annotationCredentialID          = "varroa.dev/credential-id"
	annotationCredentialKind        = "varroa.dev/credential-kind"
	annotationCredentialDescription = "varroa.dev/credential-description"
	annotationCredentialFilename    = "varroa.dev/credential-filename"

	credentialKindUsernamePassword = "usernamePassword"
	credentialKindSecretText       = "secretText"
	credentialKindSSHPrivateKey    = "sshPrivateKey"
	credentialKindSecretFile       = "secretFile"
	credentialKindCertificate      = "certificate"
	credentialKindGitHubApp        = "githubApp"

	// maxBinaryCredentialBytes caps one secretFile, certificate, or githubApp
	// privateKey value, measured as raw bytes. The cell budget
	// (cellCredentialBudgetBytes) bounds the sum over one cell.
	maxBinaryCredentialBytes = 64 * 1024

	// desiredStateMaxBytes is the NATS max_payload. The cell desired state
	// travels as one core NATS request, and the chart does not raise the
	// server default.
	desiredStateMaxBytes = 1024 * 1024
	// cellDesiredStateReserveBytes is the part of one request kept for the
	// JCasC, RBAC, and items documents and the small fields.
	cellDesiredStateReserveBytes = 256 * 1024
	// cellCredentialBudgetBytes bounds the projected tenant credentials of
	// one cell, measured as they travel in the request.
	cellCredentialBudgetBytes = desiredStateMaxBytes - rehydrate.BundleBudgetBytes - cellDesiredStateReserveBytes
)

// cellCredentialBudget tracks the wire bytes of the projected tenant
// credentials of one cell. A credential that does not fit is skipped, and
// the scan continues, so a later smaller credential can still fit.
type cellCredentialBudget struct {
	limit, used int
}

func newCellCredentialBudget(limit int) *cellCredentialBudget {
	return &cellCredentialBudget{limit: limit}
}

// credentialIndentBytes is the allowance for each YAML line of a credential
// in the final items document. The credential sits several levels deep
// there, so every line gains indentation that this form does not carry.
const credentialIndentBytes = 16

// credentialWireBytes returns the length of one credential as it travels:
// its YAML form, JSON-encoded as a string inside the command, plus an
// indentation allowance per line. Base64 encoding of binary values and
// JSON escapes are both counted.
func credentialWireBytes(c items.FolderCredential) (int, error) {
	out, err := yaml.Marshal(c)
	if err != nil {
		return 0, err
	}
	wire, err := json.Marshal(string(out))
	if err != nil {
		return 0, err
	}
	return len(wire) + credentialIndentBytes*bytes.Count(out, []byte{'\n'}), nil
}

// trim keeps the credentials of prop that fit in the remaining budget, in
// their projected order, and charges their bytes. It skips the rest and
// returns one CredentialSkipped warning for each skipped credential. An
// entry left empty is removed, and prop.FolderCredentials is nil when no
// entry remains. It returns the bytes it charged.
func (b *cellCredentialBudget) trim(tenant string, prop *items.FolderCredentialsProperty) (charged int, warnings []v1alpha1.TenantWarning) {
	if prop == nil {
		return 0, nil
	}
	var entries []items.FolderCredentialEntry
	for _, entry := range prop.FolderCredentials {
		var kept []items.FolderCredential
		for _, c := range entry.Credentials {
			id, _ := folderCredentialIDKind(c)
			size, err := credentialWireBytes(c)
			switch {
			case err != nil:
				warnings = append(warnings, v1alpha1.TenantWarning{
					Tenant:  tenant,
					Reason:  v1alpha1.TeamReasonCredentialSkipped,
					Message: fmt.Sprintf("credential %q skipped: it cannot be measured: %v", id, err),
				})
			case b.used+size > b.limit:
				warnings = append(warnings, v1alpha1.TenantWarning{
					Tenant:  tenant,
					Reason:  v1alpha1.TeamReasonCredentialSkipped,
					Message: fmt.Sprintf("credential %q skipped: its %d bytes do not fit in the %d-byte credential budget of the cell", id, size, b.limit),
				})
			default:
				b.used += size
				charged += size
				kept = append(kept, c)
			}
		}
		if len(kept) > 0 {
			entry.Credentials = kept
			entries = append(entries, entry)
		}
	}
	prop.FolderCredentials = entries
	return charged, warnings
}

// refund returns n bytes to the budget. The caller uses it when a tenant
// is held and its credentials are not carried.
func (b *cellCredentialBudget) refund(n int) {
	b.used -= n
}

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
	case credentialKindSecretFile:
		content := data["file"]
		if len(content) == 0 {
			return nil, skip("secretFile secret missing the file data key")
		}
		if len(content) > maxBinaryCredentialBytes {
			return nil, skip(fmt.Sprintf("secretFile secret file data key exceeds %d bytes", maxBinaryCredentialBytes))
		}
		fileName := ann[annotationCredentialFilename]
		if fileName == "" {
			fileName = name
		}
		if !validCredentialFilename(fileName) {
			return nil, skip(fmt.Sprintf("secretFile secret has an invalid credential-filename %q", fileName))
		}
		cred.File = &items.FileCredential{
			ID:          id,
			Scope:       "GLOBAL",
			Description: ann[annotationCredentialDescription],
			FileName:    fileName,
			SecretBytes: base64.StdEncoding.EncodeToString(content),
		}
	case credentialKindCertificate:
		keystore := data["keystore"]
		if len(keystore) == 0 {
			return nil, skip("certificate secret missing the keystore data key")
		}
		if len(keystore) > maxBinaryCredentialBytes {
			return nil, skip(fmt.Sprintf("certificate secret keystore data key exceeds %d bytes", maxBinaryCredentialBytes))
		}
		// A PKCS12 keystore is a DER SEQUENCE, so its first byte is 0x30.
		if keystore[0] != 0x30 {
			return nil, skip("certificate secret keystore data key is not a binary PKCS#12 keystore")
		}
		// An empty or missing password means a keystore with no password.
		cred.Certificate = &items.CertificateCredential{
			ID:          id,
			Scope:       "GLOBAL",
			Description: ann[annotationCredentialDescription],
			Password:    string(data["password"]),
			KeyStoreSource: items.KeyStoreSource{
				Uploaded: &items.UploadedKeyStoreSource{
					UploadedKeystore: base64.StdEncoding.EncodeToString(keystore),
				},
			},
		}
	case credentialKindGitHubApp:
		appID, privateKey := strings.TrimSpace(string(data["appID"])), data["privateKey"]
		if appID == "" || !isDecimal(appID) {
			return nil, skip("githubApp secret missing a numeric appID data key")
		}
		if len(privateKey) == 0 {
			return nil, skip("githubApp secret missing the privateKey data key")
		}
		if len(privateKey) > maxBinaryCredentialBytes {
			return nil, skip(fmt.Sprintf("githubApp secret privateKey data key exceeds %d bytes", maxBinaryCredentialBytes))
		}
		if !strings.HasPrefix(strings.TrimSpace(string(privateKey)), "-----BEGIN PRIVATE KEY-----") {
			return nil, skip("githubApp secret privateKey is not a PKCS#8 PEM key")
		}
		cred.GitHubApp = &items.GitHubAppCredential{
			ID:          id,
			Scope:       "GLOBAL",
			Description: ann[annotationCredentialDescription],
			AppID:       appID,
			PrivateKey:  string(privateKey),
			Owner:       strings.TrimSpace(string(data["owner"])),
			APIURI:      strings.TrimSpace(string(data["apiUri"])),
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

// isDecimal reports whether s is a non-empty run of ASCII digits.
func isDecimal(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// validCredentialFilename reports whether s is one plain file name, so the
// file binding cannot write outside its temporary directory.
func validCredentialFilename(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
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

// injectTenantFolderCredentials adds each tenant's projected credentials to
// the top-level folder of the same name in itemsYAML. The caller runs it
// after ResolveVars and the unresolved-variable check, so a value reaches
// Jenkins exactly as the Secret holds it. A folder that is not in itemsYAML
// is an error, because assembleCellContent carries credentials only for a
// folder it also merged.
func injectTenantFolderCredentials(itemsYAML string, folders []bundle.CellFolderCredentials) (string, error) {
	if len(folders) == 0 {
		return itemsYAML, nil
	}
	manifest, err := items.Parse(itemsYAML)
	if err != nil {
		return "", err
	}
	for _, f := range folders {
		var folder *items.Item
		for i := range manifest.Items {
			if manifest.Items[i].Name == f.Folder && manifest.Items[i].IsFolder() {
				folder = &manifest.Items[i]
				break
			}
		}
		if folder == nil {
			return "", fmt.Errorf("tenant folder %q is not in the cell items", f.Folder)
		}
		prop := folderCredentialsPropertyOf(folder)
		prop.FolderCredentials = append(prop.FolderCredentials, f.Entries...)
	}
	out, err := yaml.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("marshal cell items: %w", err)
	}
	return string(out), nil
}
