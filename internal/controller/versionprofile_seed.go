package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// versionProfileSeedLabel is the dedicated ownership guard for objects seeded
// by VersionProfileSeedReconciler — set on both the JenkinsVersionProfile CR
// and its paired pluginset ConfigMap. A live object of the seeded name that
// lacks (or has a future promotion clear) this label is treated as foreign
// and never overwritten, mirroring starterManagedLabel's role in starter.go.
const versionProfileSeedLabel = "varroa.dev/version-profile-seed"

// VersionProfileSeedReconciler seeds the ship-time default JenkinsVersionProfile
// CRs and their pluginset ConfigMaps from content embedded in the operator
// binary (pluginlock.Seeds()), replacing the Helm-chart-templated versions of
// the same objects. It is a separate reconciler from StarterReconciler: the
// two seed unrelated resource kinds from unrelated embedded content stores.
type VersionProfileSeedReconciler struct {
	client            ResourceClient
	store             crdstore.Backend
	operatorNamespace string
	logger            *slog.Logger
}

// NewVersionProfileSeedReconciler creates a new VersionProfileSeedReconciler.
func NewVersionProfileSeedReconciler(client ResourceClient, store crdstore.Backend, operatorNamespace string, logger *slog.Logger) *VersionProfileSeedReconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &VersionProfileSeedReconciler{
		client:            client,
		store:             store,
		operatorNamespace: operatorNamespace,
		logger:            logger,
	}
}

// versionProfileSeedLabels returns the labels stamped on every object this
// reconciler writes: app.kubernetes.io/managed-by for continuity/observability
// (the removed chart template already set it on the CR), and the dedicated
// versionProfileSeedLabel that is the actual ownership guard.
func versionProfileSeedLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "varroa-operator",
		versionProfileSeedLabel:        "true",
	}
}

// versionProfileSeedOwned reports whether labels carry this reconciler's
// ownership marker.
func versionProfileSeedOwned(labels map[string]string) bool {
	return labels[versionProfileSeedLabel] == "true"
}

// Reconcile applies every pluginlock.Seeds() entry: the pluginset ConfigMap
// first, then the JenkinsVersionProfile CR pointing at it. It never deletes a
// profile or ConfigMap for a version no longer present in Seeds() (design
// decision: an orphaned, still-owned object is left in place rather than
// pruned). One profile's failure is logged and does not stop the rest.
func (r *VersionProfileSeedReconciler) Reconcile(ctx context.Context) {
	for _, seed := range pluginlock.Seeds() {
		if err := r.reconcileOne(ctx, seed); err != nil {
			r.logger.Warn("version profile seed failed", "version", seed.Version, "error", err)
		}
	}
}

func (r *VersionProfileSeedReconciler) reconcileOne(ctx context.Context, seed pluginlock.SeedProfile) error {
	name := pluginlock.ProfileName(seed.Version)
	logger := r.logger.With("profile", name)

	// Best-effort preflight: skip the whole entry without touching the
	// ConfigMap if the CR already exists and is foreign (chart-templated but
	// not yet pruned, hand-authored, or promoted). Not a hard guarantee — a
	// concurrent writer can still take the CR between here and the ApplyOwned
	// call below, which re-checks ownership itself.
	live, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](ctx, r.store, name, "")
	profileExists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get profile %s: %w", name, err)
	}
	if profileExists && !versionProfileSeedOwned(live.Labels) {
		logger.Debug("skipping version profile not owned by the seed reconciler")
		return nil
	}

	labels := versionProfileSeedLabels()
	cmName := seed.Spec.PluginSetRef.Name
	if err := r.client.CreateOrUpdateOwnedConfigMap(ctx, cmName, r.operatorNamespace, map[string]string{
		"plugins.yaml": seed.Plugins,
	}, labels); err != nil {
		if errors.Is(err, ErrConfigMapNotOwned) {
			logger.Debug("skipping version profile: pluginset configmap not owned by the seed reconciler", "configMap", cmName)
			return nil
		}
		return fmt.Errorf("write pluginset configmap %s: %w", cmName, err)
	}

	if profileExists && reflect.DeepEqual(live.Spec, seed.Spec) {
		// Already owned and content-identical: skip the write.
		return nil
	}

	profile := &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: seed.Spec,
	}
	if err := crdstore.ApplyOwned(ctx, r.store, profile, func(l *unstructured.Unstructured) bool {
		return versionProfileSeedOwned(l.GetLabels())
	}); err != nil {
		if errors.Is(err, crdstore.ErrNotOwned) {
			logger.Debug("skipping version profile not owned by the seed reconciler")
			return nil
		}
		return fmt.Errorf("apply profile %s: %w", name, err)
	}
	return nil
}
