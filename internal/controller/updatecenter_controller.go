package controller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/jenkinsver"
	"github.com/varroaci/varroa-jenkins/internal/oci"
	"github.com/varroaci/varroa-jenkins/internal/pluginver"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

const (
	// updateCenterSingletonName is the fixed name of the singleton UpdateCenter CR.
	updateCenterSingletonName = "varroa-update-center"

	// updateCenterPVCSuffix is appended to the release name for the local-storage PVC.
	// Matches the chart template pattern: {{ include "varroa.fullname" (dict "Release" .Release "name" "updatecenter") }}
	// which resolves to "<release>-updatecenter".
	updateCenterPVCName = "varroa-updatecenter"

	// updateCenterImportTokenSecret is the Secret holding the import Bearer token.
	updateCenterImportTokenSecret = "varroa-updatecenter-import-token"
	// updateCenterImportTokenKey is the data key in the import token Secret.
	updateCenterImportTokenKey = "token"

	// Condition types (exact per spec).
	condTypeStorageReady     = "StorageReady"
	condTypeSeedImported     = "SeedImported"
	condTypeCoverageComplete = "CoverageComplete"
	condTypePluginsServable  = "PluginsServable"
	condTypeReady            = "Ready"

	// Reason strings (exact per spec).
	reasonStorageUnavailable   = "StorageUnavailable"
	reasonInventoryUnavailable = "InventoryUnavailable"
	reasonGapAnalysisComplete  = "GapAnalysisComplete"
	reasonAllPluginsServable   = "AllPluginsServable"
	reasonPluginsUnservable    = "PluginsUnservable"
	reasonResolutionDegraded   = "ResolutionDegraded"

	// Per-plugin unservable reasons. They match the update center's servability
	// reason codes, mapped to CRD-facing names.
	unservableNotListed           = "NotListed"
	unservableArchiveDisabled     = "ArchiveDisabled"
	unservablePullThroughDisabled = "PullThroughDisabled"

	// Max gaps and max unservable entries to surface.
	maxGaps = 50
)

// UpdateCenterReconciler reconciles the singleton UpdateCenter CRD.
// It checks storage readiness, imports seed refs, computes coverage gaps,
// and derives a lifecycle phase.
type UpdateCenterReconciler struct {
	client            ResourceClient
	store             crdstore.Backend
	operatorNamespace string
	ucBaseURL         string // VARROA_UPDATE_CENTER_URL
	logger            *slog.Logger

	// newRegistryStore creates a BlobStore for the given OCI reference.
	// Exported only for test injection; default is oci.NewRegistryStore.
	newRegistryStore func(ref string, opts oci.RegistryOptions) (oci.BlobStore, error)

	// httpDoer performs HTTP requests. Default is http.DefaultClient.
	httpDoer interface {
		Do(*http.Request) (*http.Response, error)
	}
}

// NewUpdateCenterReconciler creates a new UpdateCenterReconciler.
func NewUpdateCenterReconciler(client ResourceClient, store crdstore.Backend, operatorNamespace, ucBaseURL string, logger *slog.Logger) *UpdateCenterReconciler {
	return &UpdateCenterReconciler{
		client:            client,
		store:             store,
		operatorNamespace: operatorNamespace,
		ucBaseURL:         strings.TrimRight(ucBaseURL, "/"),
		logger:            logger,
		newRegistryStore: func(ref string, opts oci.RegistryOptions) (oci.BlobStore, error) {
			return oci.NewRegistryStore(ref, opts)
		},
		httpDoer: http.DefaultClient,
	}
}

// Reconcile implements the reconcile.Reconciler interface.
func (r *UpdateCenterReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := r.logger.With("updateCenter", req.Name)

	// §3.1 — singleton guard
	if req.Name != updateCenterSingletonName {
		logger.Debug("skipping non-singleton UpdateCenter", "name", req.Name)
		return reconcile.Result{}, nil
	}

	uc, err := crdstore.Get[v1alpha1.UpdateCenter](ctx, r.store, req.Name, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Debug("UpdateCenter not found, nothing to reconcile")
			return reconcile.Result{}, nil
		}
		logger.Error("failed to get UpdateCenter", "error", err)
		return reconcile.Result{}, err
	}

	// Build initial status from current state.
	status := &v1alpha1.UpdateCenterStatus{}
	uc.Status.DeepCopyInto(status)
	if status.Conditions == nil {
		status.Conditions = []v1alpha1.UpdateCenterCondition{}
	}

	// The reserved CatalogSource exists for as long as the singleton does. It
	// is asserted before the storage gate: derivation is a separate concern
	// from whether the store is reachable, and a source that disappears every
	// time storage blips would take its derived items with it.
	r.reconcileReservedCatalogSource(ctx, uc, logger)

	// §3.2 — storage readiness
	storageReady := r.checkStorageReady(ctx, uc, logger)
	if !storageReady {
		// Set all four conditions False with reason StorageUnavailable, phase=Error,
		// then patch and return early.
		r.setAllConditionsFalse(status, reasonStorageUnavailable, "storage is unavailable")
		status.Phase = v1alpha1.UpdateCenterPhase("Error")
		status.LastSyncTime = metav1.Now()
		return reconcile.Result{}, crdstore.PatchStatus[v1alpha1.UpdateCenter](ctx, r.store, uc.Name, "", status)
	}

	// Storage is ready.
	r.setCondition(status, v1alpha1.UpdateCenterCondition{
		Type:    condTypeStorageReady,
		Status:  metav1.ConditionTrue,
		Reason:  "StorageAvailable",
		Message: "storage is reachable",
	})

	// §3.3 — seed import (only when storage is ready)
	r.reconcileSeedImport(ctx, uc, status, logger)

	// §3.4 — gap computation
	r.computeGaps(ctx, status, logger)

	// LTS-line metadata sources for pull-through resolution (add-lts-checksum-resolution).
	// Runs only after the storage-readiness gate above.
	r.reconcileMetadataSources(ctx, uc, status, logger)

	// §3.5 — phase derivation + patch
	r.derivePhase(status)
	status.LastSyncTime = metav1.Now()

	if err := crdstore.PatchStatus[v1alpha1.UpdateCenter](ctx, r.store, uc.Name, "", status); err != nil {
		logger.Error("failed to patch status", "error", err)
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// ---------------------------------------------------------------------------
// §3.2 — storage readiness
// ---------------------------------------------------------------------------

// checkStorageReady returns true if the configured storage backend is reachable.
// On failure it sets StorageReady=False (and the other three conditions are set
// by the caller).
func (r *UpdateCenterReconciler) checkStorageReady(ctx context.Context, uc *v1alpha1.UpdateCenter, logger *slog.Logger) bool {
	switch uc.Spec.Storage.Type {
	case "local":
		return r.checkLocalStorage(ctx, logger)
	case "oci":
		return r.checkOCIStorage(ctx, uc, logger)
	default:
		logger.Error("unknown storage type", "type", uc.Spec.Storage.Type)
		return false
	}
}

// checkLocalStorage verifies the local PVC exists and is Bound.
func (r *UpdateCenterReconciler) checkLocalStorage(ctx context.Context, logger *slog.Logger) bool {
	pvc, err := r.client.GetPVC(ctx, r.operatorNamespace, updateCenterPVCName)
	if err != nil {
		logger.Warn("PVC not found or unreachable", "namespace", r.operatorNamespace, "pvc", updateCenterPVCName, "error", err)
		return false
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		logger.Warn("PVC not bound", "namespace", r.operatorNamespace, "pvc", updateCenterPVCName, "phase", pvc.Status.Phase)
		return false
	}
	return true
}

// checkOCIStorage verifies the OCI registry is reachable by performing a
// resolve or list against it using the configured credentials.
func (r *UpdateCenterReconciler) checkOCIStorage(ctx context.Context, uc *v1alpha1.UpdateCenter, logger *slog.Logger) bool {
	if uc.Spec.Storage.OCI == nil {
		logger.Error("OCI storage config is nil")
		return false
	}

	cfg := uc.Spec.Storage.OCI
	opts := oci.RegistryOptions{Insecure: cfg.Insecure}

	// Resolve credentials from existingSecret if configured.
	if cfg.ExistingSecret != "" {
		secretData, err := r.client.GetSecret(ctx, cfg.ExistingSecret, r.operatorNamespace)
		if err != nil {
			logger.Warn("failed to get OCI credentials secret", "secret", cfg.ExistingSecret, "error", err)
			return false
		}
		configPath, cleanup, err := writeTempDockerConfig(secretData)
		if err != nil {
			logger.Warn("failed to write temp docker config", "error", err)
			return false
		}
		defer cleanup()
		opts.CredentialConfigPath = configPath
	}

	store, err := r.newRegistryStore(cfg.Ref, opts)
	if err != nil {
		logger.Warn("failed to create registry store", "ref", cfg.Ref, "error", err)
		return false
	}

	// Prove reachability: a simple Resolve or ListManifests call.
	descs, err := store.ListManifests(ctx)
	if err != nil {
		logger.Warn("registry unreachable", "ref", cfg.Ref, "error", err)
		return false
	}
	logger.Debug("OCI storage reachable", "ref", cfg.Ref, "manifests", len(descs))
	return true
}

// ---------------------------------------------------------------------------
// §3.3 — seed import
// ---------------------------------------------------------------------------

// reconcileSeedImport iterates spec.seed.refs, resolves each to a digest,
// skips already-imported digests, pulls + tars + POSTs new ones, and updates
// status.seedImportedDigests.
func (r *UpdateCenterReconciler) reconcileSeedImport(ctx context.Context, uc *v1alpha1.UpdateCenter, status *v1alpha1.UpdateCenterStatus, logger *slog.Logger) {
	refs := uc.Spec.Seed.Refs

	// Vacuous case: empty refs → SeedImported=True.
	if len(refs) == 0 {
		r.setCondition(status, v1alpha1.UpdateCenterCondition{
			Type:    condTypeSeedImported,
			Status:  metav1.ConditionTrue,
			Reason:  "NoSeedRefs",
			Message: "no seed refs configured",
		})
		status.SeedImportedDigests = nil
		return
	}

	// Build lookup of already-imported digests.
	importedSet := make(map[string]bool, len(status.SeedImportedDigests))
	for _, d := range status.SeedImportedDigests {
		importedSet[d] = true
	}

	// Read the import token once.
	token, err := r.getImportToken(ctx)
	if err != nil {
		logger.Warn("failed to get import token", "error", err)
		r.setCondition(status, v1alpha1.UpdateCenterCondition{
			Type:    condTypeSeedImported,
			Status:  metav1.ConditionFalse,
			Reason:  "TokenUnavailable",
			Message: fmt.Sprintf("import token not available: %v", err),
		})
		return
	}

	// Resolve pull credentials once, after the vacuous-seed fast path: an
	// unused secretRef must never turn an empty seed list into a failure, nor
	// prevent status.SeedImportedDigests from being cleared. A secret that is
	// missing, unreadable, or malformed fails the whole seed rather than
	// falling back to anonymous pulls, which would surface as a confusing 403
	// on every ref.
	var credentialConfigPath string
	if uc.Spec.Seed.SecretRef != "" {
		data, err := r.client.GetSecret(ctx, uc.Spec.Seed.SecretRef, r.operatorNamespace)
		if err == nil {
			var auth *bundle.OCIAuth
			auth, err = bundle.OCIAuthFromSecret(data)
			if err == nil {
				credentialConfigPath, err = bundle.WriteDockerConfigJSON(auth)
			}
		}
		if err != nil {
			logger.Warn("failed to resolve seed credentials", "secretRef", uc.Spec.Seed.SecretRef, "error", err)
			r.setCondition(status, v1alpha1.UpdateCenterCondition{
				Type:    condTypeSeedImported,
				Status:  metav1.ConditionFalse,
				Reason:  "ImportFailed",
				Message: fmt.Sprintf("seed credentials %s: %v", uc.Spec.Seed.SecretRef, err),
			})
			return
		}
		// Registered inside the branch but run on return, so the config file
		// outlives the whole ref loop below. The emptiness guard is not
		// decorative: WriteDockerConfigJSON's contract returns ("", nil) for a
		// nil auth, and filepath.Dir("") is ".", so an unguarded RemoveAll would
		// delete the operator's working directory. OCIAuthFromSecret cannot
		// return (nil, nil) today, which is exactly why this must not depend on
		// that staying true.
		if credentialConfigPath != "" {
			defer func() { _ = os.RemoveAll(filepath.Dir(credentialConfigPath)) }()
		}
	}

	var failures []string
	newDigests := make([]string, 0, len(refs))

	for _, ref := range refs {
		refLogger := logger.With("ref", ref)

		// §3.3a — resolve-only: get the current content digest.
		digest, err := r.resolveSeedRef(ctx, ref, credentialConfigPath)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: resolve: %v", ref, err))
			refLogger.Warn("failed to resolve seed ref", "error", err)
			continue
		}

		// Skip if already imported (checked against the freshly-resolved digest).
		// Re-record it so a ref that is still declared keeps its tracking entry;
		// only a ref dropped from spec.seed.refs falls out of the set.
		if importedSet[digest] {
			newDigests = append(newDigests, digest)
			refLogger.Debug("seed ref already imported, skipping", "digest", digest)
			continue
		}

		// §3.3b — fetch + POST: pull, tar, POST to import endpoint.
		if err := r.importSeedRef(ctx, ref, token, credentialConfigPath); err != nil {
			failures = append(failures, fmt.Sprintf("%s: import: %v", ref, err))
			refLogger.Warn("failed to import seed ref", "error", err)
			continue
		}

		// Record the digest only once the pack actually landed in the store.
		// Recording it before the POST would make the NEXT reconcile treat a
		// failed import as already-imported: it would skip the retry, report no
		// failures, and settle on SeedImported=True while the plugins were never
		// delivered. Registry auth failures surface here, so this path is live.
		newDigests = append(newDigests, digest)
		refLogger.Info("seed ref imported", "digest", digest)
	}

	// Update status with the full current set of successfully imported digests.
	status.SeedImportedDigests = newDigests

	if len(failures) > 0 {
		r.setCondition(status, v1alpha1.UpdateCenterCondition{
			Type:    condTypeSeedImported,
			Status:  metav1.ConditionFalse,
			Reason:  "ImportFailed",
			Message: strings.Join(failures, "; "),
		})
	} else {
		r.setCondition(status, v1alpha1.UpdateCenterCondition{
			Type:    condTypeSeedImported,
			Status:  metav1.ConditionTrue,
			Reason:  "AllImported",
			Message: fmt.Sprintf("%d seed refs imported", len(refs)),
		})
	}
}

// resolveSeedRef resolves a seed OCI ref to its current content digest.
// §3.3a: resolve-only, no pull.
func (r *UpdateCenterReconciler) resolveSeedRef(ctx context.Context, ref, credentialConfigPath string) (string, error) {
	store, err := r.newRegistryStore(ref, oci.RegistryOptions{CredentialConfigPath: credentialConfigPath})
	if err != nil {
		return "", fmt.Errorf("create registry store for %q: %w", ref, err)
	}
	desc, err := store.Resolve(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", ref, err)
	}
	return desc.Digest, nil
}

// importSeedRef pulls a ref into a temp OCI layout, tars it, and POSTs to
// the update-center import endpoint. §3.3b.
func (r *UpdateCenterReconciler) importSeedRef(ctx context.Context, ref, token, credentialConfigPath string) error {
	// 1. Create a RegistryStore for the source ref.
	srcStore, err := r.newRegistryStore(ref, oci.RegistryOptions{CredentialConfigPath: credentialConfigPath})
	if err != nil {
		return fmt.Errorf("create source registry store: %w", err)
	}

	// 2. Create a temp LayoutStore for the copy destination.
	tmpDir, err := os.MkdirTemp("", "varroa-uc-seed-import-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	dstStore, err := oci.NewLayoutStore(tmpDir)
	if err != nil {
		return fmt.Errorf("create layout store: %w", err)
	}

	// 3. Copy from registry to layout using oci.Copy.
	if err := oci.Copy(ctx, srcStore, ref, dstStore, ref); err != nil {
		return fmt.Errorf("copy ref to temp layout: %w", err)
	}

	// 4. Create a tar.gz of the layout directory.
	tarBuf := new(bytes.Buffer)
	if err := tarGzDirectory(tmpDir, tarBuf); err != nil {
		return fmt.Errorf("tar+gz layout: %w", err)
	}

	// 5. POST to the import endpoint.
	importURL := r.ucBaseURL + "/api/v1/import"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, importURL, tarBuf)
	if err != nil {
		return fmt.Errorf("create import request: %w", err)
	}
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := r.httpDoer.Do(req)
	if err != nil {
		return fmt.Errorf("POST import: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("import rejected: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}

// getImportToken reads the import Bearer token from the
// varroa-updatecenter-import-token Secret.
func (r *UpdateCenterReconciler) getImportToken(ctx context.Context) (string, error) {
	data, err := r.client.GetSecret(ctx, updateCenterImportTokenSecret, r.operatorNamespace)
	if err != nil {
		return "", fmt.Errorf("get secret %s: %w", updateCenterImportTokenSecret, err)
	}
	tokenBytes, ok := data[updateCenterImportTokenKey]
	if !ok {
		return "", fmt.Errorf("secret %s has no key %q", updateCenterImportTokenSecret, updateCenterImportTokenKey)
	}
	return string(tokenBytes), nil
}

// ---------------------------------------------------------------------------
// §3.4 — gap computation
// ---------------------------------------------------------------------------

// inventoryEntry is a single plugin entry in the /api/v1/inventory response.
// Everything past sizeBytes is optional: a pack written before T1.1's layer
// annotations existed carries only name/version/sha256/upstreamUrl, and such a
// plugin still derives a catalog item — it just has no metadata to show and no
// declared dependencies to walk.
type inventoryEntry struct {
	Name         string      `json:"name"`
	Version      string      `json:"version"`
	SHA256       string      `json:"sha256"`
	SizeBytes    int64       `json:"sizeBytes"`
	DisplayName  string      `json:"displayName,omitempty"`
	Description  string      `json:"description,omitempty"`
	Tags         []string    `json:"tags,omitempty"`
	RequiredCore string      `json:"requiredCore,omitempty"`
	Dependencies []pluginDep `json:"dependencies,omitempty"`
	// DependenciesKnown is false when the update center cannot read the list.
	DependenciesKnown bool `json:"dependenciesKnown"`
}

// pluginDep is one declared dependency, in the frozen wire shape of
// dev.varroa.plugin.dependencies. Min is a MINIMUM, not a pin, and optional
// dependencies are excluded from every closure.
type pluginDep struct {
	Name     string `json:"name"`
	Min      string `json:"min"`
	Optional bool   `json:"optional"`
}

// declaredPlugin is a plugin declared by a profile or bundle.
type declaredPlugin struct {
	Name       string
	Version    string
	RequiredBy string
	// Core is the lowest core that requests this plugin; empty when no controller requests it.
	Core string
}

// computeGaps fetches the UC inventory, builds the declared set from profiles and
// bundles, and computes the gaps (declared minus stored). It then asks the update
// center which declared plugins it cannot serve at all, from the store or by
// pull-through. CoverageComplete answers the first question; PluginsServable answers
// the second, and only PluginsServable gates Ready.
func (r *UpdateCenterReconciler) computeGaps(ctx context.Context, status *v1alpha1.UpdateCenterStatus, logger *slog.Logger) {
	// §3.4a — fetch inventory.
	inventory, skipped, err := getUCInventory(ctx, r.httpDoer, r.ucBaseURL)
	if err != nil {
		logger.Warn("inventory fetch failed", "error", err)
		r.setInventoryUnavailable(status, fmt.Sprintf("inventory unavailable: %v", err))
		// Leave status.Gaps and status.Unservable unchanged — do NOT overwrite.
		return
	}

	// A non-empty skipped means "inventory" is a lower bound, not a complete
	// listing: a plugin held only by an unreadable pack reads as a gap below
	// even though it is technically still stored. That is disclosed in the
	// condition message rather than treated as a fetch failure — the readable
	// subset is still useful for gap analysis, and failing this pass entirely
	// on one unreadable legacy pack is exactly the all-or-nothing blast radius
	// this endpoint no longer has.
	var skippedNote string
	if len(skipped) > 0 {
		refs := make([]string, len(skipped))
		for i, sp := range skipped {
			refs[i] = sp.Ref
		}
		logger.Warn("update center inventory is partial: some plugin packs could not be read",
			"unreadableManifests", len(skipped), "refs", refs)
		skippedNote = fmt.Sprintf(" (inventory partial: %d plugin-pack manifest(s) unreadable: %s)",
			len(skipped), strings.Join(refs, ", "))
	}

	// Build inventory lookup set: "name@version" → true.
	invSet := make(map[string]bool, len(inventory))
	for _, e := range inventory {
		invSet[fmt.Sprintf("%s@%s", e.Name, e.Version)] = true
	}
	byName := inventoryByName(inventory)

	// Set PluginCount and StoreBytes from the inventory response.
	status.PluginCount = len(inventory)
	var totalBytes int64
	for _, e := range inventory {
		totalBytes += e.SizeBytes
	}
	status.StoreBytes = totalBytes
	r.applyUnknownDependencies(status, inventory)

	// §3.4b — build declared set from JenkinsVersionProfiles and ComposedBundles.
	declared, err := r.buildDeclaredSet(ctx, logger)
	if err != nil {
		logger.Warn("failed to build declared plugin set", "error", err)
		r.setInventoryUnavailable(status, fmt.Sprintf("declared set unavailable: %v", err))
		return
	}

	// Compute gaps: distinct declared − inventory.
	var unstored []declaredPlugin
	for _, dp := range uniqueDeclared(declared) {
		// Served metadata advertises one stored version per plugin. A latest pin is
		// stored when a stored version runs on the lowest core that requests it.
		if ucmeta.IsLatest(dp.Version) {
			if !storedForCore(byName, dp.Name, dp.Core) {
				unstored = append(unstored, dp)
			}
			continue
		}
		if !invSet[fmt.Sprintf("%s@%s", dp.Name, dp.Version)] {
			unstored = append(unstored, dp)
		}
	}

	status.GapCount = len(unstored)
	status.Gaps = nil
	for i := 0; i < len(unstored) && i < maxGaps; i++ {
		dp := unstored[i]
		status.Gaps = append(status.Gaps, v1alpha1.UpdateCenterGap{
			Plugin:     dp.Name,
			Version:    dp.Version,
			RequiredBy: dp.RequiredBy,
			Core:       coreForLatest(dp),
		})
	}

	coverageComplete := len(unstored) == 0
	msg := "coverage complete"
	if !coverageComplete {
		msg = fmt.Sprintf("%d declared plugins are not in the store", len(unstored))
		if len(unstored) > maxGaps {
			msg += fmt.Sprintf(" (status.gaps shows the first %d)", maxGaps)
		}
	}
	msg += skippedNote

	r.setCondition(status, v1alpha1.UpdateCenterCondition{
		Type:    condTypeCoverageComplete,
		Status:  boolToConditionStatus(coverageComplete),
		Reason:  reasonGapAnalysisComplete,
		Message: msg,
	})

	r.computeServability(ctx, status, unstored, logger)
}

// setInventoryUnavailable records that coverage and servability cannot be computed.
func (r *UpdateCenterReconciler) setInventoryUnavailable(status *v1alpha1.UpdateCenterStatus, msg string) {
	r.setCondition(status, v1alpha1.UpdateCenterCondition{
		Type:    condTypeCoverageComplete,
		Status:  metav1.ConditionFalse,
		Reason:  reasonInventoryUnavailable,
		Message: msg,
	})
	r.setCondition(status, v1alpha1.UpdateCenterCondition{
		Type:    condTypePluginsServable,
		Status:  metav1.ConditionFalse,
		Reason:  reasonInventoryUnavailable,
		Message: msg,
	})
}

// computeServability sets PluginsServable and status.unservable from the declared
// plugins the store does not hold. It asks the update center's servability endpoint,
// which runs the same resolution pull-through uses.
func (r *UpdateCenterReconciler) computeServability(ctx context.Context, status *v1alpha1.UpdateCenterStatus, unstored []declaredPlugin, logger *slog.Logger) {
	if len(unstored) == 0 {
		status.Unservable = nil
		status.UnservableCount = 0
		r.setCondition(status, v1alpha1.UpdateCenterCondition{
			Type:    condTypePluginsServable,
			Status:  metav1.ConditionTrue,
			Reason:  reasonAllPluginsServable,
			Message: "every declared plugin is in the store",
		})
		return
	}

	results, err := r.fetchServability(ctx, unstored)
	if errors.Is(err, errServabilityUnsupported) {
		// The update center answers without the endpoint: a version skew, not an
		// upstream outage. This is an unhealthy UC HTTP API, so it blocks Ready.
		logger.Warn("update center does not serve the servability endpoint", "error", err)
		r.setCondition(status, v1alpha1.UpdateCenterCondition{
			Type:    condTypePluginsServable,
			Status:  metav1.ConditionFalse,
			Reason:  reasonInventoryUnavailable,
			Message: fmt.Sprintf("servability unavailable: %v", err),
		})
		return
	}
	if err != nil {
		logger.Warn("servability check failed", "error", err)
		// Leave status.Unservable and status.UnservableCount unchanged.
		r.setCondition(status, v1alpha1.UpdateCenterCondition{
			Type:    condTypePluginsServable,
			Status:  metav1.ConditionUnknown,
			Reason:  reasonResolutionDegraded,
			Message: fmt.Sprintf("servability could not be checked: %v", err),
		})
		return
	}

	var (
		unservable []v1alpha1.UpdateCenterGap
		undecided  int
	)
	for i, res := range results {
		if res.Servable {
			continue
		}
		reason, isUndecided := classifyServability(res)
		if isUndecided {
			// sources-degraded: the negative is unprovable, so it is not listed.
			undecided++
			continue
		}
		unservable = append(unservable, v1alpha1.UpdateCenterGap{
			Plugin:     unstored[i].Name,
			Version:    unstored[i].Version,
			RequiredBy: unstored[i].RequiredBy,
			Core:       coreForLatest(unstored[i]),
			Reason:     reason,
		})
	}

	status.UnservableCount = len(unservable)
	status.Unservable = nil
	if len(unservable) > maxGaps {
		status.Unservable = unservable[:maxGaps]
	} else if len(unservable) > 0 {
		status.Unservable = unservable
	}

	switch {
	case len(unservable) > 0:
		msg := fmt.Sprintf("%d declared plugins cannot be served", len(unservable))
		if undecided > 0 {
			msg = fmt.Sprintf("%d declared plugins cannot be served (%d undecided: upstream metadata degraded)",
				len(unservable), undecided)
		}
		r.setCondition(status, v1alpha1.UpdateCenterCondition{
			Type:    condTypePluginsServable,
			Status:  metav1.ConditionFalse,
			Reason:  reasonPluginsUnservable,
			Message: msg,
		})
	case undecided > 0:
		r.setCondition(status, v1alpha1.UpdateCenterCondition{
			Type:    condTypePluginsServable,
			Status:  metav1.ConditionUnknown,
			Reason:  reasonResolutionDegraded,
			Message: fmt.Sprintf("%d declared plugins could not be checked: upstream metadata degraded", undecided),
		})
	default:
		r.setCondition(status, v1alpha1.UpdateCenterCondition{
			Type:    condTypePluginsServable,
			Status:  metav1.ConditionTrue,
			Reason:  reasonAllPluginsServable,
			Message: "every declared plugin is servable",
		})
	}
}

// uniqueDeclared returns the distinct name@version entries, keeping the first
// RequiredBy seen for each, sorted by plugin name and then version.
//
// A duplicate latest entry merges its Core with minCore, so the posted core is the
// lowest core any source requests for that plugin.
func uniqueDeclared(declared []declaredPlugin) []declaredPlugin {
	index := make(map[string]int, len(declared))
	out := make([]declaredPlugin, 0, len(declared))
	for _, dp := range declared {
		key := dp.Name + "@" + dp.Version
		if i, dup := index[key]; dup {
			if ucmeta.IsLatest(dp.Version) {
				out[i].Core = minCore(out[i].Core, dp.Core)
			}
			continue
		}
		index[key] = len(out)
		out = append(out, dp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// fetchServability posts the declared plugins to the update center's
// servability endpoint through postServability.
func (r *UpdateCenterReconciler) fetchServability(ctx context.Context, plugins []declaredPlugin) ([]ucServabilityResult, error) {
	req := make([]ucServabilityPlugin, 0, len(plugins))
	for _, dp := range plugins {
		req = append(req, ucServabilityPlugin{Name: dp.Name, Version: dp.Version, Core: coreForLatest(dp)})
	}
	return postServability(ctx, r.httpDoer, r.ucBaseURL, req)
}

// metadataConfigMapName is the operator-managed ConfigMap that carries the derived
// LTS-line metadata source URLs consumed by the update-center server for pull-through.
const metadataConfigMapName = "varroa-updatecenter-metadata"

// metadataConfigMapKey is the newline-delimited list of LTS metadata source URLs.
const metadataConfigMapKey = "lts-metadata-urls"

// declaredPluginsKey is the newline-delimited, sorted "name@version" set of every
// plugin any JenkinsVersionProfile or ComposedBundle declares. The update-center
// service has no Kubernetes client, so this ConfigMap key is how it learns what is
// pinned — which the upload closure planner and the served-metadata precedence rule
// both need. Unlike lts-metadata-urls it is NOT gated on pull-through: it matters
// most in the air-gapped configuration, where pull-through is off.
const declaredPluginsKey = "declared-plugins"

// maxResolvedMetadataSources bounds status.resolvedMetadataSources.
const maxResolvedMetadataSources = 20

// ltsPatchRe matches an exact MAJOR.MINOR.PATCH LTS version, e.g. "2.555.3".
var ltsPatchRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// reconcileMetadataSources derives the LTS-line metadata source URLs from declared
// version profiles, writes them to the operator-managed ConfigMap the update-center
// server mounts, and records the full source list on status. It runs only after the
// storage-readiness gate (co-located with gap computation), so a storage-unready tick
// (which returns early before this) leaves the ConfigMap and status untouched.
func (r *UpdateCenterReconciler) reconcileMetadataSources(ctx context.Context, uc *v1alpha1.UpdateCenter, status *v1alpha1.UpdateCenterStatus, logger *slog.Logger) {
	owner := metav1.OwnerReference{
		APIVersion: v1alpha1.SchemeGroupVersion.String(),
		Kind:       "UpdateCenter",
		Name:       uc.Name,
		UID:        uc.UID,
	}

	// The declared set is computed and compared independently of the LTS source list,
	// and is written on every tick past the storage-readiness gate — including the
	// pull-through-disabled branch below, where only lts-metadata-urls is cleared.
	// A nil value means buildDeclaredSet failed and the existing key is left alone.
	declared := r.buildDeclaredPluginsValue(ctx, logger)

	// Pull-through disabled: no upstream to derive from. Clear status and the LTS key.
	if !uc.Spec.PullThrough.Enabled {
		status.ResolvedMetadataSources = nil
		r.writeMetadataConfigMap(ctx, map[string]*string{
			metadataConfigMapKey: ptrTo(""),
			declaredPluginsKey:   declared,
		}, owner, logger)
		return
	}

	upstream := uc.Spec.PullThrough.UpstreamURL
	if upstream == "" {
		upstream = "https://updates.jenkins.io"
	}
	upstream = strings.TrimRight(upstream, "/")
	weekly := upstream + "/update-center.actual.json"

	// Derive dynamic-stable sources for every LTS-patch profile. On a transient list
	// failure, leave the ConfigMap and status.resolvedMetadataSources untouched (the
	// prior value is carried by the status DeepCopy) rather than dropping valid LTS
	// sources — mirroring computeGaps' leave-unchanged-on-failure invariant.
	ltsURLs, err := r.deriveLTSMetadataURLs(ctx, upstream)
	if err != nil {
		logger.Warn("skipping LTS metadata reconcile this tick; leaving existing sources untouched", "error", err)
		// The declared set is independent: a failure to list profiles for LTS URL
		// derivation must not withhold a declared set that was built successfully.
		r.writeMetadataConfigMap(ctx, map[string]*string{declaredPluginsKey: declared}, owner, logger)
		return
	}

	r.writeMetadataConfigMap(ctx, map[string]*string{
		metadataConfigMapKey: ptrTo(strings.Join(ltsURLs, "\n")),
		declaredPluginsKey:   declared,
	}, owner, logger)
	status.ResolvedMetadataSources = boundSources(append([]string{weekly}, ltsURLs...))
}

// buildDeclaredPluginsValue renders the declared plugin set as the sorted,
// newline-delimited "name@version" body of the declared-plugins key. Every entry
// carries a version: buildDeclaredSet writes a version-less entry as "latest".
// It returns nil
// when the set could not be built, which the writer reads as "leave the key alone" —
// the same leave-unchanged-on-failure invariant computeGaps applies to status.Gaps.
//
// Identical name@version entries are collapsed: buildDeclaredSet does not
// deduplicate (the same plugin is routinely declared by several bundles), and
// repeating a line carries no information the consumer can use while making the
// ConfigMap churn on unrelated bundle edits.
func (r *UpdateCenterReconciler) buildDeclaredPluginsValue(ctx context.Context, logger *slog.Logger) *string {
	declared, err := r.buildDeclaredSet(ctx, logger)
	if err != nil {
		logger.Warn("skipping declared-plugins reconcile this tick; leaving the existing key untouched", "error", err)
		return nil
	}
	seen := make(map[string]struct{}, len(declared))
	lines := make([]string, 0, len(declared))
	for _, dp := range declared {
		if dp.Name == "" {
			continue
		}
		line := dp.Name + "@" + dp.Version
		if _, dup := seen[line]; dup {
			continue
		}
		seen[line] = struct{}{}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return ptrTo(strings.Join(lines, "\n"))
}

func ptrTo[T any](v T) *T { return &v }

// deriveLTSMetadataURLs returns the deduped, sorted dynamic-stable metadata URLs for
// every JenkinsVersionProfile whose ResolveVersion is an exact MAJOR.MINOR.PATCH. A
// non-nil error means the profile list could not be read and callers MUST NOT treat
// the (empty) result as an authoritative "no LTS sources".
func (r *UpdateCenterReconciler) deriveLTSMetadataURLs(ctx context.Context, upstream string) ([]string, error) {
	profiles, err := crdstore.List[v1alpha1.JenkinsVersionProfile](ctx, r.store, "", "")
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	var urls []string
	for _, p := range profiles {
		rv := p.Spec.ResolveVersion
		if !ltsPatchRe.MatchString(rv) {
			continue
		}
		u := upstream + "/dynamic-stable-" + rv + "/update-center.actual.json"
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
	}
	sort.Strings(urls)
	return urls, nil
}

// writeMetadataConfigMap merges updates into the metadata ConfigMap, rewriting only
// when at least one key's value actually changes. Keys are compared independently,
// and a nil update means "leave this key exactly as it is" — that is how a failed
// derivation for one key avoids clobbering the other. Keys not mentioned in updates
// are carried over untouched. A best-effort operation: failures are logged, not fatal.
func (r *UpdateCenterReconciler) writeMetadataConfigMap(ctx context.Context, updates map[string]*string, owner metav1.OwnerReference, logger *slog.Logger) {
	cur, err := r.client.GetConfigMap(ctx, metadataConfigMapName, r.operatorNamespace)
	if err != nil {
		cur = nil // absent or unreadable: fall through to a create with whatever we have
	}

	data := make(map[string]string, len(cur)+len(updates))
	for k, v := range cur {
		data[k] = v
	}

	changed := false
	for k, v := range updates {
		if v == nil {
			continue // derivation failed for this key — leave it unchanged
		}
		if existing, ok := data[k]; !ok || existing != *v {
			data[k] = *v
			changed = true
		}
	}
	if !changed {
		return
	}

	if err := r.client.CreateOrUpdateConfigMap(ctx, metadataConfigMapName, r.operatorNamespace, data, owner); err != nil {
		logger.Warn("failed to write update-center metadata ConfigMap", "name", metadataConfigMapName, "error", err)
	}
}

// boundSources caps the source list at maxResolvedMetadataSources: when it would
// exceed the cap, the first (cap-1) entries are kept and the final entry is a single
// "…(+N more)" overflow marker.
func boundSources(sources []string) []string {
	if len(sources) <= maxResolvedMetadataSources {
		return sources
	}
	keep := maxResolvedMetadataSources - 1
	overflow := len(sources) - keep
	out := make([]string, 0, maxResolvedMetadataSources)
	out = append(out, sources[:keep]...)
	out = append(out, fmt.Sprintf("…(+%d more)", overflow))
	return out
}

// canonicalVersion maps every latest spelling (the empty string, or latest in any
// case) to "latest". The declared set holds one spelling for a latest pin, so a
// version-less entry and a latest entry collapse into one row.
func canonicalVersion(v string) string {
	if ucmeta.IsLatest(v) {
		return "latest"
	}
	return v
}

// coreForLatest returns the requesting core of a latest entry, and "" for a pinned
// version. A pinned version does not depend on the core.
func coreForLatest(dp declaredPlugin) string {
	if ucmeta.IsLatest(dp.Version) {
		return dp.Core
	}
	return ""
}

// requestingCore returns the exact core a controller or profile deploys, without a
// build suffix. It returns "" when the deployed version does not name an exact core.
// A two-segment version is an exact core only on the weekly channel. The baseline
// (an empty version or "lts", which have no profile) is the weekly 2.570 profile in
// the lock file. On the LTS channel a two-segment version is a line. An empty core
// accepts any stored version, which is the check the update center applied before
// core-aware reporting.
func requestingCore(version string, profile *v1alpha1.JenkinsVersionProfile) string {
	segs, ok := jenkinsver.Core(deployedCore(version, profile))
	if !ok {
		return ""
	}
	v := strings.TrimSpace(version)
	weekly := (profile != nil && profile.Spec.Channel == "weekly") || v == "" || v == "lts"
	if len(segs) == 2 && weekly {
		return fmt.Sprintf("%d.%d", segs[0], segs[1])
	}
	if len(segs) != 3 {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", segs[0], segs[1], segs[2])
}

// minCore returns the lower of two cores. An empty core requests nothing, so it
// loses to a non-empty core.
func minCore(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	case pluginver.Compare(a, b) <= 0:
		return a
	default:
		return b
	}
}

// bundleMinCores maps each bundle key "namespace/name" to the lowest deployed core of
// the Controllers that use that bundle. A bundle that no Controller uses has no entry.
// Controllers in member clusters are not visible here, so their cores are not counted.
func (r *UpdateCenterReconciler) bundleMinCores(ctx context.Context, profiles []*v1alpha1.JenkinsVersionProfile) (map[string]string, error) {
	controllers, err := crdstore.List[v1alpha1.Controller](ctx, r.store, "", "")
	if err != nil {
		return nil, err
	}
	out := make(map[string]string)
	for _, cr := range controllers {
		name, ns := v1alpha1.EffectiveBundleRef(cr, r.operatorNamespace)
		prof, _ := ResolveProfile(cr.Spec.Version, profiles)
		key := ns + "/" + name
		out[key] = minCore(out[key], requestingCore(cr.Spec.Version, prof))
	}
	return out, nil
}

// buildDeclaredSet reads all JenkinsVersionProfiles' materialized plugin sets,
// all ComposedBundles' plugin lines, and every open ProfileCandidate's
// resolved closure to build the declared plugin union.
func (r *UpdateCenterReconciler) buildDeclaredSet(ctx context.Context, logger *slog.Logger) ([]declaredPlugin, error) {
	var declared []declaredPlugin

	// 1. JenkinsVersionProfiles — read materialized ConfigMaps.
	profiles, err := crdstore.List[v1alpha1.JenkinsVersionProfile](ctx, r.store, "", "")
	if err != nil {
		return nil, fmt.Errorf("list JenkinsVersionProfiles: %w", err)
	}
	for _, p := range profiles {
		if p.Status.ContentRef == "" {
			continue
		}
		cmData, cmErr := r.client.GetConfigMap(ctx, p.Status.ContentRef, r.operatorNamespace)
		if cmErr != nil {
			logger.Warn("failed to read profile content ConfigMap", "profile", p.Name, "cm", p.Status.ContentRef, "error", cmErr)
			continue
		}
		pluginsYAML := cmData["plugins.yaml"]
		if pluginsYAML == "" {
			continue
		}
		entries := parseProfilePluginEntries(pluginsYAML)
		for _, e := range entries {
			declared = append(declared, declaredPlugin{
				Name:       e.ArtifactID,
				Version:    canonicalVersion(e.Version),
				RequiredBy: fmt.Sprintf("profile:%s", p.Name),
				Core:       requestingCore(p.Spec.Version, p),
			})
		}
	}

	// 2. ComposedBundles — read materialized ConfigMaps.
	bundles, err := crdstore.List[v1alpha1.ComposedBundle](ctx, r.store, "", "")
	if err != nil {
		return nil, fmt.Errorf("list ComposedBundles: %w", err)
	}
	bundleCores, err := r.bundleMinCores(ctx, profiles)
	if err != nil {
		return nil, fmt.Errorf("list Controllers: %w", err)
	}
	for _, cb := range bundles {
		if cb.Status.ContentRef == "" {
			continue
		}
		cmData, cmErr := r.client.GetConfigMap(ctx, cb.Status.ContentRef, cb.Namespace)
		if cmErr != nil {
			logger.Warn("failed to read bundle content ConfigMap", "bundle", cb.Name, "cm", cb.Status.ContentRef, "error", cmErr)
			continue
		}
		pluginsYAML := cmData["plugins.yaml"]
		if pluginsYAML == "" {
			continue
		}
		entries, parseErr := parsePluginEntries(pluginsYAML)
		if parseErr != nil {
			logger.Warn("failed to parse bundle plugins.yaml", "bundle", cb.Name, "error", parseErr)
			continue
		}
		for _, e := range entries {
			declared = append(declared, declaredPlugin{
				Name:       e.ArtifactId,
				Version:    canonicalVersion(e.Version),
				RequiredBy: fmt.Sprintf("bundle:%s/%s", cb.Namespace, cb.Name),
				Core:       bundleCores[cb.Namespace+"/"+cb.Name],
			})
		}
	}

	// 3. ProfileCandidates — read resolved closure ConfigMaps for open
	// candidates only. A Superseded, Failed, or Promoted candidate's closure
	// no longer represents anything pending servability, so it drops out of
	// the declared set once closed.
	candidates, err := crdstore.List[v1alpha1.ProfileCandidate](ctx, r.store, "", "")
	if err != nil {
		return nil, fmt.Errorf("list ProfileCandidates: %w", err)
	}
	for _, c := range candidates {
		if c.Status.Phase != v1alpha1.ProfileCandidatePhasePending && c.Status.Phase != v1alpha1.ProfileCandidatePhaseReady {
			continue
		}
		if c.Spec.ClosureContentRef == "" {
			continue
		}
		cmData, cmErr := r.client.GetConfigMap(ctx, c.Spec.ClosureContentRef, r.operatorNamespace)
		if cmErr != nil {
			logger.Warn("failed to read candidate closure ConfigMap", "candidate", c.Name, "cm", c.Spec.ClosureContentRef, "error", cmErr)
			continue
		}
		pluginsYAML := cmData["plugins.yaml"]
		if pluginsYAML == "" {
			continue
		}
		entries := parseProfilePluginEntries(pluginsYAML)
		for _, e := range entries {
			declared = append(declared, declaredPlugin{
				Name:       e.ArtifactID,
				Version:    canonicalVersion(e.Version),
				RequiredBy: fmt.Sprintf("candidate:%s", c.Name),
				Core:       c.Spec.ResolveVersion,
			})
		}
	}

	return declared, nil
}

// parseProfilePluginEntries parses a JenkinsVersionProfile plugins.yaml lock set.
func parseProfilePluginEntries(yamlStr string) []pluginlock.PluginEntry {
	var lockSet struct {
		Core    []string                 `yaml:"core"`
		Plugins []pluginlock.PluginEntry `yaml:"plugins"`
	}
	if err := yaml.Unmarshal([]byte(yamlStr), &lockSet); err != nil {
		return nil
	}
	return lockSet.Plugins
}

// ---------------------------------------------------------------------------
// §3.5 — phase derivation
// ---------------------------------------------------------------------------

// derivePhase sets status.Phase and the top-level Ready condition.
//
// Ready = StorageReady ∧ PluginsServable ≠ False. PluginsServable answers whether the
// update center can serve every declared plugin, from the store or by pull-through.
// CoverageComplete is informational: a plugin that is not stored yet is fetched on
// demand when pull-through is on. Phase is Ready only when PluginsServable is True.
//
// Storage failure gives phase Error. A plugin that cannot be served gives Ready=False
// and phase Degraded. A servability question that upstream could not answer keeps
// Ready=True with reason ResolutionDegraded, so an unhealthy upstream does not take
// the update center out of service.
func (r *UpdateCenterReconciler) derivePhase(status *v1alpha1.UpdateCenterStatus) {
	storageReady := conditionStatus(status.Conditions, condTypeStorageReady) == metav1.ConditionTrue
	servable := conditionStatus(status.Conditions, condTypePluginsServable)

	switch {
	case !storageReady:
		status.Phase = v1alpha1.UpdateCenterPhase("Error")
	case servable == metav1.ConditionTrue:
		status.Phase = v1alpha1.UpdateCenterPhase("Ready")
	default:
		status.Phase = v1alpha1.UpdateCenterPhase("Degraded")
	}

	// Top-level Ready condition. Every switch arm below assigns reason+message.
	var readyStatus metav1.ConditionStatus
	var readyReason, readyMsg string

	ps := conditionPtr(status.Conditions, condTypePluginsServable)
	switch {
	case !storageReady:
		readyStatus = metav1.ConditionFalse
		if sr := conditionPtr(status.Conditions, condTypeStorageReady); sr != nil {
			readyReason = sr.Reason
		} else {
			readyReason = reasonStorageUnavailable
		}
		readyMsg = "storage is unavailable"
	case servable == metav1.ConditionTrue:
		readyStatus = metav1.ConditionTrue
		readyReason = reasonAllPluginsServable
		readyMsg = "update center is ready"
	case servable == metav1.ConditionUnknown:
		readyStatus = metav1.ConditionTrue
		readyReason = reasonResolutionDegraded
		readyMsg = "serving; servability of some plugins is undecided"
	default: // PluginsServable=False, or not computed
		readyStatus = metav1.ConditionFalse
		if ps != nil {
			readyReason = ps.Reason
		} else {
			readyReason = "ServabilityNotComputed"
		}
		if readyReason == reasonPluginsUnservable {
			readyMsg = fmt.Sprintf("%d declared plugins cannot be served", status.UnservableCount)
		} else {
			readyMsg = "coverage could not be computed"
		}
	}

	r.setCondition(status, v1alpha1.UpdateCenterCondition{
		Type:    condTypeReady,
		Status:  readyStatus,
		Reason:  readyReason,
		Message: readyMsg,
	})
}

// conditionPtr returns a pointer to the condition with the given type, or nil.
func conditionPtr(conditions []v1alpha1.UpdateCenterCondition, condType string) *v1alpha1.UpdateCenterCondition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

// setAllConditionsFalse sets every condition to False with the given reason.
func (r *UpdateCenterReconciler) setAllConditionsFalse(status *v1alpha1.UpdateCenterStatus, reason, message string) {
	for _, ct := range []string{condTypeStorageReady, condTypeSeedImported, condTypeCoverageComplete, condTypePluginsServable, condTypeReady} {
		r.setCondition(status, v1alpha1.UpdateCenterCondition{
			Type:    ct,
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: message,
		})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// setCondition upserts a condition on the status, preserving LastTransitionTime
// when the condition hasn't actually changed.
func (r *UpdateCenterReconciler) setCondition(status *v1alpha1.UpdateCenterStatus, cond v1alpha1.UpdateCenterCondition) {
	cond.LastTransitionTime = metav1.Now()
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

// conditionStatus returns the Status of the condition with the given type,
// or ConditionUnknown if not found.
func conditionStatus(conditions []v1alpha1.UpdateCenterCondition, condType string) metav1.ConditionStatus {
	for _, c := range conditions {
		if c.Type == condType {
			return c.Status
		}
	}
	return metav1.ConditionUnknown
}

// boolToConditionStatus converts a bool to metav1.ConditionStatus.
func boolToConditionStatus(v bool) metav1.ConditionStatus {
	if v {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// writeTempDockerConfig writes secret data as a Docker config.json to a temp
// file and returns the path and a cleanup function.
func writeTempDockerConfig(secretData map[string][]byte) (path string, cleanup func(), err error) {
	configJSON, ok := secretData[".dockerconfigjson"]
	if !ok {
		// Try "config.json" key as fallback.
		configJSON = secretData["config.json"]
	}
	if len(configJSON) == 0 {
		return "", func() {}, fmt.Errorf("secret has no .dockerconfigjson or config.json key")
	}

	f, err := os.CreateTemp("", "varroa-uc-docker-config-")
	if err != nil {
		return "", func() {}, fmt.Errorf("create temp file: %w", err)
	}
	if _, err := f.Write(configJSON); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", func() {}, fmt.Errorf("write temp config: %w", err)
	}
	_ = f.Close()

	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}

// tarGzDirectory creates a tar.gz archive of dir and writes it to w.
func tarGzDirectory(dir string, w io.Writer) error {
	gzw := gzip.NewWriter(w)
	defer func() { _ = gzw.Close() }()

	tw := tar.NewWriter(gzw)
	defer func() { _ = tw.Close() }()

	// The oras-go oci layout store uses "index.json", "oci-layout", "blobs/".
	// We walk the directory and add every file.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read dir: %w", err)
	}

	var walkDir func(base string) error
	walkDir = func(base string) error {
		entries, err := os.ReadDir(base)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			fullPath := base + "/" + entry.Name()
			if entry.IsDir() {
				if err := walkDir(fullPath); err != nil {
					return err
				}
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			// Create tar header with a path relative to dir.
			relPath := strings.TrimPrefix(fullPath, dir)
			relPath = strings.TrimPrefix(relPath, "/")

			hdr := &tar.Header{
				Name: relPath,
				Size: info.Size(),
				Mode: int64(info.Mode()),
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, err := os.Open(fullPath)
			if err != nil {
				return err
			}
			if _, err := io.Copy(tw, f); err != nil {
				_ = f.Close()
				return err
			}
			_ = f.Close()
		}
		return nil
	}

	// Walk from dir to include everything.
	for _, entry := range entries {
		fullPath := dir + "/" + entry.Name()
		if entry.IsDir() {
			if err := walkDir(fullPath); err != nil {
				return err
			}
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relPath := entry.Name()
		hdr := &tar.Header{
			Name: relPath,
			Size: info.Size(),
			Mode: int64(info.Mode()),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(fullPath)
		if err != nil {
			return err
		}
		if _, err := io.Copy(tw, f); err != nil {
			_ = f.Close()
			return err
		}
		_ = f.Close()
	}

	return nil
}

// Ensure UpdateCenterReconciler implements reconcile.Reconciler.
var _ reconcile.Reconciler = (*UpdateCenterReconciler)(nil)
