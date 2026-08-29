package bundle

import (
	"slices"
	"testing"
)

func TestInjectedVariableNames_OIDCClientSecretRemoved(t *testing.T) {
	// varroa_oidc_client_secret must NOT be in InjectedVariableNames, while
	// varroa_oidc_issuer and varroa_oidc_client_id must still be present.
	if slices.Contains(InjectedVariableNames, "varroa_oidc_client_secret") {
		t.Error("varroa_oidc_client_secret must not be in InjectedVariableNames after #411 fix")
	}
	if !slices.Contains(InjectedVariableNames, "varroa_oidc_issuer") {
		t.Error("varroa_oidc_issuer must remain in InjectedVariableNames")
	}
	if !slices.Contains(InjectedVariableNames, "varroa_oidc_client_id") {
		t.Error("varroa_oidc_client_id must remain in InjectedVariableNames")
	}
}
