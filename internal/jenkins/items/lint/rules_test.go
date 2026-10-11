package lint

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
)

func TestNoPollSCMRule_RejectsTriggerPollSCM(t *testing.T) {
	item := items.Item{
		Kind:     "pipeline",
		Name:     "job",
		Triggers: []items.Trigger{{PollSCM: &items.PollSCMTrigger{}}},
	}
	findings := NoPollSCMRule{}.Check(item, "tenant/job")
	if len(findings) != 1 {
		t.Fatalf("expected one finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].Rule != "no-poll-scm" || findings[0].Path != "tenant/job" {
		t.Errorf("unexpected finding: %+v", findings[0])
	}
}

func TestNoPollSCMRule_RejectsPipelineTriggersProperty(t *testing.T) {
	item := items.Item{
		Kind: "pipeline",
		Name: "job",
		Properties: []items.Property{
			{PipelineTriggers: &items.PipelineTriggers{
				Triggers: []items.Trigger{{PollSCM: &items.PollSCMTrigger{}}},
			}},
		},
	}
	findings := NoPollSCMRule{}.Check(item, "tenant/job")
	if len(findings) != 1 {
		t.Fatalf("expected one finding, got %d: %+v", len(findings), findings)
	}
}

func TestNoPollSCMRule_AllowsCronTrigger(t *testing.T) {
	item := items.Item{
		Kind:     "pipeline",
		Name:     "job",
		Triggers: []items.Trigger{{Cron: &items.CronTrigger{Spec: "H * * * *"}}},
	}
	if findings := (NoPollSCMRule{}).Check(item, "tenant/job"); len(findings) != 0 {
		t.Errorf("expected no findings for a cron trigger, got %+v", findings)
	}
}

func TestNoGlobalCredentialsRule_RejectsCredentialOnNonFolderItem(t *testing.T) {
	item := items.Item{
		Kind: "pipeline",
		Name: "job",
		Properties: []items.Property{
			{FolderCredentialsProperty: &items.FolderCredentialsProperty{
				FolderCredentials: []items.FolderCredentialEntry{{
					Credentials: []items.FolderCredential{{SecretText: &items.SecretTextCredential{ID: "x", Scope: "GLOBAL", Secret: "s"}}},
				}},
			}},
		},
	}
	findings := NoGlobalCredentialsRule{}.Check(item, "tenant/job")
	if len(findings) != 1 {
		t.Fatalf("expected one finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].Rule != "no-global-credentials" {
		t.Errorf("unexpected rule id: %q", findings[0].Rule)
	}
}

func TestNoGlobalCredentialsRule_RejectsNonGlobalScope(t *testing.T) {
	item := items.Item{
		Kind: "folder",
		Name: "tenant",
		Properties: []items.Property{
			{FolderCredentialsProperty: &items.FolderCredentialsProperty{
				FolderCredentials: []items.FolderCredentialEntry{{
					Credentials: []items.FolderCredential{{SecretText: &items.SecretTextCredential{ID: "x", Scope: "SYSTEM", Secret: "s"}}},
				}},
			}},
		},
	}
	findings := NoGlobalCredentialsRule{}.Check(item, "tenant")
	if len(findings) != 1 {
		t.Fatalf("expected one finding, got %d: %+v", len(findings), findings)
	}
}

func TestNoGlobalCredentialsRule_AllowsFolderGlobalScope(t *testing.T) {
	item := items.Item{
		Kind: "folder",
		Name: "tenant",
		Properties: []items.Property{
			{FolderCredentialsProperty: &items.FolderCredentialsProperty{
				FolderCredentials: []items.FolderCredentialEntry{{
					Credentials: []items.FolderCredential{
						{SecretText: &items.SecretTextCredential{ID: "x", Scope: "GLOBAL", Secret: "s"}},
						{UsernamePassword: &items.UsernamePasswordCredential{ID: "y", Username: "u", Password: "p"}},
					},
				}},
			}},
		},
	}
	if findings := (NoGlobalCredentialsRule{}).Check(item, "tenant"); len(findings) != 0 {
		t.Errorf("expected no findings for GLOBAL/unset scope on a folder, got %+v", findings)
	}
}

func sshCredentialTenantFolder(scope string) items.Item {
	return items.Item{
		Kind: "folder",
		Name: "tenant",
		Properties: []items.Property{
			{FolderCredentialsProperty: &items.FolderCredentialsProperty{
				FolderCredentials: []items.FolderCredentialEntry{{
					Credentials: []items.FolderCredential{{BasicSSHUserPrivateKey: &items.BasicSSHUserPrivateKeyCredential{
						ID:       "deploy-key",
						Scope:    scope,
						Username: "git",
						PrivateKeySource: items.PrivateKeySource{
							DirectEntry: &items.DirectEntryPrivateKeySource{PrivateKey: "k"},
						},
					}}},
				}},
			}},
		},
	}
}

func TestNoGlobalCredentialsRule_AllowsSSHPrivateKeyGlobalScope(t *testing.T) {
	if findings := (NoGlobalCredentialsRule{}).Check(sshCredentialTenantFolder("GLOBAL"), "tenant"); len(findings) != 0 {
		t.Errorf("expected no findings for a GLOBAL SSH credential on a folder, got %+v", findings)
	}
}

func TestNoGlobalCredentialsRule_RejectsSSHPrivateKeySystemScope(t *testing.T) {
	findings := NoGlobalCredentialsRule{}.Check(sshCredentialTenantFolder("SYSTEM"), "tenant")
	if len(findings) != 1 {
		t.Fatalf("expected one finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].Rule != "no-global-credentials" {
		t.Errorf("unexpected rule id: %q", findings[0].Rule)
	}
}

func TestNoControllerConfigDependencyRule_AllowsEveryExistingPropertyField(t *testing.T) {
	item := items.Item{
		Kind: "folder",
		Name: "tenant",
		Properties: []items.Property{
			{BuildDiscarder: &items.BuildDiscarderProperty{}},
			{DisableConcurrentBuilds: &items.DisableConcurrentBuilds{}},
			{DisableResume: &items.DisableResume{}},
			{Parameters: &items.ParametersProperty{}},
			{GithubProjectProperty: &items.GithubProjectProperty{ProjectURLStr: "https://example.com"}},
			{DurabilityHint: &items.DurabilityHint{Hint: "PERFORMANCE_OPTIMIZED"}},
			{PreserveStashes: &items.PreserveStashes{}},
			{RateLimitBuilds: &items.RateLimitBuilds{}},
			{PipelineTriggers: &items.PipelineTriggers{}},
			{EnvVars: &items.EnvVarsProperty{}},
			{FolderLibraries: &items.FolderLibrariesProperty{}},
			{ItemRestrictions: &items.ItemRestrictionsProperty{}},
			{Kubernetes: &items.KubernetesFolderProperty{PermittedClouds: []string{"team-a"}}},
		},
	}
	if findings := (NoControllerConfigDependencyRule{}).Check(item, "tenant"); len(findings) != 0 {
		t.Errorf("expected every allow-listed property to pass, got %+v", findings)
	}
}

// TestDisallowedFieldNames_RejectsASyntheticName exercises the allow-list
// check directly with a name that items.Property can never actually carry
// today, since every real field is on the allow-list. This is what proves
// the rule fires once a new controller-scoped property is added.
func TestDisallowedFieldNames_RejectsASyntheticName(t *testing.T) {
	got := disallowedFieldNames([]string{"kubernetes", "somethingNew"})
	if len(got) != 1 || got[0] != "somethingNew" {
		t.Fatalf("expected only the synthetic unknown name to be rejected, got %+v", got)
	}
}

func TestDisallowedFieldNames_AllowsEveryAllowListedName(t *testing.T) {
	names := make([]string, 0, len(allowedPropertyFields))
	for name := range allowedPropertyFields {
		names = append(names, name)
	}
	if got := disallowedFieldNames(names); len(got) != 0 {
		t.Errorf("expected every allow-listed name to pass, got %+v", got)
	}
}

func fileCertificateTenantFolder(scope string) items.Item {
	return items.Item{
		Kind: "folder",
		Name: "tenant",
		Properties: []items.Property{
			{FolderCredentialsProperty: &items.FolderCredentialsProperty{
				FolderCredentials: []items.FolderCredentialEntry{{
					Credentials: []items.FolderCredential{
						{File: &items.FileCredential{ID: "kubeconfig", Scope: scope, FileName: "kubeconfig", SecretBytes: "RklMRQ=="}},
						{Certificate: &items.CertificateCredential{
							ID:             "signing",
							Scope:          scope,
							KeyStoreSource: items.KeyStoreSource{Uploaded: &items.UploadedKeyStoreSource{UploadedKeystore: "MIIBAv8="}},
						}},
					},
				}},
			}},
		},
	}
}

func TestNoGlobalCredentialsRule_RejectsFileAndCertificateSystemScope(t *testing.T) {
	findings := NoGlobalCredentialsRule{}.Check(fileCertificateTenantFolder("SYSTEM"), "tenant")
	if len(findings) != 2 {
		t.Fatalf("expected one finding per credential, got %d: %+v", len(findings), findings)
	}
	for _, f := range findings {
		if f.Rule != "no-global-credentials" {
			t.Errorf("unexpected rule id: %q", f.Rule)
		}
	}
}

func TestNoGlobalCredentialsRule_AllowsFileAndCertificateGlobalScope(t *testing.T) {
	if findings := (NoGlobalCredentialsRule{}).Check(fileCertificateTenantFolder("GLOBAL"), "tenant"); len(findings) != 0 {
		t.Errorf("expected no findings for GLOBAL file and certificate credentials, got %+v", findings)
	}
}
