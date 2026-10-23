package forgeops

import (
	"context"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	return asStatus(err).Code
}

func TestChooseCredential_Label(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	fx.secrets.addCred("s1", "a", "tok-a")
	if _, err := fx.ops.chooseCredential(context.Background(), "payments", testHost); err != nil {
		t.Fatal(err)
	}
	if fx.secrets.selector != "varroa.dev/tenant-credential=true" {
		t.Fatalf("selector %q", fx.secrets.selector)
	}
	if fx.secrets.selector != v1alpha1.TenantCredentialSelector {
		t.Fatalf("selector differs from the shared constant")
	}
}

func TestChooseCredential_Kinds(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	fx.secrets.addCred("ok", "a", "tok-a")
	fx.secrets.records = append(fx.secrets.records, map[string][]byte{"_name": []byte("ssh"), "privatekey": []byte("k")})
	fx.secrets.ann["ssh"] = map[string]string{
		v1alpha1.CredentialIDAnnotation:   "ssh-id",
		v1alpha1.CredentialKindAnnotation: "sshPrivateKey",
		"varroa.dev/allowed-hosts":        testHost,
	}
	fx.secrets.records = append(fx.secrets.records, map[string][]byte{"_name": []byte("app"), "github-app-private-key": []byte("k")})
	fx.secrets.ann["app"] = map[string]string{
		v1alpha1.CredentialIDAnnotation:   "app-id",
		v1alpha1.CredentialKindAnnotation: "githubApp",
		"varroa.dev/allowed-hosts":        testHost,
	}
	creds, err := fx.ops.chooseCredential(context.Background(), "payments", testHost)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 || creds[0].ID != "a" {
		t.Fatalf("creds %+v", creds)
	}
}

func TestChooseCredential_HostCheck(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	fx.secrets.addCred("other", "a", "tok-a")
	fx.secrets.ann["other"]["varroa.dev/allowed-hosts"] = "other.example.org"
	fx.secrets.addCred("none", "b", "tok-b")
	delete(fx.secrets.ann["none"], "varroa.dev/allowed-hosts")
	_, err := fx.ops.chooseCredential(context.Background(), "payments", testHost)
	if got := codeOf(t, err); got != forgeapi.CodeNoMatchingCredential {
		t.Fatalf("code %q", got)
	}
}

func TestChooseCredential_IDOrder(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	fx.secrets.addCred("sb", "b", "tok-b")
	fx.secrets.addCred("sa", "a", "tok-a")
	fx.secrets.addCred("sc", "c", "tok-c")
	creds, err := fx.ops.chooseCredential(context.Background(), "payments", testHost)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c"}
	for i, c := range creds {
		if c.ID != want[i] {
			t.Fatalf("order %d = %q, want %q", i, c.ID, want[i])
		}
	}
}

func TestChooseCredential_NoTenant(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	_, err := fx.ops.chooseCredential(context.Background(), "missing", testHost)
	if got := codeOf(t, err); got != forgeapi.CodeNoMatchingCredential {
		t.Fatalf("code %q", got)
	}
}

func TestChooseCredential_NoPipelines(t *testing.T) {
	fx := newFixture(t, "")
	_, err := fx.ops.chooseCredential(context.Background(), "payments", testHost)
	if got := codeOf(t, err); got != forgeapi.CodeNoMatchingCredential {
		t.Fatalf("code %q", got)
	}
}
