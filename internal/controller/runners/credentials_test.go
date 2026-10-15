package runners

import (
	"context"
	"errors"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// fakeSecretReader returns exactly the records a real label-selector Secret
// list would: only the records configured for the requested namespace. A
// Secret that exists but carries no matching label is simply never added
// here, matching what the real API returns for it.
type fakeSecretReader struct {
	namespace string
	records   []map[string][]byte
	err       error
}

func (f *fakeSecretReader) ListSecrets(_ context.Context, namespace, _ string) ([]map[string][]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	if namespace != f.namespace {
		return nil, nil
	}
	return f.records, nil
}

func secretRecord(name string, data map[string]string) map[string][]byte {
	rec := make(map[string][]byte, len(data)+1)
	for k, v := range data {
		rec[k] = []byte(v)
	}
	rec["_name"] = []byte(name)
	return rec
}

func credentialRunner(credentialRef string) *v1alpha1.Runner {
	r := sshRunner("win-1", "payments")
	r.Spec.CredentialRef = credentialRef
	return r
}

// TestReadCredential_HappyPath is task 5.1's happy-path acceptance.
func TestReadCredential_HappyPath(t *testing.T) {
	secrets := &fakeSecretReader{
		namespace: "agents",
		records: []map[string][]byte{
			secretRecord("win-1-key", map[string]string{
				"username":   "jenkins",
				"privateKey": "PEMDATA",
			}),
		},
	}
	runner := credentialRunner("win-1-key")

	cred, fault := readCredential(context.Background(), secrets, runner, "agents")
	if fault != nil {
		t.Fatalf("fault = %+v, want none", fault)
	}
	if cred.Username != "jenkins" || cred.PrivateKey != "PEMDATA" {
		t.Fatalf("cred = %+v", cred)
	}
	if cred.HasPassphrase {
		t.Fatal("HasPassphrase true with no passphrase key present")
	}
}

func TestReadCredential_HappyPathWithPassphrase(t *testing.T) {
	secrets := &fakeSecretReader{
		namespace: "agents",
		records: []map[string][]byte{
			secretRecord("win-1-key", map[string]string{
				"username":   "jenkins",
				"privateKey": "PEMDATA",
				"passphrase": "s3cret",
			}),
		},
	}
	runner := credentialRunner("win-1-key")

	cred, fault := readCredential(context.Background(), secrets, runner, "agents")
	if fault != nil {
		t.Fatalf("fault = %+v, want none", fault)
	}
	if !cred.HasPassphrase || cred.Passphrase != "s3cret" {
		t.Fatalf("cred = %+v", cred)
	}
}

// TestReadCredential_AbsentSecret is task 5.1's "an absent Secret"
// acceptance.
func TestReadCredential_AbsentSecret(t *testing.T) {
	secrets := &fakeSecretReader{namespace: "agents"}
	runner := credentialRunner("win-1-key")

	_, fault := readCredential(context.Background(), secrets, runner, "agents")
	if fault == nil || fault.Reason != ReasonCredentialMissing {
		t.Fatalf("fault = %+v, want CredentialMissing", fault)
	}
}

// TestReadCredential_UnlabelledSecret is task 5.1's "an unlabelled Secret"
// acceptance: a Secret named win-1-key exists, but not among the records a
// varroa.dev/runner-credential label-selector list would return, so it is
// indistinguishable here from an absent one.
func TestReadCredential_UnlabelledSecret(t *testing.T) {
	secrets := &fakeSecretReader{
		namespace: "agents",
		records: []map[string][]byte{
			secretRecord("some-other-secret", map[string]string{
				"username":   "jenkins",
				"privateKey": "PEMDATA",
			}),
		},
	}
	runner := credentialRunner("win-1-key")

	_, fault := readCredential(context.Background(), secrets, runner, "agents")
	if fault == nil || fault.Reason != ReasonCredentialMissing {
		t.Fatalf("fault = %+v, want CredentialMissing", fault)
	}
}

func TestReadCredential_EmptyCredentialRef(t *testing.T) {
	secrets := &fakeSecretReader{namespace: "agents"}
	runner := sshRunner("win-1", "payments")
	runner.Spec.CredentialRef = ""

	_, fault := readCredential(context.Background(), secrets, runner, "agents")
	if fault == nil || fault.Reason != ReasonCredentialMissing {
		t.Fatalf("fault = %+v, want CredentialMissing", fault)
	}
}

func TestReadCredential_ListError(t *testing.T) {
	secrets := &fakeSecretReader{namespace: "agents", err: errors.New("boom")}
	runner := credentialRunner("win-1-key")

	_, fault := readCredential(context.Background(), secrets, runner, "agents")
	if fault == nil || fault.Reason != ReasonCredentialMissing {
		t.Fatalf("fault = %+v, want CredentialMissing", fault)
	}
}

// TestReadCredential_MissingRequiredKey is task 5.1's "each missing key"
// acceptance, for the two required keys.
func TestReadCredential_MissingRequiredKey(t *testing.T) {
	cases := []struct {
		name string
		data map[string]string
	}{
		{"username", map[string]string{"privateKey": "PEMDATA"}},
		{"privateKey", map[string]string{"username": "jenkins"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			secrets := &fakeSecretReader{
				namespace: "agents",
				records:   []map[string][]byte{secretRecord("win-1-key", tc.data)},
			}
			runner := credentialRunner("win-1-key")

			_, fault := readCredential(context.Background(), secrets, runner, "agents")
			if fault == nil || fault.Reason != ReasonCredentialMalformed {
				t.Fatalf("missing %s: fault = %+v, want CredentialMalformed", tc.name, fault)
			}
		})
	}
}

// TestReadCredential_UnresolvedTokenInEachValue is task 5.1's "a ${ in each
// of the three values" acceptance: every one of username, privateKey, and
// passphrase is injected after bundle.RemainingVarRefs has run, so each carries
// its own check.
func TestReadCredential_UnresolvedTokenInEachValue(t *testing.T) {
	cases := []struct {
		name string
		data map[string]string
	}{
		{"username", map[string]string{"username": "${team}", "privateKey": "PEMDATA"}},
		{"privateKey", map[string]string{"username": "jenkins", "privateKey": "${key}"}},
		{"passphrase", map[string]string{"username": "jenkins", "privateKey": "PEMDATA", "passphrase": "${pass}"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			secrets := &fakeSecretReader{
				namespace: "agents",
				records:   []map[string][]byte{secretRecord("win-1-key", tc.data)},
			}
			runner := credentialRunner("win-1-key")

			_, fault := readCredential(context.Background(), secrets, runner, "agents")
			if fault == nil || fault.Reason != ReasonCredentialMalformed {
				t.Fatalf("${ in %s: fault = %+v, want CredentialMalformed", tc.name, fault)
			}
		})
	}
}

// TestRenderCredential_MatchesDesignYAML is task 5.2's golden-file
// acceptance, matching the shape in design D5.
func TestRenderCredential_MatchesDesignYAML(t *testing.T) {
	runner := credentialRunner("win-1-key")
	cred := &Credential{
		Username:   "jenkins",
		PrivateKey: "-----BEGIN TEST KEY-----\nabc\n-----END TEST KEY-----\n",
	}

	entry, ok := renderCredential(runner, cred).(map[string]any)
	if !ok {
		t.Fatalf("entry = %T, want map[string]any", entry)
	}
	basic, ok := entry["basicSSHUserPrivateKey"].(map[string]any)
	if !ok {
		t.Fatalf("missing basicSSHUserPrivateKey block: %+v", entry)
	}
	if basic["scope"] != "SYSTEM" {
		t.Errorf("scope = %v, want SYSTEM", basic["scope"])
	}
	if basic["id"] != "varroa-runner-win-1" {
		t.Errorf("id = %v, want varroa-runner-win-1", basic["id"])
	}
	if basic["username"] != "jenkins" {
		t.Errorf("username = %v, want jenkins", basic["username"])
	}
	pks, ok := basic["privateKeySource"].(map[string]any)
	if !ok {
		t.Fatalf("missing privateKeySource: %+v", basic)
	}
	direct, ok := pks["directEntry"].(map[string]any)
	if !ok {
		t.Fatalf("missing directEntry: %+v", pks)
	}
	if direct["privateKey"] != cred.PrivateKey {
		t.Errorf("privateKey = %v, want %v", direct["privateKey"], cred.PrivateKey)
	}
	if _, hasPassphrase := basic["passphrase"]; hasPassphrase {
		t.Error("passphrase present when the Secret carried no such key")
	}
}

func TestRenderCredential_IncludesPassphraseWhenPresent(t *testing.T) {
	runner := credentialRunner("win-1-key")
	cred := &Credential{Username: "jenkins", PrivateKey: "PEM", Passphrase: "s3cret", HasPassphrase: true}

	entry := renderCredential(runner, cred).(map[string]any)
	basic := entry["basicSSHUserPrivateKey"].(map[string]any)
	if basic["passphrase"] != "s3cret" {
		t.Errorf("passphrase = %v, want s3cret", basic["passphrase"])
	}
}

// TestRenderCredential_IDMatchesLauncherCredentialsID is task 5.2's second
// acceptance: the credential id must equal the credentialsId task 4.2
// renders on the ssh launcher, for the same Runner.
func TestRenderCredential_IDMatchesLauncherCredentialsID(t *testing.T) {
	runner := credentialRunner("win-1-key")
	cred := &Credential{Username: "jenkins", PrivateKey: "PEM"}

	credEntry := renderCredential(runner, cred).(map[string]any)
	basic := credEntry["basicSSHUserPrivateKey"].(map[string]any)

	node := renderNode(runner)
	ssh := node["permanent"].(map[string]any)["launcher"].(map[string]any)["ssh"].(map[string]any)

	if basic["id"] != ssh["credentialsId"] {
		t.Fatalf("credential id %v != launcher credentialsId %v", basic["id"], ssh["credentialsId"])
	}
}

// TestBuildCredentials_DeliveredSSHRunnerRendersOne proves the exported
// reader renders a credential for a delivered ssh runner placed on cell.
func TestBuildCredentials_DeliveredSSHRunnerRendersOne(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	runner := credentialRunner("win-1-key")
	crdstore.MustSeed(f, runner)

	secrets := &fakeSecretReader{
		namespace: "agents",
		records: []map[string][]byte{
			secretRecord("win-1-key", map[string]string{"username": "jenkins", "privateKey": "PEM"}),
		},
	}

	creds, err := BuildCredentials(context.Background(), f, secrets, cellFor("cell-a"))
	if err != nil {
		t.Fatalf("BuildCredentials: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("Credentials = %d entries, want 1", len(creds))
	}
}

// TestBuildCredentials_InboundRunnerContributesNone proves an inbound
// runner, which never carries an ssh credential, contributes nothing.
func TestBuildCredentials_InboundRunnerContributesNone(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, inboundRunner())

	creds, err := BuildCredentials(context.Background(), f, &fakeSecretReader{namespace: "agents"}, cellFor("cell-a"))
	if err != nil {
		t.Fatalf("BuildCredentials: %v", err)
	}
	if len(creds) != 0 {
		t.Fatalf("Credentials for an inbound-only cell = %d, want 0", len(creds))
	}
}

// TestBuildCredentials_FaultedCredentialContributesNone proves a runner
// whose Secret read faults contributes no credential entry, matching
// "one bad runner does not stop the others" for the credential set.
func TestBuildCredentials_FaultedCredentialContributesNone(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, credentialRunner("missing-key"))

	creds, err := BuildCredentials(context.Background(), f, &fakeSecretReader{namespace: "agents"}, cellFor("cell-a"))
	if err != nil {
		t.Fatalf("BuildCredentials: %v", err)
	}
	if len(creds) != 0 {
		t.Fatalf("Credentials for a faulted runner = %d, want 0", len(creds))
	}
}

// TestBuildCredentials_OnlyThisCell proves a runner placed on a different
// cell contributes nothing.
func TestBuildCredentials_OnlyThisCell(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, credentialRunner("win-1-key"))

	secrets := &fakeSecretReader{
		namespace: "agents",
		records: []map[string][]byte{
			secretRecord("win-1-key", map[string]string{"username": "jenkins", "privateKey": "PEM"}),
		},
	}

	creds, err := BuildCredentials(context.Background(), f, secrets, cellFor("cell-b"))
	if err != nil {
		t.Fatalf("BuildCredentials: %v", err)
	}
	if len(creds) != 0 {
		t.Fatalf("Credentials for the wrong cell = %d, want 0", len(creds))
	}
}
