package controller

import (
	"context"
	"testing"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
)

// tenantCredentialTestNamespace is the fixed agentNamespace every test in
// this file projects tenant credentials from.
const tenantCredentialTestNamespace = "payments-ns"

// seedTenantCredentialSecret registers a fixture Secret for ListSecrets,
// GetSecretAnnotations, and GetSecret, matching the shape a real
// varroa.dev/tenant-credential Secret carries.
func seedTenantCredentialSecret(t *testing.T, client *testClient, name string, annotations map[string]string, data map[string][]byte) {
	t.Helper()
	client.labeledSecrets = append(client.labeledSecrets, labeledSecretFixture{
		name: name, namespace: tenantCredentialTestNamespace,
		labels: map[string]string{"varroa.dev/tenant-credential": "true"},
	})
	if client.secretAnnotations == nil {
		client.secretAnnotations = map[string]map[string]string{}
	}
	client.secretAnnotations[name] = annotations
	client.existingSecrets[name] = data
}

func newFolderItem(t *testing.T) *items.Item {
	t.Helper()
	return &items.Item{Kind: "folder", Name: "payments"}
}

// TestProjectTenantCredentials_HappyPath is task 8.11's happy-path
// acceptance: a well-formed usernamePassword and a secretText Secret both
// project as folder-credentials entries with scope GLOBAL.
func TestProjectTenantCredentials_HappyPath(t *testing.T) {
	client := newTestClient()
	seedTenantCredentialSecret(t, client, "cred-up",
		map[string]string{
			"varroa.dev/credential-id":          "up-cred",
			"varroa.dev/credential-kind":        "usernamePassword",
			"varroa.dev/credential-description": "a username/password credential",
		},
		map[string][]byte{"username": []byte("bob"), "password": []byte("hunter2")},
	)
	seedTenantCredentialSecret(t, client, "cred-st",
		map[string]string{
			"varroa.dev/credential-id":   "st-cred",
			"varroa.dev/credential-kind": "secretText",
		},
		map[string][]byte{"secret": []byte("shh")},
	)
	rec := newTestReconciler(client)
	folder := newFolderItem(t)

	warnings := rec.projectTenantCredentials(context.Background(), "payments", "payments-ns", folder)
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %+v", warnings)
	}

	var prop *items.FolderCredentialsProperty
	for _, p := range folder.Properties {
		if p.FolderCredentialsProperty != nil {
			prop = p.FolderCredentialsProperty
		}
	}
	if prop == nil || len(prop.FolderCredentials) != 1 || len(prop.FolderCredentials[0].Credentials) != 2 {
		t.Fatalf("expected one folder-credentials entry with two credentials, got %+v", prop)
	}
	var sawUP, sawST bool
	for _, c := range prop.FolderCredentials[0].Credentials {
		if c.UsernamePassword != nil {
			sawUP = true
			if c.UsernamePassword.ID != "up-cred" || c.UsernamePassword.Scope != "GLOBAL" ||
				c.UsernamePassword.Username != "bob" || c.UsernamePassword.Password != "hunter2" {
				t.Errorf("unexpected usernamePassword credential: %+v", c.UsernamePassword)
			}
		}
		if c.SecretText != nil {
			sawST = true
			if c.SecretText.ID != "st-cred" || c.SecretText.Scope != "GLOBAL" || c.SecretText.Secret != "shh" {
				t.Errorf("unexpected secretText credential: %+v", c.SecretText)
			}
		}
	}
	if !sawUP || !sawST {
		t.Errorf("expected both credential kinds projected, got %+v", prop.FolderCredentials[0].Credentials)
	}
}

// TestProjectTenantCredentials_MalformedSecretSkipped is task 8.11's
// malformed-secret acceptance: a secretText Secret with no secret data key
// is skipped and warned, not fatal.
func TestProjectTenantCredentials_MalformedSecretSkipped(t *testing.T) {
	client := newTestClient()
	seedTenantCredentialSecret(t, client, "cred-bad",
		map[string]string{
			"varroa.dev/credential-id":   "bad-cred",
			"varroa.dev/credential-kind": "secretText",
		},
		map[string][]byte{}, // no "secret" key
	)
	rec := newTestReconciler(client)
	folder := newFolderItem(t)

	warnings := rec.projectTenantCredentials(context.Background(), "payments", "payments-ns", folder)
	if len(warnings) != 1 || warnings[0].Reason != v1alpha1.TeamReasonCredentialSkipped || warnings[0].Tenant != "payments" {
		t.Fatalf("expected one CredentialSkipped warning, got %+v", warnings)
	}
	for _, p := range folder.Properties {
		if p.FolderCredentialsProperty != nil {
			t.Errorf("expected no folder-credentials property for an all-skipped tenant, got %+v", p.FolderCredentialsProperty)
		}
	}
}

// TestProjectTenantCredentials_UnknownKindSkipped is task 8.11's
// unknown-kind acceptance.
func TestProjectTenantCredentials_UnknownKindSkipped(t *testing.T) {
	client := newTestClient()
	seedTenantCredentialSecret(t, client, "cred-unknown",
		map[string]string{
			"varroa.dev/credential-id":   "unknown-cred",
			"varroa.dev/credential-kind": "sshKey",
		},
		map[string][]byte{"key": []byte("...")},
	)
	rec := newTestReconciler(client)
	folder := newFolderItem(t)

	warnings := rec.projectTenantCredentials(context.Background(), "payments", "payments-ns", folder)
	if len(warnings) != 1 || warnings[0].Reason != v1alpha1.TeamReasonCredentialSkipped {
		t.Fatalf("expected one CredentialSkipped warning, got %+v", warnings)
	}
}

// TestProjectTenantCredentials_DuplicateIDSkipped is task 8.11's
// duplicate-id acceptance: one of the two is projected, the other skipped
// and warned, naming the duplicated id.
func TestProjectTenantCredentials_DuplicateIDSkipped(t *testing.T) {
	client := newTestClient()
	seedTenantCredentialSecret(t, client, "cred-a",
		map[string]string{"varroa.dev/credential-id": "dup", "varroa.dev/credential-kind": "secretText"},
		map[string][]byte{"secret": []byte("one")},
	)
	seedTenantCredentialSecret(t, client, "cred-b",
		map[string]string{"varroa.dev/credential-id": "dup", "varroa.dev/credential-kind": "secretText"},
		map[string][]byte{"secret": []byte("two")},
	)
	rec := newTestReconciler(client)
	folder := newFolderItem(t)

	warnings := rec.projectTenantCredentials(context.Background(), "payments", "payments-ns", folder)
	if len(warnings) != 1 || warnings[0].Reason != v1alpha1.TeamReasonCredentialSkipped {
		t.Fatalf("expected one CredentialSkipped warning, got %+v", warnings)
	}
	if !containsSubstr(warnings[0].Message, "dup") {
		t.Errorf("expected the warning to name the duplicated id, got %q", warnings[0].Message)
	}

	var prop *items.FolderCredentialsProperty
	for _, p := range folder.Properties {
		if p.FolderCredentialsProperty != nil {
			prop = p.FolderCredentialsProperty
		}
	}
	if prop == nil || len(prop.FolderCredentials[0].Credentials) != 1 {
		t.Fatalf("expected exactly one projected credential (the other is the duplicate), got %+v", prop)
	}
}

const testSSHPrivateKey = "-----BEGIN FIXTURE KEY-----\nZmFrZQ==\n-----END FIXTURE KEY-----\n"

// projectedCredentials returns the credentials of folder's single
// folder-credentials entry, or nil when folder has none.
func projectedCredentials(t *testing.T, folder *items.Item) []items.FolderCredential {
	t.Helper()
	for _, p := range folder.Properties {
		if p.FolderCredentialsProperty != nil {
			if len(p.FolderCredentialsProperty.FolderCredentials) != 1 {
				t.Fatalf("expected one folder-credentials entry, got %+v", p.FolderCredentialsProperty)
			}
			return p.FolderCredentialsProperty.FolderCredentials[0].Credentials
		}
	}
	return nil
}

func sshCredentialAnnotations(id string) map[string]string {
	return map[string]string{
		"varroa.dev/credential-id":          id,
		"varroa.dev/credential-kind":        "sshPrivateKey",
		"varroa.dev/credential-description": "GitHub deploy key",
	}
}

func TestProjectTenantCredentials_SSHPrivateKey(t *testing.T) {
	client := newTestClient()
	seedTenantCredentialSecret(t, client, "deploy-key", sshCredentialAnnotations("varroa-ci-deploy-key"),
		map[string][]byte{
			"privateKey": []byte(testSSHPrivateKey),
			"username":   []byte("git"),
			"passphrase": []byte("s3cret"),
		},
	)
	rec := newTestReconciler(client)
	folder := newFolderItem(t)

	warnings := rec.projectTenantCredentials(context.Background(), "payments", tenantCredentialTestNamespace, folder)
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %+v", warnings)
	}
	creds := projectedCredentials(t, folder)
	if len(creds) != 1 || creds[0].BasicSSHUserPrivateKey == nil {
		t.Fatalf("expected one basicSSHUserPrivateKey credential, got %+v", creds)
	}
	got := creds[0].BasicSSHUserPrivateKey
	if got.ID != "varroa-ci-deploy-key" || got.Scope != "GLOBAL" || got.Description != "GitHub deploy key" ||
		got.Username != "git" || got.Passphrase != "s3cret" {
		t.Errorf("unexpected credential: %+v", got)
	}
	if got.PrivateKeySource.DirectEntry == nil || got.PrivateKeySource.DirectEntry.PrivateKey != testSSHPrivateKey {
		t.Errorf("unexpected private key source: %+v", got.PrivateKeySource)
	}
}

func TestProjectTenantCredentials_SSHPrivateKeyNoPassphrase(t *testing.T) {
	cases := map[string]map[string][]byte{
		"no passphrase key": {
			"privateKey": []byte(testSSHPrivateKey),
			"username":   []byte("git"),
		},
		"empty passphrase key": {
			"privateKey": []byte(testSSHPrivateKey),
			"username":   []byte("git"),
			"passphrase": []byte(""),
		},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			client := newTestClient()
			seedTenantCredentialSecret(t, client, "deploy-key", sshCredentialAnnotations("deploy"), data)
			rec := newTestReconciler(client)
			folder := newFolderItem(t)

			warnings := rec.projectTenantCredentials(context.Background(), "payments", tenantCredentialTestNamespace, folder)
			if len(warnings) != 0 {
				t.Fatalf("expected no warnings, got %+v", warnings)
			}
			creds := projectedCredentials(t, folder)
			if len(creds) != 1 || creds[0].BasicSSHUserPrivateKey == nil {
				t.Fatalf("expected one basicSSHUserPrivateKey credential, got %+v", creds)
			}
			if creds[0].BasicSSHUserPrivateKey.Passphrase != "" {
				t.Errorf("expected an empty passphrase, got %q", creds[0].BasicSSHUserPrivateKey.Passphrase)
			}
		})
	}
}

func TestProjectTenantCredentials_SSHPrivateKeyMissingPrivateKey(t *testing.T) {
	cases := map[string]map[string][]byte{
		"missing":             {"username": []byte("git")},
		"empty":               {"privateKey": []byte(""), "username": []byte("git")},
		"missing both fields": {},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			client := newTestClient()
			seedTenantCredentialSecret(t, client, "deploy-key", sshCredentialAnnotations("deploy"), data)
			rec := newTestReconciler(client)
			folder := newFolderItem(t)

			warnings := rec.projectTenantCredentials(context.Background(), "payments", tenantCredentialTestNamespace, folder)
			want := `secret "deploy-key": sshPrivateKey secret missing the privateKey data key`
			if len(warnings) != 1 || warnings[0].Reason != v1alpha1.TeamReasonCredentialSkipped || warnings[0].Message != want {
				t.Fatalf("expected one CredentialSkipped warning %q, got %+v", want, warnings)
			}
			if creds := projectedCredentials(t, folder); len(creds) != 0 {
				t.Errorf("expected no projected credential, got %+v", creds)
			}
		})
	}
}

func TestProjectTenantCredentials_SSHPrivateKeyMissingUsername(t *testing.T) {
	cases := map[string]map[string][]byte{
		"missing": {"privateKey": []byte(testSSHPrivateKey)},
		"empty":   {"privateKey": []byte(testSSHPrivateKey), "username": []byte("")},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			client := newTestClient()
			seedTenantCredentialSecret(t, client, "deploy-key", sshCredentialAnnotations("deploy"), data)
			rec := newTestReconciler(client)
			folder := newFolderItem(t)

			warnings := rec.projectTenantCredentials(context.Background(), "payments", tenantCredentialTestNamespace, folder)
			want := `secret "deploy-key": sshPrivateKey secret missing the username data key`
			if len(warnings) != 1 || warnings[0].Reason != v1alpha1.TeamReasonCredentialSkipped || warnings[0].Message != want {
				t.Fatalf("expected one CredentialSkipped warning %q, got %+v", want, warnings)
			}
			if creds := projectedCredentials(t, folder); len(creds) != 0 {
				t.Errorf("expected no projected credential, got %+v", creds)
			}
		})
	}
}

func TestProjectTenantCredentials_SSHPrivateKeyDuplicateID(t *testing.T) {
	client := newTestClient()
	seedTenantCredentialSecret(t, client, "cred-a",
		map[string]string{"varroa.dev/credential-id": "dup", "varroa.dev/credential-kind": "secretText"},
		map[string][]byte{"secret": []byte("one")},
	)
	seedTenantCredentialSecret(t, client, "cred-b", sshCredentialAnnotations("dup"),
		map[string][]byte{"privateKey": []byte(testSSHPrivateKey), "username": []byte("git")},
	)
	rec := newTestReconciler(client)
	folder := newFolderItem(t)

	warnings := rec.projectTenantCredentials(context.Background(), "payments", tenantCredentialTestNamespace, folder)
	want := `secret "cred-b": duplicate credential id "dup"`
	if len(warnings) != 1 || warnings[0].Reason != v1alpha1.TeamReasonCredentialSkipped || warnings[0].Message != want {
		t.Fatalf("expected one CredentialSkipped warning %q, got %+v", want, warnings)
	}
	creds := projectedCredentials(t, folder)
	if len(creds) != 1 || creds[0].SecretText == nil || creds[0].BasicSSHUserPrivateKey != nil {
		t.Fatalf("expected only the earlier secretText credential, got %+v", creds)
	}
}

func containsSubstr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestApplyCellContent_TenantCredentialsNeverReachAConfigMap is task 8.11's
// no-ConfigMap-leak acceptance: assembling a cell's desired state with
// tenant credentials writes no credential value into any ConfigMap the
// operator creates or updates.
func TestApplyCellContent_TenantCredentialsNeverReachAConfigMap(t *testing.T) {
	client := newTestClient()
	crdstore.MustSeed(client.store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"payments-ns"},
			Pipelines: &v1alpha1.TeamPipelinesSpec{
				Profile:        "jenkins-lts",
				AgentNamespace: "payments-ns",
			},
		},
	})
	const secretValue = "super-secret-value"
	seedTenantCredentialSecret(t, client, "cred-st",
		map[string]string{"varroa.dev/credential-id": "st-cred", "varroa.dev/credential-kind": "secretText"},
		map[string][]byte{"secret": []byte(secretValue)},
	)
	const keyBody = "c3NoLWtleS1ib2R5LWZpeHR1cmU"
	const passphrase = "ssh-key-passphrase-value"
	seedTenantCredentialSecret(t, client, "cred-ssh", sshCredentialAnnotations("ssh-cred"),
		map[string][]byte{
			"privateKey": []byte("-----BEGIN FIXTURE KEY-----\n" + keyBody + "\n-----END FIXTURE KEY-----\n"),
			"username":   []byte("git"),
			"passphrase": []byte(passphrase),
		},
	)

	rec := newTestReconciler(client)
	cr := cellControllerFixture()
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"payments"}}
	mat := &bundle.MaterializedBundle{}

	if err := rec.applyCellContent(context.Background(), cr, mat, "http://cell1-svc.ns1.svc.cluster.local:8080"); err != nil {
		t.Fatalf("applyCellContent: %v", err)
	}
	carried, err := yaml.Marshal(mat.CellFolderCredentials)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{secretValue, keyBody, passphrase} {
		if containsSubstr(mat.ItemsYAML, value) {
			t.Fatalf("value %q must not be in items.yaml before ResolveVars, got:\n%s", value, mat.ItemsYAML)
		}
		if !containsSubstr(string(carried), value) {
			t.Fatalf("expected %q in the carried folder credentials, got:\n%s", value, carried)
		}
	}
	for name, data := range client.configMapData {
		for key, val := range data {
			for _, value := range []string{secretValue, keyBody, passphrase} {
				if containsSubstr(val, value) {
					t.Errorf("credential value %q leaked into ConfigMap %q key %q", value, name, key)
				}
			}
		}
	}
}

// A tenant credential value is Secret data. A ${...} text in it must reach
// Jenkins as written, and must not stop the push of any tenant on the cell.
func TestResolveBundleForController_CellCredentialWithInterpolationTextKeepsTheCell(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	payments := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{tenantCredentialTestNamespace},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{Profile: "lts", AgentNamespace: tenantCredentialTestNamespace},
		},
	}
	seedTenantBundle(client, payments, "payments-bundle", cleanPipelineItemsYAML)
	seedTenantCredentialSecret(t, client, "cred-literal",
		map[string]string{"varroa.dev/credential-id": "literal", "varroa.dev/credential-kind": "secretText"},
		map[string][]byte{"secret": []byte("abc${def}")},
	)
	seedTenantCredentialSecret(t, client, "cred-varname",
		map[string]string{
			"varroa.dev/credential-id":          "varname",
			"varroa.dev/credential-kind":        "usernamePassword",
			"varroa.dev/credential-description": "uses ${varroa_controller_name}",
		},
		map[string][]byte{"username": []byte("u${x}"), "password": []byte("${varroa_controller_name}")},
	)

	other := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-b"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"team-b-ns"},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{Profile: "lts", AgentNamespace: "team-b-ns"},
		},
	}
	seedTenantBundle(client, other, "team-b-bundle", cleanPipelineItemsYAML)
	client.labeledSecrets = append(client.labeledSecrets, labeledSecretFixture{
		name: "cred-b", namespace: "team-b-ns",
		labels: map[string]string{"varroa.dev/tenant-credential": "true"},
	})
	client.secretAnnotations["cred-b"] = map[string]string{"varroa.dev/credential-id": "b-token", "varroa.dev/credential-kind": "secretText"}
	client.existingSecrets["cred-b"] = map[string][]byte{"secret": []byte("team-b-value")}

	crdstore.MustSeed(client.store, payments)
	crdstore.MustSeed(client.store, other)
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"payments", "team-b"}}

	mat, _, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err != nil {
		t.Fatalf("resolveBundleForController: %v", err)
	}

	manifest, err := items.Parse(mat.ItemsYAML)
	if err != nil {
		t.Fatalf("parse items: %v", err)
	}
	got := map[string]items.FolderCredential{}
	for _, it := range manifest.Items {
		for _, p := range it.Properties {
			if p.FolderCredentialsProperty == nil {
				continue
			}
			for _, e := range p.FolderCredentialsProperty.FolderCredentials {
				for _, c := range e.Credentials {
					switch {
					case c.SecretText != nil:
						got[it.Name+"/"+c.SecretText.ID] = c
					case c.UsernamePassword != nil:
						got[it.Name+"/"+c.UsernamePassword.ID] = c
					}
				}
			}
		}
	}
	if c, ok := got["payments/literal"]; !ok || c.SecretText.Secret != "abc${def}" {
		t.Errorf("payments/literal = %+v, want secret abc${def}", c.SecretText)
	}
	c, ok := got["payments/varname"]
	if !ok {
		t.Fatalf("payments/varname is missing, got %+v", got)
	}
	up := c.UsernamePassword
	if up.Password != "${varroa_controller_name}" || up.Username != "u${x}" || up.Description != "uses ${varroa_controller_name}" {
		t.Errorf("a credential field changed through ResolveVars: %+v", up)
	}
	if c, ok := got["team-b/b-token"]; !ok || c.SecretText.Secret != "team-b-value" {
		t.Errorf("the other tenant's credential is missing or changed: %+v", got)
	}
}

func TestInjectTenantFolderCredentials_AppendsToTenantFolder(t *testing.T) {
	in := `items:
  - kind: folder
    name: payments
    properties:
      - folderCredentialsProperty:
          folderCredentials:
            - credentials:
                - secretText:
                    id: from-bundle
                    scope: GLOBAL
                    secret: bundle-value
  - kind: folder
    name: team-b
`
	entry := items.FolderCredentialEntry{Credentials: []items.FolderCredential{{
		SecretText: &items.SecretTextCredential{ID: "projected", Scope: "GLOBAL", Secret: "x${y}"},
	}}}
	out, err := injectTenantFolderCredentials(in, []bundle.CellFolderCredentials{{Folder: "payments", Entries: []items.FolderCredentialEntry{entry}}})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	manifest, err := items.Parse(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	prop := folderCredentialsPropertyOf(&manifest.Items[0])
	if len(prop.FolderCredentials) != 2 {
		t.Fatalf("want the bundle entry and the projected entry, got %+v", prop.FolderCredentials)
	}
	if got := prop.FolderCredentials[1].Credentials[0].SecretText; got == nil || got.Secret != "x${y}" {
		t.Errorf("projected entry = %+v", got)
	}
	if len(manifest.Items[1].Properties) != 0 {
		t.Errorf("another folder changed: %+v", manifest.Items[1])
	}
}

func TestInjectTenantFolderCredentials_MissingFolderFails(t *testing.T) {
	_, err := injectTenantFolderCredentials("items:\n  - kind: folder\n    name: payments\n",
		[]bundle.CellFolderCredentials{{Folder: "gone"}})
	if err == nil {
		t.Fatal("expected an error for a folder that is not in the items")
	}
}

func TestInjectTenantFolderCredentials_NoCredentialsKeepsTheText(t *testing.T) {
	in := "items:\n    - kind: folder\n      name: payments\n"
	out, err := injectTenantFolderCredentials(in, nil)
	if err != nil || out != in {
		t.Fatalf("inject with no credentials = %q, %v; want the input unchanged", out, err)
	}
}
