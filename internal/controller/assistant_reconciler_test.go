package controller

import (
	"context"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func egressPolicyKey() string { return testNamespace + "/" + assistantEgressPolicyName }

func reconcilerFor(store crdstore.Backend, secrets *fakeAssistantSecrets, enabled bool) *AssistantReconciler {
	return &AssistantReconciler{
		Store: store, Secrets: secrets, Namespace: testNamespace,
		NetworkPolicyEnabled: enabled, Logger: testLogger(),
	}
}

func storedStatus(t *testing.T, store crdstore.Backend) v1alpha1.AssistantConfigStatus {
	t.Helper()
	obj, err := crdstore.Get[v1alpha1.AssistantConfig](context.Background(), store, v1alpha1.AssistantConfigName, "")
	if err != nil {
		t.Fatalf("get assistant config: %v", err)
	}
	return obj.Status
}

func TestReconcilePrunesKeyOfRemovedProvider(t *testing.T) {
	store := crdstore.NewFake()
	assistantObject(t, store, keyProvider("smoke"))
	secrets := newFakeAssistantSecrets()
	secrets.secrets[testNamespace+"/"+v1alpha1.AssistantKeySecretName] = keySecret(true, map[string]string{"smoke": "sk-keep", "gone": "sk-drop"})
	if err := reconcilerFor(store, secrets, true).Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	data := secrets.secrets[testNamespace+"/"+v1alpha1.AssistantKeySecretName].Data
	if _, ok := data["gone"]; ok {
		t.Fatalf("key of a removed provider survived: %v", data)
	}
	if string(data["smoke"]) != "sk-keep" {
		t.Fatalf("key of a listed provider was removed: %v", data)
	}
}

// TestReconcilePrunesKeyOfProviderWithoutAPIKeyAuth pins that a listed provider
// with auth none loses a key that was stored for it.
func TestReconcilePrunesKeyOfProviderWithoutAPIKeyAuth(t *testing.T) {
	store := crdstore.NewFake()
	keyless := keyProvider("smoke")
	keyless.Auth = v1alpha1.AssistantAuthNone
	assistantObject(t, store, keyless)
	secrets := newFakeAssistantSecrets()
	secrets.secrets[testNamespace+"/"+v1alpha1.AssistantKeySecretName] = keySecret(true, map[string]string{"smoke": "sk-stale"})
	if err := reconcilerFor(store, secrets, true).Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if _, ok := secrets.secrets[testNamespace+"/"+v1alpha1.AssistantKeySecretName].Data["smoke"]; ok {
		t.Fatalf("key of a provider with auth none survived")
	}
}

// TestReconcileKeepsKeyStoredDuringTick pins the read order of a tick. A key
// that a responder stores while the tick runs belongs to a provider that is
// listed by then. The prune must keep it.
func TestReconcileKeepsKeyStoredDuringTick(t *testing.T) {
	store := crdstore.NewFake()
	assistantObject(t, store, keyProvider("smoke"))
	secrets := newFakeAssistantSecrets()
	secrets.secrets[testNamespace+"/"+v1alpha1.AssistantKeySecretName] = keySecret(true, map[string]string{"smoke": "sk-first"})
	secrets.beforeRead = func() {
		// The responder path: list a new provider, then store its key.
		cur, err := crdstore.Get[v1alpha1.AssistantConfig](context.Background(), store, v1alpha1.AssistantConfigName, "")
		if err != nil {
			t.Fatalf("read config in hook: %v", err)
		}
		cur.Spec.Providers = append(cur.Spec.Providers, keyProvider("added"))
		if err := crdstore.ApplyAt[v1alpha1.AssistantConfig](context.Background(), store, cur); err != nil {
			t.Fatalf("list provider in hook: %v", err)
		}
		secrets.secrets[testNamespace+"/"+v1alpha1.AssistantKeySecretName].Data["added"] = []byte("sk-added")
		secrets.beforeRead = nil
	}
	if err := reconcilerFor(store, secrets, true).Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if string(secrets.secrets[testNamespace+"/"+v1alpha1.AssistantKeySecretName].Data["added"]) != "sk-added" {
		t.Fatalf("the tick removed the key of a provider that was listed during the tick")
	}
}

// TestReconcileAdoptsUnlabelledSecret pins that a Secret created by hand gets
// the operator labels, and that only its listed keys survive.
func TestReconcileAdoptsUnlabelledSecret(t *testing.T) {
	store := crdstore.NewFake()
	assistantObject(t, store, keyProvider("smoke"))
	secrets := newFakeAssistantSecrets()
	secrets.secrets[testNamespace+"/"+v1alpha1.AssistantKeySecretName] = keySecret(false, map[string]string{"smoke": "sk-hand", "stray": "sk-stray"})
	if err := reconcilerFor(store, secrets, true).Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	s := secrets.secrets[testNamespace+"/"+v1alpha1.AssistantKeySecretName]
	if s.Labels["app.kubernetes.io/managed-by"] != "varroa-operator" {
		t.Fatalf("hand-made Secret was not adopted: %v", s.Labels)
	}
	if string(s.Data["smoke"]) != "sk-hand" {
		t.Fatalf("listed key of the adopted Secret was lost")
	}
	if _, ok := s.Data["stray"]; ok {
		t.Fatalf("unlisted key of the adopted Secret survived")
	}
}

// TestReconcileKeepsSecretWithoutObject pins that a missing AssistantConfig
// never removes a stored credential.
func TestReconcileKeepsSecretWithoutObject(t *testing.T) {
	store := crdstore.NewFake()
	secrets := newFakeAssistantSecrets()
	secrets.secrets[testNamespace+"/"+v1alpha1.AssistantKeySecretName] = keySecret(true, map[string]string{"smoke": "sk-kept"})
	if err := reconcilerFor(store, secrets, true).Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if string(secrets.secrets[testNamespace+"/"+v1alpha1.AssistantKeySecretName].Data["smoke"]) != "sk-kept" {
		t.Fatalf("a missing object removed a stored key")
	}
}

func TestReconcileAppliesEgressPolicy(t *testing.T) {
	store := crdstore.NewFake()
	assistantObject(t, store, keyProvider("smoke"))
	secrets := newFakeAssistantSecrets()
	if err := reconcilerFor(store, secrets, true).Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if secrets.policies[egressPolicyKey()] == nil {
		t.Fatalf("egress policy was not applied")
	}
}

func TestReconcileDeletesPolicyWhenNetworkPolicyOff(t *testing.T) {
	store := crdstore.NewFake()
	assistantObject(t, store, keyProvider("smoke"))
	secrets := newFakeAssistantSecrets()
	secrets.policies[egressPolicyKey()] = &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: assistantEgressPolicyName, Namespace: testNamespace}}
	if err := reconcilerFor(store, secrets, false).Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if secrets.policies[egressPolicyKey()] != nil {
		t.Fatalf("egress policy survived with network policy off")
	}
}

func TestReconcileDeletesPolicyWhenNoRule(t *testing.T) {
	store := crdstore.NewFake()
	noCIDR := keyProvider("smoke")
	noCIDR.CIDRs = nil
	assistantObject(t, store, noCIDR)
	secrets := newFakeAssistantSecrets()
	secrets.policies[egressPolicyKey()] = &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: assistantEgressPolicyName, Namespace: testNamespace}}
	if err := reconcilerFor(store, secrets, true).Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if secrets.policies[egressPolicyKey()] != nil {
		t.Fatalf("egress policy survived with no rule")
	}
}

// TestReconcileWritesStatusKeySet pins the key state that the dashboard reads.
func TestReconcileWritesStatusKeySet(t *testing.T) {
	store := crdstore.NewFake()
	assistantObject(t, store, keyProvider("smoke"), keyProvider("other"))
	secrets := newFakeAssistantSecrets()
	secrets.secrets[testNamespace+"/"+v1alpha1.AssistantKeySecretName] = keySecret(true, map[string]string{"smoke": "sk-set"})
	if err := reconcilerFor(store, secrets, true).Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	status := storedStatus(t, store)
	got := map[string]bool{}
	for _, p := range status.Providers {
		got[p.Name] = p.KeySet
	}
	if !got["smoke"] || got["other"] {
		t.Fatalf("keySet per provider: %v, want smoke true and other false", got)
	}
}

// TestReconcileConditionNamesProviderWithoutCIDRs pins the EgressReady condition
// when a listed provider has no CIDRs.
func TestReconcileConditionNamesProviderWithoutCIDRs(t *testing.T) {
	store := crdstore.NewFake()
	missing := keyProvider("lonely")
	missing.CIDRs = nil
	assistantObject(t, store, keyProvider("smoke"), missing)
	secrets := newFakeAssistantSecrets()
	if err := reconcilerFor(store, secrets, true).Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	status := storedStatus(t, store)
	var cond *metav1.Condition
	for i := range status.Conditions {
		if status.Conditions[i].Type == v1alpha1.AssistantConfigCondEgressReady {
			cond = &status.Conditions[i]
		}
	}
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.AssistantEgressReasonMissing {
		t.Fatalf("EgressReady condition %+v, want False ProviderEgressMissing", cond)
	}
	if !strings.Contains(cond.Message, "lonely") || strings.Contains(cond.Message, "smoke") {
		t.Fatalf("condition message %q must name only lonely", cond.Message)
	}
	// The policy still renders for the provider that has CIDRs.
	if secrets.policies[egressPolicyKey()] == nil {
		t.Fatalf("policy for the provider with CIDRs was not applied")
	}
}
