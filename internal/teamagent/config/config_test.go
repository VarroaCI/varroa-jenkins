package config

import (
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider/factories"
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
	bad := map[string]string{}
	for k, v := range base {
		bad[k] = v
	}
	bad[provider.ConfigEnv] = "{not json"
	cfg, err = FromEnv(envOf(bad))
	if err != nil {
		t.Fatalf("FromEnv with a bad assistant config: %v", err)
	}
	if cfg.AssistantErr == nil {
		t.Error("a malformed assistant config was not kept in AssistantErr")
	}
}

func TestProviderReady(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"providers":[{"name":"local","kind":"openai-compatible","baseURL":"http://127.0.0.1:1/v1",` +
		`"models":["m1"],"trust":"internal","default":true,"hasSecret":false}],` +
		`"externalDataSharing":"metadata","budgets":{"session":1,"userDay":1,"teamDay":1,"steps":1},` +
		`"approvalExpiry":"24h"}`)
	pc, err := provider.Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	reg, err := provider.NewRegistry(pc, factories.DefaultFactories(), dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	cfg := Config{Assistant: pc}

	ta := func(p, m string) *v1alpha1.TeamAgent {
		out := &v1alpha1.TeamAgent{}
		out.Spec.Provider = p
		out.Spec.Model = m
		return out
	}
	cases := []struct {
		name   string
		cfg    Config
		reg    *provider.Registry
		ta     *v1alpha1.TeamAgent
		status metav1.ConditionStatus
		reason string
	}{
		{"default provider and model", cfg, reg, ta("", ""), metav1.ConditionTrue, v1alpha1.TeamAgentReasonReady},
		{"unknown provider", cfg, reg, ta("nope", ""), metav1.ConditionFalse, v1alpha1.TeamAgentReasonUnknownProvider},
		{"unknown model", cfg, reg, ta("local", "missing"), metav1.ConditionFalse, v1alpha1.TeamAgentReasonUnknownModel},
		{"decode error", Config{AssistantErr: os.ErrInvalid}, reg, ta("", ""), metav1.ConditionFalse, v1alpha1.TeamAgentReasonAssistantDisabled},
		{"no registry", cfg, nil, ta("", ""), metav1.ConditionFalse, v1alpha1.TeamAgentReasonAssistantDisabled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, reason, _ := ProviderReady(c.cfg, c.reg, c.ta)
			if status != c.status || reason != c.reason {
				t.Fatalf("ProviderReady = %v %s, want %v %s", status, reason, c.status, c.reason)
			}
		})
	}
}

func TestProviderReady_MissingKey(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"providers":[{"name":"keyed","kind":"openai-compatible","baseURL":"http://127.0.0.1:1/v1",` +
		`"models":["m1"],"trust":"internal","default":true,"hasSecret":true}],` +
		`"externalDataSharing":"metadata","budgets":{"session":1,"userDay":1,"teamDay":1,"steps":1},` +
		`"approvalExpiry":"24h"}`)
	pc, err := provider.Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	reg, err := provider.NewRegistry(pc, factories.DefaultFactories(), dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	out := &v1alpha1.TeamAgent{}
	status, reason, _ := ProviderReady(Config{Assistant: pc}, reg, out)
	if status != metav1.ConditionFalse || reason != v1alpha1.TeamAgentReasonKeyMissing {
		t.Fatalf("no key file: %v %s", status, reason)
	}
	if err := os.MkdirAll(filepath.Join(dir, "keyed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keyed", "apiKey"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, reason, _ = ProviderReady(Config{Assistant: pc}, reg, out)
	if status != metav1.ConditionTrue {
		t.Fatalf("with a key file: %v %s", status, reason)
	}
}
