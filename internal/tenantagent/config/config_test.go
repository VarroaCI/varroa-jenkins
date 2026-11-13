package config

import (
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider/factories"
	"github.com/varroaci/varroa-jenkins/internal/assistantconfig"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestFromEnv(t *testing.T) {
	base := map[string]string{
		"VARROA_RECORDSTORE_DSN": "postgres://x",
		"VARROA_BFF_URL":         "http://varroa-bff:8080/",
		"VARROA_CLUSTER":         "core",
		"POD_NAME":               "varroa-agent-0",
		"POD_NAMESPACE":          "varroa-system",
	}
	cfg, err := FromEnv(envOf(base))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.BFFURL != "http://varroa-bff:8080" {
		t.Errorf("BFFURL = %q, want the trailing slash removed", cfg.BFFURL)
	}
	if cfg.KeyPath != DefaultKeyPath {
		t.Errorf("KeyPath = %q, want the chart default", cfg.KeyPath)
	}
	for _, drop := range []string{"VARROA_RECORDSTORE_DSN", "VARROA_BFF_URL", "VARROA_CLUSTER"} {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		delete(m, drop)
		if _, err := FromEnv(envOf(m)); err == nil {
			t.Errorf("FromEnv without %s returned no error", drop)
		}
	}
}

// snapOf builds the snapshot of an AssistantConfig spec. The key files live in dir.
func snapOf(t *testing.T, dir string, spec v1alpha1.AssistantConfigSpec) *assistantconfig.Snapshot {
	t.Helper()
	snap := assistantconfig.Build(&v1alpha1.AssistantConfig{Spec: spec}, factories.DefaultFactories(), dir)
	if snap.Err != nil {
		t.Fatalf("Build: %v", snap.Err)
	}
	return snap
}

func localSpec(auth string) v1alpha1.AssistantConfigSpec {
	return v1alpha1.AssistantConfigSpec{
		Providers: []v1alpha1.AssistantProvider{{
			Name: "local", Kind: v1alpha1.AssistantProviderKindOpenAICompatible, BaseURL: "http://127.0.0.1:1/v1",
			Models: []string{"m1"}, Trust: v1alpha1.AssistantTrustInternal, Auth: auth,
		}},
		DefaultProvider: "local",
		DefaultModel:    "m1",
	}
}

func TestProviderReady(t *testing.T) {
	dir := t.TempDir()
	snap := snapOf(t, dir, localSpec(v1alpha1.AssistantAuthNone))
	bad := localSpec(v1alpha1.AssistantAuthNone)
	bad.ApprovalExpiry = "soon"
	broken := assistantconfig.Build(&v1alpha1.AssistantConfig{Spec: bad}, factories.DefaultFactories(), dir)
	if broken.Err == nil {
		t.Fatal("a bad approval expiry built without an error")
	}

	ta := func(p, m string) *v1alpha1.TenantAgent {
		out := &v1alpha1.TenantAgent{}
		out.Spec.Provider = p
		out.Spec.Model = m
		return out
	}
	cases := []struct {
		name   string
		snap   *assistantconfig.Snapshot
		ta     *v1alpha1.TenantAgent
		status metav1.ConditionStatus
		reason string
	}{
		{"default provider and model", snap, ta("", ""), metav1.ConditionTrue, v1alpha1.TenantAgentReasonReady},
		{"unknown provider", snap, ta("nope", ""), metav1.ConditionFalse, v1alpha1.TenantAgentReasonUnknownProvider},
		{"unknown model", snap, ta("local", "missing"), metav1.ConditionFalse, v1alpha1.TenantAgentReasonUnknownModel},
		{"snapshot error", broken, ta("", ""), metav1.ConditionFalse, v1alpha1.TenantAgentReasonAssistantDisabled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, reason, _ := ProviderReady(c.snap, c.ta)
			if status != c.status || reason != c.reason {
				t.Fatalf("ProviderReady = %v %s, want %v %s", status, reason, c.status, c.reason)
			}
		})
	}
}

// TestProviderReadyReportsSnapshotError proves that a snapshot that did not
// build reports AssistantDisabled on a TenantAgent, whatever it names.
func TestProviderReadyReportsSnapshotError(t *testing.T) {
	bad := localSpec(v1alpha1.AssistantAuthNone)
	bad.ApprovalExpiry = "soon"
	snap := assistantconfig.Build(&v1alpha1.AssistantConfig{Spec: bad}, factories.DefaultFactories(), t.TempDir())
	status, reason, _ := ProviderReady(snap, &v1alpha1.TenantAgent{})
	if status != metav1.ConditionFalse || reason != v1alpha1.TenantAgentReasonAssistantDisabled {
		t.Fatalf("ProviderReady = %v %s, want False AssistantDisabled", status, reason)
	}
}

func TestProviderReady_MissingKey(t *testing.T) {
	dir := t.TempDir()
	snap := snapOf(t, dir, localSpec(v1alpha1.AssistantAuthAPIKey))
	out := &v1alpha1.TenantAgent{}
	status, reason, _ := ProviderReady(snap, out)
	if status != metav1.ConditionFalse || reason != v1alpha1.TenantAgentReasonKeyMissing {
		t.Fatalf("no key file: %v %s", status, reason)
	}
	if err := os.WriteFile(filepath.Join(dir, "local"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, reason, _ = ProviderReady(snap, out)
	if status != metav1.ConditionTrue {
		t.Fatalf("with a key file: %v %s", status, reason)
	}
}
