package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/oci"
	"github.com/varroaci/varroa-jenkins/internal/pluginresolve"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"

	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// maxPreflightFailures caps ProfileCandidateStatus.Preflight.FailingControllers
// — the true count is always carried in ControllersFailing even when the
// list itself is truncated.
const maxPreflightFailures = 50

// maxFailingControllerMessageLen caps ProfileCandidateFailingController.Message.
const maxFailingControllerMessageLen = 200

// candidateRequeueAfter is how soon a candidate stuck on a retryable
// condition (ErrMetadataUnavailable, an unreadable in-cluster UC inventory)
// is re-reconciled, mirroring the tracker's own conservative-but-not-slow
// posture.
const candidateRequeueAfter = 5 * time.Minute

// rootHPIPath is where the operator-signed VarroaSecurityRealm HPI is laid
// down in the operator image (see clientset_client.go's init-container copy
// step). It is a build artifact, not an embed, so it is read from the local
// filesystem at point of use.
const rootHPIPath = "/opt/varroa/varroa-mite-auth.hpi"

// candidateLockSet is the shape of both a JenkinsVersionProfile's
// "<profile>-pluginset-content" ConfigMap and a ProfileCandidate's own
// "<candidate>-closure" ConfigMap: a seed core list plus a flat plugin list.
// It matches versionprofile_controller.go's inline lockSet shape exactly.
type candidateLockSet struct {
	Core    []string                 `yaml:"core"`
	Plugins []pluginlock.PluginEntry `yaml:"plugins"`
}

// ProfileCandidateReconciler resolves, verifies, and pre-flights a single
// ProfileCandidate.
type ProfileCandidateReconciler struct {
	client            ResourceClient
	store             crdstore.Backend
	operatorNamespace string
	ucBaseURL         string
	activityPublisher activity.EventSink
	logger            *slog.Logger

	httpDoer interface {
		Do(*http.Request) (*http.Response, error)
	}
	newRegistryStore func(ref string, opts oci.RegistryOptions) (oci.BlobStore, error)
	readRootHPI      func() ([]byte, error)

	// upstreamBaseURL is the online-source base, defaulting to
	// upstreamUpdatesBaseURL. Test-overridable so online-path tests run
	// against a local httptest server rather than updates.jenkins.io.
	upstreamBaseURL string
}

// candidateHTTPTimeout bounds every outbound call this reconciler makes (the
// in-cluster update-center inventory fetch and upstream metadata
// resolution). It runs on a leader-gated ticker that reconciles candidates
// one after another in a single goroutine, so a connection that stalls after
// accepting must fail within one candidate's slice of the tick rather than
// blocking it — and every other candidate behind it — indefinitely. 30
// seconds comfortably covers a small JSON metadata fetch while staying well
// under the 30-second tick interval and the 5-minute retryable-failure
// backoff both call sites back off into on error.
const candidateHTTPTimeout = 30 * time.Second

// NewProfileCandidateReconciler creates a new ProfileCandidateReconciler.
func NewProfileCandidateReconciler(client ResourceClient, store crdstore.Backend, operatorNamespace, ucBaseURL string, activityPublisher activity.EventSink, logger *slog.Logger) *ProfileCandidateReconciler {
	return &ProfileCandidateReconciler{
		client:            client,
		store:             store,
		operatorNamespace: operatorNamespace,
		ucBaseURL:         ucBaseURL,
		activityPublisher: activityPublisher,
		logger:            logger,
		httpDoer:          &http.Client{Timeout: candidateHTTPTimeout},
		newRegistryStore: func(ref string, opts oci.RegistryOptions) (oci.BlobStore, error) {
			return oci.NewRegistryStore(ref, opts)
		},
		readRootHPI:     func() ([]byte, error) { return os.ReadFile(rootHPIPath) },
		upstreamBaseURL: upstreamUpdatesBaseURL,
	}
}

// Reconcile reconciles a single ProfileCandidate by name. Only candidates in
// Phase Pending or with an empty/absent Phase are processed — Ready, Failed,
// Promoted, and Superseded are all terminal-for-this-reconciler outcomes
// (Ready blocks here only on PluginsServable and is re-driven by staying in
// Pending; Promoted is set exclusively by the promotion handler in §5).
func (r *ProfileCandidateReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	name := req.Name
	logger := r.logger.With("candidate", name)

	candidate, err := crdstore.Get[v1alpha1.ProfileCandidate](ctx, r.store, name, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("ProfileCandidate not found, skipping")
			return reconcile.Result{}, nil
		}
		logger.Error("failed to get ProfileCandidate", "error", err)
		return reconcile.Result{}, err
	}

	if candidate.Status.Phase != "" && candidate.Status.Phase != v1alpha1.ProfileCandidatePhasePending {
		logger.Debug("candidate not in scope for this reconciler", "phase", candidate.Status.Phase)
		return reconcile.Result{}, nil
	}

	status := &v1alpha1.ProfileCandidateStatus{}
	candidate.Status.DeepCopyInto(status)
	if status.Conditions == nil {
		status.Conditions = []v1alpha1.ProfileCandidateCondition{}
	}

	profile, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](ctx, r.store, candidate.Spec.ProfileRef, "")
	if err != nil {
		logger.Error("failed to get referenced JenkinsVersionProfile", "profile", candidate.Spec.ProfileRef, "error", err)
		return reconcile.Result{}, err
	}

	// --- Step 0.d: select the metadata source and HPI fetcher once. Step 1
	// needs a working fetch on every pass, whether or not step 0.b actually
	// runs this time, so this happens before resolution and is shared by both.
	source, fetch, err := r.selectSource(ctx, candidate.Spec.ResolveVersion, logger)
	if err != nil {
		logger.Warn("failed to select metadata source; retrying later", "error", err)
		if candidate.Spec.ClosureContentRef == "" {
			r.setCondition(status, v1alpha1.ProfileCandidateCondition{
				Type:               v1alpha1.ConditionCandidateResolved,
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             "MetadataUnavailable",
				Message:            fmt.Sprintf("selecting metadata source: %v", err),
			})
		}
		if patchErr := r.patchStatus(ctx, name, status); patchErr != nil {
			logger.Error("failed to patch status", "error", patchErr)
			return reconcile.Result{}, patchErr
		}
		return reconcile.Result{RequeueAfter: candidateRequeueAfter}, nil
	}

	// --- Step 0.a-c: closure resolution ---
	closure, requeue, err := r.resolveClosure(ctx, candidate, profile, source, status, logger)
	if err != nil {
		if errors.Is(err, pluginresolve.ErrMetadataUnavailable) {
			if patchErr := r.patchStatus(ctx, name, status); patchErr != nil {
				logger.Error("failed to patch status", "error", patchErr)
				return reconcile.Result{}, patchErr
			}
			return reconcile.Result{RequeueAfter: candidateRequeueAfter}, nil
		}
		status.Phase = v1alpha1.ProfileCandidatePhaseFailed
		if patchErr := r.patchStatus(ctx, name, status); patchErr != nil {
			logger.Error("failed to patch status", "error", patchErr)
			return reconcile.Result{}, patchErr
		}
		return reconcile.Result{}, nil
	}
	if requeue {
		if patchErr := r.patchStatus(ctx, name, status); patchErr != nil {
			logger.Error("failed to patch status", "error", patchErr)
			return reconcile.Result{}, patchErr
		}
		return reconcile.Result{RequeueAfter: candidateRequeueAfter}, nil
	}

	// --- Step 1: bootstrap closure + core-floor verification ---
	failed := r.verifyClosure(ctx, candidate, closure, fetch, status, logger)

	// --- Step 2: plugins-servable check ---
	pluginsServable, pluginsServableChecked := r.checkPluginsServable(ctx, closure, status, logger)

	// --- Step 3: advisory fleet pre-flight (never blocking) ---
	r.checkPreflight(ctx, candidate, profile, closure, status, logger)

	// --- Step 4: phase determination ---
	switch {
	case failed:
		status.Phase = v1alpha1.ProfileCandidatePhaseFailed
	case !pluginsServableChecked:
		// Inventory fetch failed/timed out/unparseable: leave PluginsServable at
		// its prior value and requeue rather than hard-failing.
		if err := r.patchStatus(ctx, name, status); err != nil {
			logger.Error("failed to patch status", "error", err)
			return reconcile.Result{}, err
		}
		return reconcile.Result{RequeueAfter: candidateRequeueAfter}, nil
	case pluginsServable:
		status.Phase = v1alpha1.ProfileCandidatePhaseReady
	default:
		// PluginsServable=False for either WaitingForUpdateCenter or
		// PullThroughPending holds Pending: only Pending candidates are ever
		// re-reconciled, so this is what makes the later re-check happen.
		status.Phase = v1alpha1.ProfileCandidatePhasePending
	}

	if err := r.patchStatus(ctx, name, status); err != nil {
		logger.Error("failed to patch status", "error", err)
		return reconcile.Result{}, err
	}

	if status.Phase == v1alpha1.ProfileCandidatePhasePending {
		return reconcile.Result{RequeueAfter: candidateRequeueAfter}, nil
	}
	return reconcile.Result{}, nil
}

func (r *ProfileCandidateReconciler) patchStatus(ctx context.Context, name string, status *v1alpha1.ProfileCandidateStatus) error {
	return crdstore.PatchStatus[v1alpha1.ProfileCandidate](ctx, r.store, name, "", status)
}

func (r *ProfileCandidateReconciler) setCondition(status *v1alpha1.ProfileCandidateStatus, cond v1alpha1.ProfileCandidateCondition) {
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

// resolveClosure resolves the candidate's plugin closure. If
// Spec.ClosureContentRef is already set (a prior pass resolved the closure
// and got blocked later, e.g. by PluginsServable), it reads that ConfigMap
// back instead of re-running the expensive network/registry resolution.
func (r *ProfileCandidateReconciler) resolveClosure(ctx context.Context, candidate *v1alpha1.ProfileCandidate, profile *v1alpha1.JenkinsVersionProfile, source pluginresolve.MetadataSource, status *v1alpha1.ProfileCandidateStatus, logger *slog.Logger) (pluginresolve.Closure, bool, error) {
	if candidate.Spec.ClosureContentRef != "" {
		lockSet, err := r.readLockSetConfigMap(ctx, candidate.Spec.ClosureContentRef)
		if err != nil {
			logger.Warn("failed to re-read existing closure ConfigMap", "configMap", candidate.Spec.ClosureContentRef, "error", err)
			r.setCondition(status, v1alpha1.ProfileCandidateCondition{
				Type:               v1alpha1.ConditionCandidateResolved,
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             "MetadataUnavailable",
				Message:            fmt.Sprintf("re-reading closure ConfigMap %s: %v", candidate.Spec.ClosureContentRef, err),
			})
			return pluginresolve.Closure{}, true, nil
		}
		r.setCondition(status, v1alpha1.ProfileCandidateCondition{
			Type:               v1alpha1.ConditionCandidateResolved,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "Resolved",
			Message:            fmt.Sprintf("%d plugins resolved for %s", len(lockSet.Plugins), candidate.Spec.ResolveVersion),
		})
		return lockSetToClosure(lockSet), false, nil
	}

	// 0.a: read the profile's materialized plugin set.
	lockSet, err := r.readProfileLockSet(ctx, profile)
	if err != nil {
		logger.Warn("failed to read profile plugin set", "profile", profile.Name, "error", err)
		r.setCondition(status, v1alpha1.ProfileCandidateCondition{
			Type:               v1alpha1.ConditionCandidateResolved,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "MetadataUnavailable",
			Message:            fmt.Sprintf("reading profile %s plugin set: %v", profile.Name, err),
		})
		return pluginresolve.Closure{}, true, nil
	}

	// 0.b: resolve the transitive closure.
	closure, err := pluginresolve.Resolve(ctx, candidate.Spec.ResolveVersion, lockSet.Core, source)
	if err != nil {
		if errors.Is(err, pluginresolve.ErrMetadataUnavailable) {
			r.setCondition(status, v1alpha1.ProfileCandidateCondition{
				Type:               v1alpha1.ConditionCandidateResolved,
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             "MetadataUnavailable",
				Message:            fmt.Sprintf("resolving closure for %s: %v", candidate.Spec.ResolveVersion, err),
			})
			return pluginresolve.Closure{}, false, pluginresolve.ErrMetadataUnavailable
		}
		r.setCondition(status, v1alpha1.ProfileCandidateCondition{
			Type:               v1alpha1.ConditionCandidateResolved,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "ResolutionFailed",
			Message:            fmt.Sprintf("resolving closure for %s: %v", candidate.Spec.ResolveVersion, err),
		})
		return pluginresolve.Closure{}, false, err
	}

	r.setCondition(status, v1alpha1.ProfileCandidateCondition{
		Type:               v1alpha1.ConditionCandidateResolved,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "Resolved",
		Message:            fmt.Sprintf("%d plugins resolved for %s", len(closure.Plugins), candidate.Spec.ResolveVersion),
	})

	// 0.c: materialize the closure ConfigMap and patch Spec.ClosureContentRef.
	contentName, err := r.materializeClosure(ctx, candidate, lockSet.Core, closure)
	if err != nil {
		logger.Error("failed to materialize closure ConfigMap", "error", err)
		return pluginresolve.Closure{}, false, err
	}
	candidate.Spec.ClosureContentRef = contentName
	if err := crdstore.Update(ctx, r.store, candidate); err != nil {
		logger.Error("failed to patch ClosureContentRef", "error", err)
		return pluginresolve.Closure{}, false, err
	}

	return closure, false, nil
}

// lockSetToClosure reconstructs a pluginresolve.Closure from a stored
// candidateLockSet. AssertBootstrapClosure and AssertCoreFloor only ever read
// ArtifactID/Version off Closure.Plugins, so this lossy round-trip (no
// SHA256/RequiredCore) is sufficient for re-verification.
func lockSetToClosure(ls candidateLockSet) pluginresolve.Closure {
	pins := make([]pluginresolve.PluginPin, 0, len(ls.Plugins))
	for _, p := range ls.Plugins {
		pins = append(pins, pluginresolve.PluginPin{ArtifactID: p.ArtifactID, Version: p.Version})
	}
	return pluginresolve.Closure{Plugins: pins}
}

func (r *ProfileCandidateReconciler) readLockSetConfigMap(ctx context.Context, name string) (candidateLockSet, error) {
	data, err := r.client.GetConfigMap(ctx, name, r.operatorNamespace)
	if err != nil {
		return candidateLockSet{}, fmt.Errorf("get configmap %s: %w", name, err)
	}
	raw, ok := data["plugins.yaml"]
	if !ok || raw == "" {
		return candidateLockSet{}, fmt.Errorf("configmap %s has no plugins.yaml key", name)
	}
	var ls candidateLockSet
	if err := yaml.Unmarshal([]byte(raw), &ls); err != nil {
		return candidateLockSet{}, fmt.Errorf("parse plugins.yaml: %w", err)
	}
	return ls, nil
}

// readProfileLockSet reads the profile's own materialized "<profile>-pluginset-content"
// ConfigMap (versionprofile_controller.go's materializePluginSet output),
// never the Helm-owned "<profile>-pluginset" source.
func (r *ProfileCandidateReconciler) readProfileLockSet(ctx context.Context, profile *v1alpha1.JenkinsVersionProfile) (candidateLockSet, error) {
	if profile.Status.ContentRef == "" {
		return candidateLockSet{}, fmt.Errorf("profile %s has no materialized plugin set (status.contentRef empty)", profile.Name)
	}
	return r.readLockSetConfigMap(ctx, profile.Status.ContentRef)
}

// materializeClosure writes the "<candidate>-closure" ConfigMap, carrying both
// the unchanged seed Core list and the resolved Plugins, projected down to
// pluginlock.PluginEntry (dropping SHA256/RequiredCore).
func (r *ProfileCandidateReconciler) materializeClosure(ctx context.Context, candidate *v1alpha1.ProfileCandidate, core []string, closure pluginresolve.Closure) (string, error) {
	canonical, err := yaml.Marshal(candidateLockSet{Core: core, Plugins: projectClosureToPluginEntries(closure)})
	if err != nil {
		return "", fmt.Errorf("marshal closure: %w", err)
	}
	contentName := candidate.Name + "-closure"
	owner := profileCandidateOwnerRef(candidate)
	if err := r.client.CreateOrUpdateConfigMapWithOwner(ctx, contentName, r.operatorNamespace, map[string]string{
		"plugins.yaml": string(canonical),
	}, owner); err != nil {
		return "", fmt.Errorf("write configmap %s: %w", contentName, err)
	}
	return contentName, nil
}

func projectClosureToPluginEntries(c pluginresolve.Closure) []pluginlock.PluginEntry {
	out := make([]pluginlock.PluginEntry, 0, len(c.Plugins))
	for _, p := range c.Plugins {
		out = append(out, pluginlock.PluginEntry{ArtifactID: p.ArtifactID, Version: p.Version})
	}
	return out
}

// profileCandidateOwnerRef returns an OwnerReference for a ProfileCandidate,
// mirroring jenkinsVersionProfileOwnerRef's pattern.
func profileCandidateOwnerRef(c *v1alpha1.ProfileCandidate) metav1.OwnerReference {
	controllerFlag := true
	apiVersion := c.APIVersion
	kind := c.Kind
	if apiVersion == "" {
		apiVersion = v1alpha1.SchemeGroupVersion.String()
	}
	if kind == "" {
		kind = "ProfileCandidate"
	}
	return metav1.OwnerReference{
		APIVersion:         apiVersion,
		Kind:               kind,
		Name:               c.Name,
		UID:                c.UID,
		Controller:         &controllerFlag,
		BlockOwnerDeletion: &controllerFlag,
	}
}

// selectSource chooses the MetadataSource and Fetcher based on the
// in-cluster UpdateCenter's storage mode.
func (r *ProfileCandidateReconciler) selectSource(ctx context.Context, target string, logger *slog.Logger) (pluginresolve.MetadataSource, pluginresolve.Fetcher, error) {
	uc, err := crdstore.Get[v1alpha1.UpdateCenter](ctx, r.store, updateCenterSingletonName, "")
	if err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Warn("failed to fetch UpdateCenter; falling back to online source", "error", err)
		}
		return r.onlineSource(target, logger), pluginresolve.HTTPFetcher(r.upstreamBaseURL), nil
	}

	switch uc.Spec.Storage.Type {
	case "oci":
		store, err := r.buildRegistryStore(ctx, uc)
		if err != nil {
			logger.Warn("failed to build in-cluster registry store; treating metadata as unavailable", "error", err)
			return nil, nil, pluginresolve.ErrMetadataUnavailable
		}
		return pluginresolve.InClusterSource{Store: store}, r.inClusterHPIFetcher(store), nil
	case "local":
		// Accepted limitation: the operator process has no mount into the
		// local-PVC-backed store (only the varroa-updatecenter pod does).
		return nil, nil, pluginresolve.ErrMetadataUnavailable
	default:
		return r.onlineSource(target, logger), pluginresolve.HTTPFetcher(r.upstreamBaseURL), nil
	}
}

func (r *ProfileCandidateReconciler) buildRegistryStore(ctx context.Context, uc *v1alpha1.UpdateCenter) (oci.BlobStore, error) {
	if uc.Spec.Storage.OCI == nil {
		return nil, fmt.Errorf("update center storage type is oci but spec.storage.oci is nil")
	}
	cfg := uc.Spec.Storage.OCI
	opts := oci.RegistryOptions{Insecure: cfg.Insecure}
	if cfg.ExistingSecret != "" {
		secretData, err := r.client.GetSecret(ctx, cfg.ExistingSecret, r.operatorNamespace)
		if err != nil {
			return nil, fmt.Errorf("get OCI credentials secret %s: %w", cfg.ExistingSecret, err)
		}
		configPath, cleanup, err := writeTempDockerConfig(secretData)
		if err != nil {
			return nil, fmt.Errorf("write temp docker config: %w", err)
		}
		defer cleanup()
		opts.CredentialConfigPath = configPath
	}
	return r.newRegistryStore(cfg.Ref, opts)
}

// onlineSource builds a *ucmeta.Resolver against updates.jenkins.io, mirroring
// cmd/varroactl/export.go's newExportResolver two-source ("weekly" then
// per-target dynamic-stable) pattern.
func (r *ProfileCandidateReconciler) onlineSource(target string, logger *slog.Logger) pluginresolve.MetadataSource {
	sources := []ucmeta.Source{
		{URL: r.upstreamBaseURL + "/update-center.actual.json"},
		{URL: fmt.Sprintf("%s/dynamic-stable-%s/update-center.actual.json", r.upstreamBaseURL, target)},
	}
	resolver := ucmeta.NewResolver(func() []ucmeta.Source { return sources }, time.Hour, &http.Client{Timeout: candidateHTTPTimeout}, logger)
	return pluginresolve.UpstreamSource{Resolver: resolver}
}

// inClusterHPIFetcher returns a pluginresolve.Fetcher that walks the
// in-cluster plugin-pack store's manifests, exactly as
// pluginresolve.InClusterSource.Resolve does, to find the .hpi bytes for an
// exact name/version. ResolvedPlugin.SHA256 is the pre-verified content
// digest of its own HPI layer, so it is passed directly to FetchBlob.
func (r *ProfileCandidateReconciler) inClusterHPIFetcher(store oci.BlobStore) pluginresolve.Fetcher {
	return func(ctx context.Context, name, version string) ([]byte, error) {
		descs, err := store.ListManifests(ctx)
		if err != nil {
			return nil, fmt.Errorf("list manifests: %w", err)
		}

		var bestSHA256, bestDigest string
		for _, d := range descs {
			ref := d.Annotations["org.opencontainers.image.ref.name"]
			if ref == "" {
				ref = d.Digest
			}
			manifest, err := store.Pull(ctx, ref)
			if err != nil {
				continue
			}
			if manifest.ArtifactType != oci.ArtifactTypePluginPack {
				continue
			}
			_, plugins, err := oci.ReadPluginPack(ctx, store, ref)
			if err != nil {
				continue
			}
			for _, p := range plugins {
				if p.Name != name || p.Version != version {
					continue
				}
				if bestSHA256 == "" || d.Digest < bestDigest {
					bestSHA256, bestDigest = p.SHA256, d.Digest
				}
			}
		}
		if bestSHA256 == "" {
			return nil, fmt.Errorf("plugin %s@%s not found in in-cluster store", name, version)
		}
		rc, err := store.FetchBlob(ctx, bestSHA256)
		if err != nil {
			return nil, fmt.Errorf("fetch blob %s: %w", bestSHA256, err)
		}
		defer rc.Close() //nolint:errcheck
		return io.ReadAll(rc)
	}
}

// verifyClosure runs AssertBootstrapClosure and AssertCoreFloor, verbatim,
// against the operator's own root HPI. It returns true when the candidate
// must go to Phase Failed.
func (r *ProfileCandidateReconciler) verifyClosure(ctx context.Context, candidate *v1alpha1.ProfileCandidate, closure pluginresolve.Closure, fetch pluginresolve.Fetcher, status *v1alpha1.ProfileCandidateStatus, logger *slog.Logger) bool {
	rootHPI, err := r.readRootHPI()
	if err != nil {
		logger.Error("failed to read root HPI", "path", rootHPIPath, "error", err)
		r.setCondition(status, v1alpha1.ProfileCandidateCondition{
			Type:               v1alpha1.ConditionCandidateClosureClean,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "RootHPIUnavailable",
			Message:            fmt.Sprintf("reading %s: %v", rootHPIPath, err),
		})
		r.setCondition(status, v1alpha1.ProfileCandidateCondition{
			Type:               v1alpha1.ConditionCandidateCoreCompatOK,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "RootHPIUnavailable",
			Message:            fmt.Sprintf("reading %s: %v", rootHPIPath, err),
		})
		return true
	}

	failed := false

	if err := pluginresolve.AssertBootstrapClosure(ctx, rootHPI, closure, fetch); err != nil {
		r.setCondition(status, v1alpha1.ProfileCandidateCondition{
			Type:               v1alpha1.ConditionCandidateClosureClean,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "ClosureUnclean",
			Message:            err.Error(),
		})
		failed = true
	} else {
		r.setCondition(status, v1alpha1.ProfileCandidateCondition{
			Type:               v1alpha1.ConditionCandidateClosureClean,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "ClosureClean",
			Message:            "bootstrap closure verified clean",
		})
	}

	if err := pluginresolve.AssertCoreFloor(candidate.Spec.ResolveVersion, rootHPI); err != nil {
		r.setCondition(status, v1alpha1.ProfileCandidateCondition{
			Type:               v1alpha1.ConditionCandidateCoreCompatOK,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "CoreFloorExceeded",
			Message:            err.Error(),
		})
		failed = true
	} else {
		r.setCondition(status, v1alpha1.ProfileCandidateCondition{
			Type:               v1alpha1.ConditionCandidateCoreCompatOK,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "CoreFloorOK",
			Message:            "root HPI's RequiredCore floor satisfied",
		})
	}

	return failed
}

// checkPluginsServable checks whether the closure's plugins are all servable
// from the in-cluster update center. The second return value is false only
// when the inventory fetch itself failed/timed out/was unparseable — in that
// case the caller must leave the condition at its prior value and requeue,
// never treat it as a hard False.
func (r *ProfileCandidateReconciler) checkPluginsServable(ctx context.Context, closure pluginresolve.Closure, status *v1alpha1.ProfileCandidateStatus, logger *slog.Logger) (bool, bool) {
	uc, err := crdstore.Get[v1alpha1.UpdateCenter](ctx, r.store, updateCenterSingletonName, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.setCondition(status, v1alpha1.ProfileCandidateCondition{
				Type:               v1alpha1.ConditionCandidatePluginsServable,
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
				Reason:             "NoUpdateCenter",
				Message:            "no in-cluster update center configured",
			})
			return true, true
		}
		logger.Warn("failed to fetch UpdateCenter for plugins-servable check", "error", err)
		return false, false
	}

	inventory, err := r.fetchInventory(ctx)
	if err != nil {
		logger.Warn("failed to fetch update center inventory", "error", err)
		return false, false
	}

	served := make(map[string]struct{}, len(inventory))
	for _, e := range inventory {
		served[e.Name+"@"+e.Version] = struct{}{}
	}

	var missing []string
	for _, p := range closure.Plugins {
		if _, ok := served[p.ArtifactID+"@"+p.Version]; !ok {
			missing = append(missing, p.ArtifactID+"@"+p.Version)
		}
	}

	if len(missing) == 0 {
		r.setCondition(status, v1alpha1.ProfileCandidateCondition{
			Type:               v1alpha1.ConditionCandidatePluginsServable,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "AllPluginsServable",
			Message:            "all resolved plugins are servable from the in-cluster update center",
		})
		return true, true
	}

	sort.Strings(missing)
	const maxGaps = 10
	shown := missing
	suffix := ""
	if len(shown) > maxGaps {
		shown = shown[:maxGaps]
		suffix = fmt.Sprintf(" and %d more", len(missing)-maxGaps)
	}
	gapMsg := fmt.Sprintf("%d plugin(s) not yet servable from the in-cluster update center: %s%s", len(missing), strings.Join(shown, ", "), suffix)

	if !uc.Spec.PullThrough.Enabled {
		r.setCondition(status, v1alpha1.ProfileCandidateCondition{
			Type:               v1alpha1.ConditionCandidatePluginsServable,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "WaitingForUpdateCenter",
			Message:            gapMsg,
		})
		return false, true
	}

	r.setCondition(status, v1alpha1.ProfileCandidateCondition{
		Type:               v1alpha1.ConditionCandidatePluginsServable,
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             "PullThroughPending",
		Message:            gapMsg,
	})
	return false, true
}

// fetchInventory GETs the update center's /api/v1/inventory, mirroring
// CatalogReconciler.fetchUCInventory's own independent implementation.
func (r *ProfileCandidateReconciler) fetchInventory(ctx context.Context) ([]inventoryEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.ucBaseURL+"/api/v1/inventory", nil)
	if err != nil {
		return nil, fmt.Errorf("build inventory request: %w", err)
	}
	resp, err := r.httpDoer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch inventory: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("inventory request returned %d: %s", resp.StatusCode, string(body))
	}
	var payload ucInventoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode inventory JSON: %w", err)
	}
	return payload.Plugins, nil
}

// checkPreflight runs an advisory, never-blocking fleet check against every
// Controller currently resolving to the profile's line.
func (r *ProfileCandidateReconciler) checkPreflight(ctx context.Context, candidate *v1alpha1.ProfileCandidate, profile *v1alpha1.JenkinsVersionProfile, closure pluginresolve.Closure, status *v1alpha1.ProfileCandidateStatus, logger *slog.Logger) {
	priorFailing := 0
	if status.Preflight != nil {
		priorFailing = status.Preflight.ControllersFailing
	}

	profiles, err := crdstore.List[v1alpha1.JenkinsVersionProfile](ctx, r.store, "", "")
	if err != nil {
		logger.Warn("failed to list JenkinsVersionProfiles for pre-flight", "error", err)
		r.setCondition(status, v1alpha1.ProfileCandidateCondition{
			Type:               v1alpha1.ConditionCandidatePreflightChecked,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "PreflightRan",
			Message:            "the check ran (profile list unavailable)",
		})
		return
	}

	controllers, err := crdstore.List[v1alpha1.Controller](ctx, r.store, "", "")
	if err != nil {
		logger.Warn("failed to list Controllers for pre-flight", "error", err)
		r.setCondition(status, v1alpha1.ProfileCandidateCondition{
			Type:               v1alpha1.ConditionCandidatePreflightChecked,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "PreflightRan",
			Message:            "the check ran (controller list unavailable)",
		})
		return
	}

	set := projectClosureToPluginEntries(closure)

	summary := &v1alpha1.ProfileCandidatePreflightSummary{}
	for _, cr := range controllers {
		resolved, _ := ResolveProfile(cr.Spec.Version, profiles)
		if resolved == nil || resolved.Name != profile.Name {
			continue
		}
		summary.ControllersChecked++

		pluginsYAML, ok := r.rawPluginsYAML(ctx, cr, logger)
		if !ok {
			continue
		}
		report, err := bundle.CheckPluginPins(pluginsYAML, set)
		if err != nil {
			logger.Warn("failed to check plugin pins during pre-flight", "controller", cr.Name, "namespace", cr.Namespace, "error", err)
			continue
		}
		if len(report.Conflicts) == 0 {
			continue
		}

		summary.ControllersFailing++
		message := pluginPinConflictMessage(report)
		if len(message) > maxFailingControllerMessageLen {
			message = message[:maxFailingControllerMessageLen]
		}
		if len(summary.FailingControllers) < maxPreflightFailures {
			summary.FailingControllers = append(summary.FailingControllers, v1alpha1.ProfileCandidateFailingController{
				Name:          cr.Name,
				Namespace:     cr.Namespace,
				Message:       message,
				ConflictCount: len(report.Conflicts),
			})
		}

		r.publish(activity.Event{
			Type:       "pluginPinConflict.detected",
			Source:     "profile-candidate",
			Controller: cr.Name,
			Namespace:  cr.Namespace,
			Message:    message,
			Reason:     v1alpha1.ReasonPluginPinConflict,
			Severity:   "warning",
		})
	}

	status.Preflight = summary

	r.setCondition(status, v1alpha1.ProfileCandidateCondition{
		Type:               v1alpha1.ConditionCandidatePreflightChecked,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "PreflightRan",
		Message:            fmt.Sprintf("checked %d controller(s), %d failing", summary.ControllersChecked, summary.ControllersFailing),
	})

	if priorFailing == 0 && summary.ControllersFailing > 0 {
		r.publish(activity.Event{
			Type:     "upgrade.preflight.failed",
			Source:   "operator",
			Message:  fmt.Sprintf("candidate %s: %d controller(s) now failing pre-flight pin checks", candidate.Name, summary.ControllersFailing),
			Reason:   "PreflightFailing",
			Severity: "warning",
		})
	}
}

// rawPluginsYAML reads a Controller's effective ComposedBundle's raw,
// unresolved plugins.yaml content directly off its content ConfigMap — the
// same source resolveBundleForController's RawPluginsYAML field is built
// from, without that function's readiness gating/var injection/completeness
// machinery, which this advisory, best-effort fleet check does not need.
func (r *ProfileCandidateReconciler) rawPluginsYAML(ctx context.Context, cr *v1alpha1.Controller, logger *slog.Logger) (string, bool) {
	name, namespace := v1alpha1.EffectiveBundleRef(cr, r.operatorNamespace)
	if name == "" {
		return "", false
	}
	cb, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, r.store, name, namespace)
	if err != nil {
		logger.Debug("pre-flight: composed bundle unavailable", "controller", cr.Name, "bundle", name, "error", err)
		return "", false
	}
	if cb.Status.ContentRef == "" {
		return "", false
	}
	data, err := r.client.GetConfigMap(ctx, cb.Status.ContentRef, cb.Namespace)
	if err != nil {
		logger.Debug("pre-flight: composed bundle content unavailable", "controller", cr.Name, "configMap", cb.Status.ContentRef, "error", err)
		return "", false
	}
	raw, ok := data["plugins.yaml"]
	if !ok || raw == "" {
		return "", false
	}
	return raw, true
}

func (r *ProfileCandidateReconciler) publish(e activity.Event) {
	if r.activityPublisher == nil {
		return
	}
	r.activityPublisher.Publish(e)
}
