package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// fakeAssistantSecrets is an in-memory AssistantSecretStore. It keeps the key
// Secret and the policies, and it counts the calls that change them.
type fakeAssistantSecrets struct {
	secrets  map[string]*corev1.Secret
	policies map[string]*networkingv1.NetworkPolicy
	writes   int
	// beforeRead, when set, runs at the start of ReadSecretMeta. A test uses it
	// to place a concurrent write between two reads.
	beforeRead func()
}

func newFakeAssistantSecrets() *fakeAssistantSecrets {
	return &fakeAssistantSecrets{
		secrets:  map[string]*corev1.Secret{},
		policies: map[string]*networkingv1.NetworkPolicy{},
	}
}

func (f *fakeAssistantSecrets) ReadSecretMeta(_ context.Context, name, namespace string) (map[string]string, map[string][]byte, bool, error) {
	if f.beforeRead != nil {
		f.beforeRead()
	}
	s, ok := f.secrets[namespace+"/"+name]
	if !ok {
		return nil, nil, false, nil
	}
	labels := map[string]string{}
	for k, v := range s.Labels {
		labels[k] = v
	}
	data := map[string][]byte{}
	for k, v := range s.Data {
		data[k] = append([]byte(nil), v...)
	}
	return labels, data, true, nil
}

func (f *fakeAssistantSecrets) WriteSecretKeys(_ context.Context, name, namespace string, labels map[string]string, set map[string][]byte, remove []string) error {
	f.writes++
	key := namespace + "/" + name
	s, ok := f.secrets[key]
	if !ok {
		s = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{}}, Data: map[string][]byte{}}
		f.secrets[key] = s
	}
	for k, v := range labels {
		s.Labels[k] = v
	}
	for k, v := range set {
		s.Data[k] = append([]byte(nil), v...)
	}
	for _, k := range remove {
		delete(s.Data, k)
	}
	return nil
}

func (f *fakeAssistantSecrets) ApplyNetworkPolicy(_ context.Context, np *networkingv1.NetworkPolicy) error {
	f.policies[np.Namespace+"/"+np.Name] = np.DeepCopy()
	return nil
}

func (f *fakeAssistantSecrets) DeleteNetworkPolicy(_ context.Context, name, namespace string) error {
	delete(f.policies, namespace+"/"+name)
	return nil
}

// keySecret builds a key Secret with the given keys.
func keySecret(labeled bool, keys map[string]string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.AssistantKeySecretName, Namespace: testNamespace, Labels: map[string]string{}}, Data: map[string][]byte{}}
	if labeled {
		s.Labels = assistantKeyLabels()
	}
	for k, v := range keys {
		s.Data[k] = []byte(v)
	}
	return s
}

const testNamespace = "varroa-system"

// assistantObject stores an AssistantConfig with the given providers.
func assistantObject(t *testing.T, store crdstore.Backend, providers ...v1alpha1.AssistantProvider) {
	t.Helper()
	cr := v1alpha1.AssistantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.AssistantConfigName},
		Spec: v1alpha1.AssistantConfigSpec{
			Providers:       providers,
			DefaultProvider: providers[0].Name,
		},
	}
	if err := crdstore.ApplyAt[v1alpha1.AssistantConfig](context.Background(), store, &cr); err != nil {
		t.Fatalf("store assistant config: %v", err)
	}
}

// keyProvider is a listed provider that takes a key.
func keyProvider(name string) v1alpha1.AssistantProvider {
	return v1alpha1.AssistantProvider{
		Name: name, Kind: v1alpha1.AssistantProviderKindOpenAICompatible, BaseURL: "https://" + name + ".test/v1",
		Models: []string{"model-a"}, Trust: v1alpha1.AssistantTrustInternal, Auth: v1alpha1.AssistantAuthAPIKey,
		CIDRs: []string{"192.0.2.10/32"},
	}
}

func newTestKeys(store crdstore.Backend, secrets AssistantSecretStore) *AssistantKeys {
	return &AssistantKeys{Store: store, Secrets: secrets, Namespace: testNamespace, Logger: testLogger()}
}

func keyRequest(t *testing.T, provider, key string) []byte {
	t.Helper()
	return mustMarshal(t, bus.AssistantKeyRequest{Provider: provider, APIKey: []byte(key)})
}

func decodeKeyReply(t *testing.T, raw []byte) bus.AssistantKeyResponse {
	t.Helper()
	var resp bus.AssistantKeyResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return resp
}

func TestKeySetCreatesSecret(t *testing.T) {
	store := crdstore.NewFake()
	assistantObject(t, store, keyProvider("smoke"))
	secrets := newFakeAssistantSecrets()
	a := newTestKeys(store, secrets)
	if resp := decodeKeyReply(t, a.HandleSet(keyRequest(t, "smoke", "sk-probe-one"))); resp.Code != "" {
		t.Fatalf("set: code %q: %s", resp.Code, resp.Error)
	}
	s := secrets.secrets[testNamespace+"/"+v1alpha1.AssistantKeySecretName]
	if s == nil || string(s.Data["smoke"]) != "sk-probe-one" {
		t.Fatalf("key Secret after set: %+v", s)
	}
	if s.Labels["app.kubernetes.io/managed-by"] != "varroa-operator" {
		t.Fatalf("created Secret is not labelled: %v", s.Labels)
	}
}

func TestKeySetPatchesOneKey(t *testing.T) {
	store := crdstore.NewFake()
	assistantObject(t, store, keyProvider("smoke"), keyProvider("other"))
	secrets := newFakeAssistantSecrets()
	secrets.secrets[testNamespace+"/"+v1alpha1.AssistantKeySecretName] = keySecret(true, map[string]string{"other": "sk-other"})
	a := newTestKeys(store, secrets)
	if resp := decodeKeyReply(t, a.HandleSet(keyRequest(t, "smoke", "sk-probe-two"))); resp.Code != "" {
		t.Fatalf("set: code %q: %s", resp.Code, resp.Error)
	}
	s := secrets.secrets[testNamespace+"/"+v1alpha1.AssistantKeySecretName]
	if string(s.Data["other"]) != "sk-other" || string(s.Data["smoke"]) != "sk-probe-two" {
		t.Fatalf("data after a one-key set: %v", s.Data)
	}
}

func TestKeySetRefusesUnknownProvider(t *testing.T) {
	store := crdstore.NewFake()
	assistantObject(t, store, keyProvider("smoke"))
	secrets := newFakeAssistantSecrets()
	a := newTestKeys(store, secrets)
	resp := decodeKeyReply(t, a.HandleSet(keyRequest(t, "stranger", "sk-probe-three")))
	if resp.Code != bus.CodeNotFound {
		t.Fatalf("unknown provider: code %q, want not_found", resp.Code)
	}
	if len(secrets.secrets) != 0 {
		t.Fatalf("an unknown provider wrote a Secret")
	}
}

func TestKeySetRefusesKeylessProvider(t *testing.T) {
	store := crdstore.NewFake()
	keyless := keyProvider("local")
	keyless.Auth = v1alpha1.AssistantAuthNone
	assistantObject(t, store, keyless)
	secrets := newFakeAssistantSecrets()
	a := newTestKeys(store, secrets)
	resp := decodeKeyReply(t, a.HandleSet(keyRequest(t, "local", "sk-probe-four")))
	if resp.Code != bus.CodeInvalid {
		t.Fatalf("keyless provider: code %q, want invalid", resp.Code)
	}
	if len(secrets.secrets) != 0 {
		t.Fatalf("a keyless provider wrote a Secret")
	}
}

func TestKeySetRefusesInvalidKey(t *testing.T) {
	store := crdstore.NewFake()
	assistantObject(t, store, keyProvider("smoke"))
	secrets := newFakeAssistantSecrets()
	a := newTestKeys(store, secrets)
	for _, key := range []string{"", "   ", strings.Repeat("k", assistantKeyMaxBytes+1)} {
		if resp := decodeKeyReply(t, a.HandleSet(keyRequest(t, "smoke", key))); resp.Code != bus.CodeInvalid {
			t.Fatalf("key of %d bytes: code %q, want invalid", len(key), resp.Code)
		}
	}
	if len(secrets.secrets) != 0 {
		t.Fatalf("an invalid key wrote a Secret")
	}
}

func TestKeyClearRemovesKey(t *testing.T) {
	store := crdstore.NewFake()
	assistantObject(t, store, keyProvider("smoke"), keyProvider("other"))
	secrets := newFakeAssistantSecrets()
	secrets.secrets[testNamespace+"/"+v1alpha1.AssistantKeySecretName] = keySecret(true, map[string]string{"smoke": "sk-a", "other": "sk-b"})
	a := newTestKeys(store, secrets)
	clearReq := mustMarshal(t, bus.AssistantKeyRequest{Provider: "smoke"})
	if resp := decodeKeyReply(t, a.HandleClear(clearReq)); resp.Code != "" {
		t.Fatalf("clear: code %q: %s", resp.Code, resp.Error)
	}
	data := secrets.secrets[testNamespace+"/"+v1alpha1.AssistantKeySecretName].Data
	if _, ok := data["smoke"]; ok || string(data["other"]) != "sk-b" {
		t.Fatalf("data after clear: %v", data)
	}
}

func TestKeyClearWithoutSecretWritesNothing(t *testing.T) {
	store := crdstore.NewFake()
	assistantObject(t, store, keyProvider("smoke"))
	secrets := newFakeAssistantSecrets()
	a := newTestKeys(store, secrets)
	clearReq := mustMarshal(t, bus.AssistantKeyRequest{Provider: "smoke"})
	if resp := decodeKeyReply(t, a.HandleClear(clearReq)); resp.Code != "" {
		t.Fatalf("clear with no Secret: code %q", resp.Code)
	}
	if secrets.writes != 0 || len(secrets.secrets) != 0 {
		t.Fatalf("a clear with no Secret created or wrote one")
	}
}

// TestKeyReplyNeverHoldsKey pins that no reply carries the key, on success or
// on refusal.
func TestKeyReplyNeverHoldsKey(t *testing.T) {
	const key = "sk-reply-probe-7d21"
	store := crdstore.NewFake()
	assistantObject(t, store, keyProvider("smoke"))
	secrets := newFakeAssistantSecrets()
	a := newTestKeys(store, secrets)
	replies := [][]byte{
		a.HandleSet(keyRequest(t, "smoke", key)),
		a.HandleSet(keyRequest(t, "stranger", key)),
		a.HandleClear(mustMarshal(t, bus.AssistantKeyRequest{Provider: "smoke"})),
	}
	for i, raw := range replies {
		if strings.Contains(string(raw), key) {
			t.Fatalf("reply %d holds the key: %s", i, raw)
		}
	}
}
