package runners

import (
	"context"
	"fmt"
	"strings"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// The two Ready=False reasons tick step 7 (the credential read) raises,
// per design D2 and D5.
const (
	ReasonCredentialMissing   = "CredentialMissing"
	ReasonCredentialMalformed = "CredentialMalformed"
)

// runnerCredentialLabelSelector marks a Secret as an ssh runner's SSH key
// (design D5). It is a distinct label from C5's varroa.dev/tenant-credential:
// a runner key must never become a folder credential, because a folder
// credential is visible to builds.
const runnerCredentialLabelSelector = "varroa.dev/runner-credential=true"

// unresolvedVarToken marks a JCasC substitution the operator refuses to
// carry into a credential value. bundle.RemainingVarRefs runs earlier in
// desired-state assembly, before this Secret is read, so a value carrying
// this token would otherwise reach core unresolved.
const unresolvedVarToken = "${"

// SecretReader is the Secret access this package needs to project an ssh
// runner's credential: list-by-label in one namespace. It is satisfied by
// the operator's own Kubernetes client with no adapter.
type SecretReader interface {
	ListSecrets(ctx context.Context, namespace, labelSelector string) ([]map[string][]byte, error)
}

// Credential is one ssh runner's SSH login material, read from its
// labelled Secret (design D5).
type Credential struct {
	Username      string
	PrivateKey    string
	Passphrase    string
	HasPassphrase bool
}

// CredentialFault is a step-7 read fault: one of the two Ready=False
// reasons above, plus the message a Runner's status would carry.
type CredentialFault struct {
	Reason  string
	Message string
}

func (f *CredentialFault) Error() string {
	return fmt.Sprintf("%s: %s", f.Reason, f.Message)
}

// BuildCredentials returns the SYSTEM-scope credential blocks of the ssh
// Runners delivered to cell: the Credentials field of BuildContribution.
func BuildCredentials(ctx context.Context, backend crdstore.Backend, secrets SecretReader, cell *v1alpha1.Controller) ([]any, error) {
	contrib, err := BuildContribution(ctx, backend, secrets, cell)
	if err != nil {
		return nil, err
	}
	return contrib.Credentials, nil
}

// readCredential resolves runner's spec.credentialRef against the Secrets
// labelled varroa.dev/runner-credential: "true" in agentNamespace (design
// D5). A fault return means the runner is not deliverable for its ssh
// launcher; the caller renders no credential for it.
func readCredential(ctx context.Context, secrets SecretReader, runner *v1alpha1.Runner, agentNamespace string) (*Credential, *CredentialFault) {
	if runner.Spec.CredentialRef == "" {
		return nil, &CredentialFault{Reason: ReasonCredentialMissing, Message: "spec.credentialRef is empty"}
	}

	records, err := secrets.ListSecrets(ctx, agentNamespace, runnerCredentialLabelSelector)
	if err != nil {
		return nil, &CredentialFault{Reason: ReasonCredentialMissing, Message: fmt.Sprintf("list runner-credential secrets: %v", err)}
	}

	var data map[string][]byte
	for _, rec := range records {
		if string(rec["_name"]) == runner.Spec.CredentialRef {
			data = rec
			break
		}
	}
	if data == nil {
		return nil, &CredentialFault{
			Reason:  ReasonCredentialMissing,
			Message: fmt.Sprintf("no secret %q labelled %s in namespace %q", runner.Spec.CredentialRef, runnerCredentialLabelSelector, agentNamespace),
		}
	}

	username := string(data["username"])
	privateKey := string(data["privateKey"])
	if username == "" || privateKey == "" {
		return nil, &CredentialFault{
			Reason:  ReasonCredentialMalformed,
			Message: fmt.Sprintf("secret %q is missing the username or privateKey data key", runner.Spec.CredentialRef),
		}
	}

	passphraseBytes, hasPassphrase := data["passphrase"]
	passphrase := string(passphraseBytes)

	for _, v := range []string{username, privateKey, passphrase} {
		if strings.Contains(v, unresolvedVarToken) {
			return nil, &CredentialFault{
				Reason:  ReasonCredentialMalformed,
				Message: fmt.Sprintf("secret %q carries an unresolved %s substitution token", runner.Spec.CredentialRef, unresolvedVarToken),
			}
		}
	}

	return &Credential{
		Username:      username,
		PrivateKey:    privateKey,
		Passphrase:    passphrase,
		HasPassphrase: hasPassphrase,
	}, nil
}

// renderCredential builds one SYSTEM-scope global credentials.system.
// domainCredentials[].credentials[] entry for runner's ssh key (design D5).
// Its id is varroa-runner-<Runner object name>, the same value
// renderLauncher's ssh block uses as credentialsId.
func renderCredential(runner *v1alpha1.Runner, cred *Credential) any {
	entry := map[string]any{
		"scope":    "SYSTEM",
		"id":       credentialID(runner.Name),
		"username": cred.Username,
		"privateKeySource": map[string]any{
			"directEntry": map[string]any{
				"privateKey": cred.PrivateKey,
			},
		},
	}
	if cred.HasPassphrase {
		entry["passphrase"] = cred.Passphrase
	}
	return map[string]any{"basicSSHUserPrivateKey": entry}
}
