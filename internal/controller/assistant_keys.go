package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// assistantKeyMaxBytes is the longest provider key the operator accepts.
const assistantKeyMaxBytes = 4096

// AssistantSecretStore is the Secret and NetworkPolicy access of the assistant
// key and egress code. ClientsetClient implements it.
type AssistantSecretStore interface {
	// ReadSecretMeta answers the labels and data of a Secret. found is false
	// when the Secret is absent.
	ReadSecretMeta(ctx context.Context, name, namespace string) (labels map[string]string, data map[string][]byte, found bool, err error)
	// WriteSecretKeys changes the named data keys and the labels of a Secret.
	WriteSecretKeys(ctx context.Context, name, namespace string, labels map[string]string, set map[string][]byte, remove []string) error
	// ApplyNetworkPolicy creates or updates a NetworkPolicy.
	ApplyNetworkPolicy(ctx context.Context, np *networkingv1.NetworkPolicy) error
	// DeleteNetworkPolicy deletes a NetworkPolicy. A missing policy is not an error.
	DeleteNetworkPolicy(ctx context.Context, name, namespace string) error
}

// assistantKeyLabels label every object that the assistant code owns.
func assistantKeyLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "varroa-operator",
		"app.kubernetes.io/component":  "assistant",
	}
}

// hasAssistantKeyLabels reports whether the Secret carries every label of
// assistantKeyLabels.
func hasAssistantKeyLabels(labels map[string]string) bool {
	for k, v := range assistantKeyLabels() {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// AssistantKeys serves operator.<cluster>.assistantkeys.set and .clear. It
// writes one data key of the key Secret. A key never enters a reply, a log, or
// an event.
type AssistantKeys struct {
	Store     crdstore.Backend
	Secrets   AssistantSecretStore
	Namespace string
	// OnChange runs after a key write, so the reconciler reports the new key
	// state at once. Nil means no hook.
	OnChange func()
	Logger   *slog.Logger
}

// HandleSet handles operator.<cluster>.assistantkeys.set. The provider must be
// listed with auth apiKey.
func (a *AssistantKeys) HandleSet(data []byte) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var req bus.AssistantKeyRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return assistantKeyReply(bus.CodeInvalid, "invalid request")
	}
	if !validAssistantKey(req.APIKey) {
		return assistantKeyReply(bus.CodeInvalid, "the key must be 1 to 4096 bytes and not only whitespace")
	}
	if code, msg := a.checkListedKeyProvider(ctx, req.Provider); code != "" {
		return assistantKeyReply(code, msg)
	}
	set := map[string][]byte{req.Provider: req.APIKey}
	if err := a.Secrets.WriteSecretKeys(ctx, v1alpha1.AssistantKeySecretName, a.Namespace, assistantKeyLabels(), set, nil); err != nil {
		a.logError("write the assistant key Secret", req.Provider, err)
		return assistantKeyReply(bus.CodeInternal, "the key Secret could not be written")
	}
	a.changed()
	return assistantKeyReply("", "")
}

// HandleClear handles operator.<cluster>.assistantkeys.clear. An absent key
// Secret answers success, because no key is set.
func (a *AssistantKeys) HandleClear(data []byte) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var req bus.AssistantKeyRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return assistantKeyReply(bus.CodeInvalid, "invalid request")
	}
	if code, msg := a.checkListedKeyProvider(ctx, req.Provider); code != "" {
		return assistantKeyReply(code, msg)
	}
	_, keys, found, err := a.Secrets.ReadSecretMeta(ctx, v1alpha1.AssistantKeySecretName, a.Namespace)
	if err != nil {
		a.logError("read the assistant key Secret", req.Provider, err)
		return assistantKeyReply(bus.CodeInternal, "the key Secret could not be read")
	}
	if !found {
		return assistantKeyReply("", "")
	}
	if _, ok := keys[req.Provider]; !ok {
		return assistantKeyReply("", "")
	}
	if err := a.Secrets.WriteSecretKeys(ctx, v1alpha1.AssistantKeySecretName, a.Namespace, assistantKeyLabels(), nil, []string{req.Provider}); err != nil {
		a.logError("clear the assistant key", req.Provider, err)
		return assistantKeyReply(bus.CodeInternal, "the key Secret could not be written")
	}
	a.changed()
	return assistantKeyReply("", "")
}

// checkListedKeyProvider answers a refusal code when the provider is not listed
// in the AssistantConfig or does not take a key. An empty code means the
// provider takes a key.
func (a *AssistantKeys) checkListedKeyProvider(ctx context.Context, name string) (string, string) {
	if name == "" {
		return bus.CodeInvalid, "a provider name is required"
	}
	obj, err := crdstore.Get[v1alpha1.AssistantConfig](ctx, a.Store, v1alpha1.AssistantConfigName, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			return bus.CodeNotFound, "no AssistantConfig is stored"
		}
		a.logError("read the AssistantConfig", name, err)
		return bus.CodeInternal, "the AssistantConfig could not be read"
	}
	for _, p := range obj.Spec.Providers {
		if p.Name != name {
			continue
		}
		if p.Auth == v1alpha1.AssistantAuthNone {
			return bus.CodeInvalid, "the provider takes no key"
		}
		return "", ""
	}
	return bus.CodeNotFound, "the provider is not listed in the AssistantConfig"
}

// changed runs the OnChange hook when one is set.
func (a *AssistantKeys) changed() {
	if a.OnChange != nil {
		a.OnChange()
	}
}

// logError records a failed key operation. It names the provider and the
// error. It never names the key.
func (a *AssistantKeys) logError(action, provider string, err error) {
	if a.Logger == nil {
		return
	}
	a.Logger.Error("assistant key: "+action, "provider", provider, "error", err)
}

// validAssistantKey reports whether a key has 1 to 4096 bytes and holds a byte
// that is not whitespace.
func validAssistantKey(key []byte) bool {
	if len(key) == 0 || len(key) > assistantKeyMaxBytes {
		return false
	}
	return len(bytes.TrimSpace(key)) > 0
}

// assistantKeyReply encodes an AssistantKeyResponse. The message never holds a
// key.
func assistantKeyReply(code, msg string) []byte {
	out, err := json.Marshal(bus.AssistantKeyResponse{Code: code, Error: strings.TrimSpace(msg)})
	if err != nil {
		return []byte(`{"code":"internal"}`)
	}
	return out
}
