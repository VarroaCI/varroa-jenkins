package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// JenkinsVersionProfileReconciler reconciles JenkinsVersionProfile CRDs.
// It materializes referenced plugin sets into owned ConfigMaps and sets
// status conditions (PluginSetReady, LockJcascMismatch).
type JenkinsVersionProfileReconciler struct {
	client            ResourceClient
	store             crdstore.Backend
	operatorNamespace string
	logger            *slog.Logger
}

// NewJenkinsVersionProfileReconciler creates a new JenkinsVersionProfileReconciler.
func NewJenkinsVersionProfileReconciler(client ResourceClient, store crdstore.Backend, operatorNamespace string, logger *slog.Logger) *JenkinsVersionProfileReconciler {
	return &JenkinsVersionProfileReconciler{
		client:            client,
		store:             store,
		operatorNamespace: operatorNamespace,
		logger:            logger,
	}
}

// Reconcile reconciles a single JenkinsVersionProfile by name.
func (r *JenkinsVersionProfileReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	name := req.Name
	logger := r.logger.With("profile", name)

	profile, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](ctx, r.store, name, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("JenkinsVersionProfile not found, skipping")
			return reconcile.Result{}, nil
		}
		logger.Error("failed to get JenkinsVersionProfile", "error", err)
		return reconcile.Result{}, err
	}

	// Build initial status from current state.
	status := &v1alpha1.JenkinsVersionProfileStatus{}
	profile.Status.DeepCopyInto(status)
	if status.Conditions == nil {
		status.Conditions = []v1alpha1.JenkinsVersionProfileCondition{}
	}

	// --- Plugin set materialization ---
	if profile.Spec.PluginSetRef == nil {
		// Metadata/JCasC-only profile: no materialization needed.
		// Do NOT set PluginSetReady=True, leave contentRef empty.
		logger.Debug("no pluginSetRef, metadata/JCasC-only profile")
	} else {
		r.materializePluginSet(ctx, profile, status, logger)
	}

	// --- Lock↔JCasC warning condition (non-blocking) ---
	if profile.Spec.JCasC != nil && len(profile.Spec.JCasC.RequiredPlugins) > 0 {
		r.checkRequiredPlugins(profile, status, logger)
	} else {
		// Clear the condition if JCasC is nil or has no required plugins.
		r.setCondition(status, v1alpha1.JenkinsVersionProfileCondition{
			Type:               "LockJcascMismatch",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "NoRequiredPlugins",
			Message:            "no required plugins configured",
		})
	}

	// Patch status.
	if err := r.patchStatus(ctx, name, status); err != nil {
		logger.Error("failed to patch status", "error", err)
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// materializePluginSet reads the source ConfigMap, parses it, and writes an
// owner-referenced copy. On failure it sets PluginSetReady=False and does NOT
// wipe an existing contentRef.
func (r *JenkinsVersionProfileReconciler) materializePluginSet(ctx context.Context, profile *v1alpha1.JenkinsVersionProfile, status *v1alpha1.JenkinsVersionProfileStatus, logger *slog.Logger) {
	ref := profile.Spec.PluginSetRef

	cmData, err := r.client.GetConfigMap(ctx, ref.Name, r.operatorNamespace)
	if err != nil {
		logger.Warn("failed to get source ConfigMap for plugin set", "configMap", ref.Name, "namespace", r.operatorNamespace, "error", err)
		r.setCondition(status, v1alpha1.JenkinsVersionProfileCondition{
			Type:               "PluginSetReady",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "SourceUnavailable",
			Message:            fmt.Sprintf("source ConfigMap %s/%s: %v", r.operatorNamespace, ref.Name, err),
		})
		// Do NOT wipe an existing contentRef.
		return
	}

	pluginsYAML, ok := cmData["plugins.yaml"]
	if !ok || pluginsYAML == "" {
		logger.Warn("source ConfigMap has no plugins.yaml key", "configMap", ref.Name)
		r.setCondition(status, v1alpha1.JenkinsVersionProfileCondition{
			Type:               "PluginSetReady",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "SourceUnavailable",
			Message:            fmt.Sprintf("source ConfigMap %s/%s has no plugins.yaml key", r.operatorNamespace, ref.Name),
		})
		return
	}

	// Parse the lock set (same shape as pluginlock.lockSet).
	var lockSet struct {
		Core    []string                 `yaml:"core"`
		Plugins []pluginlock.PluginEntry `yaml:"plugins"`
	}
	if err := yaml.Unmarshal([]byte(pluginsYAML), &lockSet); err != nil {
		logger.Warn("failed to parse plugins.yaml as lock set", "error", err)
		r.setCondition(status, v1alpha1.JenkinsVersionProfileCondition{
			Type:               "PluginSetReady",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "SourceUnavailable",
			Message:            fmt.Sprintf("parse plugins.yaml: %v", err),
		})
		return
	}

	// Canonicalize the lock set back to YAML for the owned ConfigMap.
	canonical, err := yaml.Marshal(lockSet)
	if err != nil {
		logger.Warn("failed to marshal canonicalized lock set", "error", err)
		r.setCondition(status, v1alpha1.JenkinsVersionProfileCondition{
			Type:               "PluginSetReady",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "SourceUnavailable",
			Message:            fmt.Sprintf("marshal lock set: %v", err),
		})
		return
	}

	// Distinct from the Helm-owned source ConfigMap (profile.Spec.PluginSetRef,
	// named "<profile>-pluginset"). Writing the owned copy back over that name
	// would strip Helm's ownership labels/annotations and break helm upgrade.
	contentName := profile.Name + "-pluginset-content"
	owner := jenkinsVersionProfileOwnerRef(profile)
	if err := r.client.CreateOrUpdateConfigMapWithOwner(ctx, contentName, r.operatorNamespace, map[string]string{
		"plugins.yaml": string(canonical),
	}, owner); err != nil {
		logger.Warn("failed to write owned ConfigMap", "configMap", contentName, "error", err)
		r.setCondition(status, v1alpha1.JenkinsVersionProfileCondition{
			Type:               "PluginSetReady",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "SourceUnavailable",
			Message:            fmt.Sprintf("write ConfigMap %s: %v", contentName, err),
		})
		return
	}

	status.ContentRef = contentName
	status.PluginCount = len(lockSet.Plugins)
	r.setCondition(status, v1alpha1.JenkinsVersionProfileCondition{
		Type:               "PluginSetReady",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "Materialized",
		Message:            fmt.Sprintf("plugin set materialized into %s (%d plugins)", contentName, len(lockSet.Plugins)),
	})
}

// checkRequiredPlugins checks if all required plugins from JCasC are present
// in the materialized plugin set. Non-blocking: sets LockJcascMismatch but
// never affects PluginSetReady.
func (r *JenkinsVersionProfileReconciler) checkRequiredPlugins(profile *v1alpha1.JenkinsVersionProfile, status *v1alpha1.JenkinsVersionProfileStatus, logger *slog.Logger) {
	required := profile.Spec.JCasC.RequiredPlugins

	// Build a set of materialized artifactIds. If no materialized set yet,
	// use the empty set (warns on all required plugins).
	materialized := map[string]bool{}
	if status.ContentRef != "" {
		// We need the plugin IDs from the spec plugin set. Since we may not
		// have them in status, read from the profile spec's source.
		// If pluginSetRef is set, the source CM was already parsed by
		// materializePluginSet. We stored the IDs there. But we don't have
		// access to the parsed set in this method without re-reading.
		// Simplest approach: if the profile has a pluginSetRef, re-read the
		// source ConfigMap and parse to get artifactIds. Otherwise, warn on all.
		if profile.Spec.PluginSetRef != nil {
			cmData, err := r.client.GetConfigMap(context.Background(), profile.Spec.PluginSetRef.Name, r.operatorNamespace)
			if err == nil {
				if pluginsYAML, ok := cmData["plugins.yaml"]; ok && pluginsYAML != "" {
					var lockSet struct {
						Plugins []pluginlock.PluginEntry `yaml:"plugins"`
					}
					if err := yaml.Unmarshal([]byte(pluginsYAML), &lockSet); err == nil {
						for _, p := range lockSet.Plugins {
							materialized[p.ArtifactID] = true
						}
					}
				}
			}
		}
	}

	var missing []string
	for _, rid := range required {
		if !materialized[rid] {
			missing = append(missing, rid)
		}
	}

	if len(missing) > 0 {
		r.setCondition(status, v1alpha1.JenkinsVersionProfileCondition{
			Type:               "LockJcascMismatch",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "MissingPlugins",
			Message:            fmt.Sprintf("required plugins not in pinned set: %s", strings.Join(missing, ", ")),
		})
		logger.Warn("required plugins missing from pinned set", "missing", missing)
	} else {
		r.setCondition(status, v1alpha1.JenkinsVersionProfileCondition{
			Type:               "LockJcascMismatch",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "AllRequiredPluginsPresent",
			Message:            "all required plugins are present in the pinned set",
		})
	}
}

// setCondition upserts a condition on the status.
func (r *JenkinsVersionProfileReconciler) setCondition(status *v1alpha1.JenkinsVersionProfileStatus, cond v1alpha1.JenkinsVersionProfileCondition) {
	for i, existing := range status.Conditions {
		if existing.Type == cond.Type {
			if existing.Status == cond.Status && existing.Reason == cond.Reason && existing.Message == cond.Message {
				cond.LastTransitionTime = existing.LastTransitionTime
			}
			status.Conditions[i] = cond
			return
		}
	}
	status.Conditions = append(status.Conditions, cond)
}

// patchStatus patches the status subresource of a JenkinsVersionProfile.
func (r *JenkinsVersionProfileReconciler) patchStatus(ctx context.Context, name string, status *v1alpha1.JenkinsVersionProfileStatus) error {
	return crdstore.PatchStatus[v1alpha1.JenkinsVersionProfile](ctx, r.store, name, "", status)
}

// jenkinsVersionProfileOwnerRef returns an OwnerReference for a JenkinsVersionProfile.
func jenkinsVersionProfileOwnerRef(p *v1alpha1.JenkinsVersionProfile) metav1.OwnerReference {
	controller := true
	apiVersion := p.APIVersion
	kind := p.Kind
	if apiVersion == "" {
		apiVersion = v1alpha1.SchemeGroupVersion.String()
	}
	if kind == "" {
		kind = "JenkinsVersionProfile"
	}
	return metav1.OwnerReference{
		APIVersion:         apiVersion,
		Kind:               kind,
		Name:               p.Name,
		UID:                p.UID,
		BlockOwnerDeletion: &controller,
		Controller:         &controller,
	}
}

// Ensure JenkinsVersionProfileReconciler implements reconcile.Reconciler.
var _ reconcile.Reconciler = (*JenkinsVersionProfileReconciler)(nil)
