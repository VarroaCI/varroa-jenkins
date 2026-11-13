package controller

import (
	"context"
	"log/slog"
	"reflect"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// AssistantReconciler makes the key Secret and the egress policy follow the
// AssistantConfig object. It runs on the leader. Each tick it:
//   - prunes the key Secret: every key that no listed provider with auth apiKey
//     owns is removed, and an unlabelled Secret is adopted;
//   - applies the egress policy, or deletes it when no rule remains or when
//     network policy is off;
//   - writes the status: each provider's key state and the EgressReady
//     condition.
//
// With no AssistantConfig object the reconciler keeps the key Secret as it is,
// so a missing object never removes a stored credential.
type AssistantReconciler struct {
	Store     crdstore.Backend
	Secrets   AssistantSecretStore
	Namespace string
	// NetworkPolicyEnabled mirrors VARROA_NETWORKPOLICY_ENABLED. When false the
	// reconciler never renders the egress policy.
	NetworkPolicyEnabled bool
	Logger               *slog.Logger
}

// Tick runs one reconcile pass.
func (r *AssistantReconciler) Tick(ctx context.Context) error {
	// The Secret is read before the object. A key that a responder stores for a
	// provider added later is then always listed by the object read that follows.
	// Reversing the order would let the prune remove that new key.
	labels, data, found, err := r.Secrets.ReadSecretMeta(ctx, v1alpha1.AssistantKeySecretName, r.Namespace)
	if err != nil {
		return err
	}
	obj, err := crdstore.Get[v1alpha1.AssistantConfig](ctx, r.Store, v1alpha1.AssistantConfigName, "")
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		obj = nil
	}
	keyOwners := map[string]bool{}
	if obj != nil {
		for _, p := range obj.Spec.Providers {
			if p.Auth != v1alpha1.AssistantAuthNone {
				keyOwners[p.Name] = true
			}
		}
	}
	if obj != nil && found {
		if err := r.pruneKeys(ctx, labels, data, keyOwners); err != nil {
			return err
		}
	}

	var missing []string
	if obj == nil || !r.NetworkPolicyEnabled {
		if err := r.Secrets.DeleteNetworkPolicy(ctx, assistantEgressPolicyName, r.Namespace); err != nil {
			return err
		}
		if obj != nil {
			_, missing = buildAssistantEgressPolicy(r.Namespace, obj.Spec.Providers)
		}
	} else {
		np, miss := buildAssistantEgressPolicy(r.Namespace, obj.Spec.Providers)
		missing = miss
		if np == nil {
			if err := r.Secrets.DeleteNetworkPolicy(ctx, assistantEgressPolicyName, r.Namespace); err != nil {
				return err
			}
		} else if err := r.Secrets.ApplyNetworkPolicy(ctx, np); err != nil {
			return err
		}
	}

	if obj == nil {
		return nil
	}
	keys := map[string]bool{}
	if found {
		for k := range data {
			keys[k] = true
		}
	}
	return r.writeStatus(ctx, obj, keys, missing)
}

// pruneKeys removes each key that no listed apiKey provider owns, and labels
// an unlabelled Secret so the operator owns it from then on.
func (r *AssistantReconciler) pruneKeys(ctx context.Context, labels map[string]string, data map[string][]byte, owners map[string]bool) error {
	var remove []string
	for k := range data {
		if !owners[k] {
			remove = append(remove, k)
		}
	}
	sort.Strings(remove)
	if len(remove) == 0 && hasAssistantKeyLabels(labels) {
		return nil
	}
	if len(remove) > 0 && r.Logger != nil {
		r.Logger.Info("assistant key Secret: removing keys of providers no longer listed", "count", len(remove))
	}
	return r.Secrets.WriteSecretKeys(ctx, v1alpha1.AssistantKeySecretName, r.Namespace, assistantKeyLabels(), nil, remove)
}

// writeStatus writes the provider key states and the EgressReady condition when
// they differ from the stored status.
func (r *AssistantReconciler) writeStatus(ctx context.Context, obj *v1alpha1.AssistantConfig, keys map[string]bool, missing []string) error {
	status := v1alpha1.AssistantConfigStatus{
		ObservedGeneration: obj.Generation,
		Conditions:         append([]metav1.Condition(nil), obj.Status.Conditions...),
	}
	for _, p := range obj.Spec.Providers {
		status.Providers = append(status.Providers, v1alpha1.AssistantProviderStatus{
			Name:   p.Name,
			KeySet: p.Auth != v1alpha1.AssistantAuthNone && keys[p.Name],
		})
	}
	cond := metav1.Condition{
		Type:               v1alpha1.AssistantConfigCondEgressReady,
		Status:             metav1.ConditionTrue,
		Reason:             v1alpha1.AssistantEgressReasonReady,
		Message:            "every provider has an egress rule",
		ObservedGeneration: obj.Generation,
	}
	if len(missing) > 0 {
		cond.Status = metav1.ConditionFalse
		cond.Reason = v1alpha1.AssistantEgressReasonMissing
		cond.Message = "no CIDRs for provider(s): " + strings.Join(missing, ", ")
	}
	meta.SetStatusCondition(&status.Conditions, cond)
	if reflect.DeepEqual(status, obj.Status) {
		return nil
	}
	return crdstore.PatchStatus[v1alpha1.AssistantConfig](ctx, r.Store, v1alpha1.AssistantConfigName, "", status)
}
