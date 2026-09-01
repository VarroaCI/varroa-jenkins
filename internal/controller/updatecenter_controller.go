package controller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
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
	"github.com/varroaci/varroa-jenkins/internal/oci"
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
	condTypeReady            = "Ready"

	// Reason strings (exact per spec).
	reasonStorageUnavailable   = "StorageUnavailable"
	reasonInventoryUnavailable = "InventoryUnavailable"
	reasonGapAnalysisComplete  = "GapAnalysisComplete"

	// Max gaps to surface.
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
	r.derivePhase(status, uc.Spec.PullThrough.Enabled)
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
}

// computeGaps fetches the UC inventory, builds the declared set from profiles
// and bundles, and computes gaps = declared − inventory.
func (r *UpdateCenterReconciler) computeGaps(ctx context.Context, status *v1alpha1.UpdateCenterStatus, logger *slog.Logger) {
	// §3.4a — fetch inventory.
	inventoryURL := r.ucBaseURL + "/api/v1/inventory"
	inventory, skipped, err := r.fetchInventory(ctx, inventoryURL)
	if err != nil {
		logger.Warn("inventory fetch failed", "error", err)
		r.setCondition(status, v1alpha1.UpdateCenterCondition{
			Type:    condTypeCoverageComplete,
			Status:  metav1.ConditionFalse,
			Reason:  reasonInventoryUnavailable,
			Message: fmt.Sprintf("inventory unavailable: %v", err),
		})
		// Leave status.Gaps unchanged — do NOT overwrite.
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

	// Set PluginCount and StoreBytes from the inventory response.
	status.PluginCount = len(inventory)
	var totalBytes int64
	for _, e := range inventory {
		totalBytes += e.SizeBytes
	}
	status.StoreBytes = totalBytes

	// §3.4b — build declared set from JenkinsVersionProfiles and ComposedBundles.
	declared, err := r.buildDeclaredSet(ctx, logger)
	if err != nil {
		logger.Warn("failed to build declared plugin set", "error", err)
		// Treat this like inventory failure: CoverageComplete=False, leave gaps.
		r.setCondition(status, v1alpha1.UpdateCenterCondition{
			Type:    condTypeCoverageComplete,
			Status:  metav1.ConditionFalse,
			Reason:  reasonInventoryUnavailable,
			Message: fmt.Sprintf("declared set unavailable: %v", err),
		})
		return
	}

	// Compute gaps: declared − inventory.
	var gaps []v1alpha1.UpdateCenterGap
	for _, dp := range declared {
		key := fmt.Sprintf("%s@%s", dp.Name, dp.Version)
		if !invSet[key] {
			gaps = append(gaps, v1alpha1.UpdateCenterGap{
				Plugin:     dp.Name,
				Version:    dp.Version,
				RequiredBy: dp.RequiredBy,
			})
		}
	}

	// Truncate to maxGaps.
	truncated := len(gaps) > maxGaps
	if truncated {
		gaps = gaps[:maxGaps]
	}

	status.Gaps = gaps

	coverageComplete := len(gaps) == 0
	msg := "coverage complete"
	if !coverageComplete {
		msg = fmt.Sprintf("%d plugins missing from store", len(gaps))
		if truncated {
			msg += fmt.Sprintf(" (%d more gaps not shown)", len(declared)-maxGaps)
		}
	}
	msg += skippedNote

	r.setCondition(status, v1alpha1.UpdateCenterCondition{
		Type:    condTypeCoverageComplete,
		Status:  boolToConditionStatus(coverageComplete),
		Reason:  reasonGapAnalysisComplete,
		Message: msg,
	})
}

// fetchInventory GETs the /api/v1/inventory endpoint and parses its response.
// The second result names every plugin-pack manifest the update center could
// not read (see ucInventoryResponse); it is empty on a fully complete scan.
func (r *UpdateCenterReconciler) fetchInventory(ctx context.Context, url string) ([]inventoryEntry, []ucSkippedPack, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := r.httpDoer.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("GET inventory: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, nil, fmt.Errorf("inventory HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload ucInventoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, nil, fmt.Errorf("parse inventory JSON: %w", err)
	}
	return payload.Plugins, payload.SkippedPacks, nil
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
// newline-delimited "name@version" body of the declared-plugins key. It returns nil
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
		if dp.Name == "" || dp.Version == "" {
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
				Version:    e.Version,
				RequiredBy: fmt.Sprintf("profile:%s", p.Name),
			})
		}
	}

	// 2. ComposedBundles — read materialized ConfigMaps.
	bundles, err := crdstore.List[v1alpha1.ComposedBundle](ctx, r.store, "", "")
	if err != nil {
		return nil, fmt.Errorf("list ComposedBundles: %w", err)
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
				Version:    e.Version,
				RequiredBy: fmt.Sprintf("bundle:%s/%s", cb.Namespace, cb.Name),
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
				Version:    e.Version,
				RequiredBy: fmt.Sprintf("candidate:%s", c.Name),
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
// Readiness = StorageReady ∧ (CoverageComplete ∨ pull-through-covers-gaps). In
// pull-through mode the store serves misses on demand (fetching from upstream,
// with jenkins-plugin-cli falling back to upstream directly for anything the
// store can't resolve), so genuine coverage gaps are an informational quality
// signal, not a serving prerequisite — a cold-cache pull-through UC is still
// usable and must report Ready so controllers route to it (otherwise the store
// never warms: the native path only engages when Ready, but the store only
// fills when the native path is used). Only an air-gapped UC (pull-through
// disabled) with gaps is genuinely Degraded: it cannot serve a plugin it does
// not hold.
//
// Pull-through does NOT mask a failure to *compute* coverage (inventory endpoint
// down, declared-set unbuildable → CoverageComplete=False with reason
// InventoryUnavailable): that signals an unhealthy UC HTTP API, so it stays out
// of Ready regardless of pull-through. Only genuine gaps (reason
// GapAnalysisComplete) are waived.
//
// The Ready condition's Reason surfaces specific context (e.g.
// InventoryUnavailable rather than a generic "NotReady").
func (r *UpdateCenterReconciler) derivePhase(status *v1alpha1.UpdateCenterStatus, pullThroughEnabled bool) {
	storageReady := conditionStatus(status.Conditions, condTypeStorageReady) == metav1.ConditionTrue
	coverageComplete := conditionStatus(status.Conditions, condTypeCoverageComplete) == metav1.ConditionTrue

	// Pull-through waives genuine gaps (reason GapAnalysisComplete) but not a
	// coverage-computation failure.
	gapsOnly := false
	if cc := conditionPtr(status.Conditions, condTypeCoverageComplete); cc != nil {
		gapsOnly = cc.Reason == reasonGapAnalysisComplete
	}
	pullThroughCoversGaps := pullThroughEnabled && gapsOnly
	usable := coverageComplete || pullThroughCoversGaps

	// Derive phase.
	switch {
	case !storageReady:
		status.Phase = v1alpha1.UpdateCenterPhase("Error")
	case usable:
		status.Phase = v1alpha1.UpdateCenterPhase("Ready")
	default:
		// Storage ready, coverage incomplete and not waivable: degraded.
		status.Phase = v1alpha1.UpdateCenterPhase("Degraded")
	}

	// Top-level Ready condition. Every switch arm below assigns reason+message.
	readyStatus := metav1.ConditionFalse
	var readyReason, readyMsg string

	switch {
	case storageReady && coverageComplete:
		readyStatus = metav1.ConditionTrue
		readyReason = "AllConditionsPassed"
		readyMsg = "update center is ready"
	case storageReady && pullThroughCoversGaps:
		// Coverage incomplete but pull-through serves misses on demand.
		readyStatus = metav1.ConditionTrue
		readyReason = "PullThroughServing"
		readyMsg = "update center is serving via pull-through; missing plugins are fetched on demand"
	case !storageReady:
		if sr := conditionPtr(status.Conditions, condTypeStorageReady); sr != nil {
			readyReason = sr.Reason
		} else {
			readyReason = reasonStorageUnavailable
		}
		readyMsg = "storage is unavailable"
	default: // storage ready, coverage incomplete, not waived
		if cc := conditionPtr(status.Conditions, condTypeCoverageComplete); cc != nil {
			readyReason = cc.Reason
		} else {
			readyReason = "CoverageIncomplete"
		}
		readyMsg = "coverage is incomplete"
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

// setAllConditionsFalse sets all four conditions to False with the given reason.
func (r *UpdateCenterReconciler) setAllConditionsFalse(status *v1alpha1.UpdateCenterStatus, reason, message string) {
	for _, ct := range []string{condTypeStorageReady, condTypeSeedImported, condTypeCoverageComplete, condTypeReady} {
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
