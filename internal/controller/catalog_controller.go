package controller

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/oci"
)

const (
	// defaultSyncIntervalSec is the default sync interval for CatalogSources that
	// do not specify a SyncIntervalSeconds.
	defaultSyncIntervalSec = 300

	// minSyncIntervalSec is the minimum allowed sync interval, enforced to
	// prevent tight polling loops.
	minSyncIntervalSec = 30

	// syncRequestedAtAnno is the annotation key for requesting an immediate sync.
	// The value should be an RFC 3339 timestamp.
	syncRequestedAtAnno = "varroa.dev/sync-requested-at"

	// catalogSourceLabel is the label key set on CatalogItems to identify their
	// originating CatalogSource.
	catalogSourceLabel = "varroa.dev/catalog-source"

	// catalogTypeLabel is the label key set on CatalogItems for their type.
	catalogTypeLabel = "varroa.dev/catalog-type"
)

// CatalogReconciler synchronizes CatalogSource CRDs from git repositories
// and reconciles ComposedBundle CRDs.
type CatalogReconciler struct {
	client            ResourceClient
	store             crdstore.Backend
	cloner            *bundle.GitCloner
	resolver          *bundle.Resolver
	cloneCache        *bundle.CloneCache
	workDir           string
	logger            *slog.Logger
	operatorNamespace string

	// ucBaseURL is VARROA_UPDATE_CENTER_URL. Empty means the update center is
	// not enabled, which the reserved source reports as an error rather than
	// deriving nothing silently.
	ucBaseURL string
	// ucHTTP is this reconciler's OWN client, with a timeout. It deliberately
	// does not share UpdateCenterReconciler's doer, which is http.DefaultClient
	// in production and has none: the catalog ticker is leader-only and
	// reconciles every source serially, so one hung fetch would stop all
	// catalog syncing.
	ucHTTP httpDoer
}

// httpDoer is the narrow HTTP seam the update-center arm needs.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// NewCatalogReconciler creates a new CatalogReconciler.
func NewCatalogReconciler(client ResourceClient, store crdstore.Backend, cloner *bundle.GitCloner, resolver *bundle.Resolver, cloneCache *bundle.CloneCache, workDir string, logger *slog.Logger, operatorNamespace, ucBaseURL string, ucHTTP httpDoer) *CatalogReconciler {
	if ucHTTP == nil {
		ucHTTP = &http.Client{Timeout: 30 * time.Second}
	}
	return &CatalogReconciler{
		client:            client,
		store:             store,
		cloner:            cloner,
		resolver:          resolver,
		cloneCache:        cloneCache,
		workDir:           workDir,
		logger:            logger,
		operatorNamespace: operatorNamespace,
		ucBaseURL:         strings.TrimRight(ucBaseURL, "/"),
		ucHTTP:            ucHTTP,
	}
}

// Reconcile syncs a single CatalogSource: clones the repo, parses the index,
// creates or updates CatalogItem CRDs, and GCs stale items. Errors are
// communicated by setting the status to CatalogSyncError.
func (r *CatalogReconciler) Reconcile(ctx context.Context, src *v1alpha1.CatalogSource) {
	if src == nil {
		return
	}

	if src.Name == updateCenterSourceName {
		// Teardown backstop, ahead of the sync gate. UpdateCenterReconciler
		// returns early when the singleton is absent and is not registered at
		// all when VARROA_UPDATE_CENTER_URL is empty — so disabling the update
		// center is exactly the case where nothing else would remove the
		// reserved source. The owner reference covers a live cluster; this
		// covers a source orphaned by an install that predates it.
		if removed := r.teardownReservedSource(ctx, src); removed {
			return
		}
	}

	if !r.isSyncDue(src) {
		return
	}

	// Three-way dispatch. The reserved name is the only shape carrying neither
	// repoURL nor ociRef; CEL enforces that pairing but cannot see the
	// namespace, so the namespace guard lives here.
	switch {
	case src.Spec.RepoURL != "" || src.Spec.OCIRef != "":
	case src.Name == updateCenterSourceName && src.Namespace == r.operatorNamespace:
		r.markSyncing(ctx, src)
		r.reconcileUpdateCenterSource(ctx, src)
		return
	case src.Name == updateCenterSourceName:
		r.setError(ctx, src, fmt.Sprintf(
			"reserved name; the update-center catalog source is only valid in namespace %s", r.operatorNamespace))
		return
	default:
		r.setError(ctx, src, "must set either repoURL or ociRef")
		return
	}

	// Patch status to Syncing.
	r.markSyncing(ctx, src)

	// Build work directory.
	dir := filepath.Join(r.workDir, "catalogs", src.Namespace, src.Name)
	if err := os.RemoveAll(dir); err != nil {
		r.setError(ctx, src, fmt.Sprintf("clean work dir: %v", err))
		return
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		r.setError(ctx, src, fmt.Sprintf("create work dir: %v", err))
		return
	}

	// Clone the repository with optional git auth, or pull OCI artifact.
	var rev string
	if src.Spec.RepoURL != "" {
		var auth *bundle.GitAuth
		if src.Spec.SecretRef != "" {
			secData, err := r.client.GetSecret(ctx, src.Spec.SecretRef, src.Namespace)
			if err != nil {
				r.setError(ctx, src, fmt.Sprintf("read git auth secret %s: %v", src.Spec.SecretRef, err))
				return
			}
			auth, err = bundle.GitAuthFromSecret(secData, "")
			if err != nil {
				r.setError(ctx, src, fmt.Sprintf("bad git auth secret %s: %v", src.Spec.SecretRef, err))
				return
			}
			// Enforce host-scoped credential use for basic-auth git secrets.
			// An annotations read failure must fail closed (treat as
			// unannotated) for basic-auth, never skip the check.
			ann, aErr := r.client.GetSecretAnnotations(ctx, src.Spec.SecretRef, src.Namespace)
			if aErr != nil {
				r.logger.Warn("failed to read git auth secret annotations; treating as unannotated",
					"secret", src.Spec.SecretRef, "error", aErr)
			}
			if hErr := bundle.CheckGitSecretHost(auth, ann, src.Spec.RepoURL); hErr != nil {
				r.setError(ctx, src, fmt.Sprintf("git host check failed for secret %s: %v", src.Spec.SecretRef, hErr))
				return
			}
		}

		if r.cloneCache != nil {
			var err error
			rev, _, err = r.cloneCache.Checkout(ctx, src.Spec.RepoURL, src.Spec.Revision, dir, auth)
			if err != nil {
				r.setError(ctx, src, fmt.Sprintf("clone repo: %v", err))
				return
			}
		} else {
			if err := r.cloner.Clone(src.Spec.RepoURL, src.Spec.Revision, dir, auth); err != nil {
				r.setError(ctx, src, fmt.Sprintf("clone repo: %v", err))
				return
			}
			var err error
			rev, err = getGitRevision(dir)
			if err != nil {
				r.setError(ctx, src, fmt.Sprintf("resolve revision: %v", err))
				return
			}
		}
	} else if src.Spec.OCIRef != "" {
		// Pull OCI artifact.
		var auth *bundle.OCIAuth
		if src.Spec.SecretRef != "" {
			secData, err := r.client.GetSecret(ctx, src.Spec.SecretRef, src.Namespace)
			if err != nil {
				r.setError(ctx, src, fmt.Sprintf("read OCI auth secret %s: %v", src.Spec.SecretRef, err))
				return
			}
			auth, err = bundle.OCIAuthFromSecret(secData)
			if err != nil {
				r.setError(ctx, src, fmt.Sprintf("bad OCI auth secret %s: %v", src.Spec.SecretRef, err))
				return
			}
		}

		// Build a temp docker config.json for auth (if any).
		var configPath string
		var configDir string
		if auth != nil {
			var err error
			configPath, err = bundle.WriteDockerConfigJSON(auth)
			if err != nil {
				r.setError(ctx, src, fmt.Sprintf("prepare OCI auth: %v", err))
				return
			}
			configDir = filepath.Dir(configPath)
			defer func() { _ = os.RemoveAll(configDir) }()
		}

		store, err := oci.NewRegistryStore(src.Spec.OCIRef, oci.RegistryOptions{
			CredentialConfigPath: configPath,
			Insecure:             false,
		})
		if err != nil {
			r.setError(ctx, src, fmt.Sprintf("create registry store for %q: %v", src.Spec.OCIRef, err))
			return
		}

		// Resolve the manifest digest for ObservedRevision.
		desc, err := store.Resolve(ctx, src.Spec.OCIRef)
		if err != nil {
			r.setError(ctx, src, fmt.Sprintf("resolve OCI ref %q: %v", src.Spec.OCIRef, err))
			return
		}
		rev = desc.Digest

		// Pull the manifest to find the catalog bundle layer.
		manifest, err := store.Pull(ctx, src.Spec.OCIRef)
		if err != nil {
			r.setError(ctx, src, fmt.Sprintf("pull OCI artifact %q: %v", src.Spec.OCIRef, err))
			return
		}

		// Find and extract the catalog bundle layer.
		var layer *oci.Descriptor
		for _, l := range manifest.Layers {
			if l.MediaType == "application/vnd.varroa.catalog.v1.tar+gzip" {
				layer = &l
				break
			}
		}
		if layer == nil {
			r.setError(ctx, src, fmt.Sprintf("OCI artifact %q: no layer with media type application/vnd.varroa.catalog.v1.tar+gzip", src.Spec.OCIRef))
			return
		}

		rc, err := store.FetchBlob(ctx, layer.Digest)
		if err != nil {
			r.setError(ctx, src, fmt.Sprintf("fetch catalog layer %q: %v", layer.Digest, err))
			return
		}
		defer func() { _ = rc.Close() }()

		if err := untarGzipBundle(rc, dir); err != nil {
			r.setError(ctx, src, fmt.Sprintf("extract catalog bundle: %v", err))
			return
		}
	}

	// Determine catalog root directory.
	targetDir := dir
	if src.Spec.Path != "" {
		var err error
		targetDir, err = bundle.ResolveContainedPath(dir, src.Spec.Path)
		if err != nil {
			r.setError(ctx, src, fmt.Sprintf("catalog path: %v", err))
			return
		}
	}

	// Parse the catalog index.
	index, err := bundle.ParseCatalogIndex(targetDir)
	if err != nil {
		r.setError(ctx, src, fmt.Sprintf("parse catalog index: %v", err))
		return
	}

	// Create or update CatalogItem CRDs for each index entry.
	desired := make(map[string]bool)
	var warnings []string
	for _, entry := range index.Items {
		fullPath, err := bundle.ResolveContainedPath(targetDir, entry.Path)
		if err != nil {
			r.logger.Warn("skipping catalog entry with unsafe path",
				"source", src.Name, "path", entry.Path, "error", err)
			continue
		}
		content, err := os.ReadFile(fullPath)
		if err != nil {
			r.logger.Warn("skipping unreadable catalog entry",
				"source", src.Name, "path", entry.Path, "error", err)
			continue
		}
		contentStr := string(content)
		hash := sha256Hex([]byte(contentStr))
		valid, msg := bundle.ValidateCatalogItem(entry.Type, content, entry.Variables)

		status := v1alpha1.CatalogItemStatus{
			ContentHash:      hash,
			ObservedRevision: rev,
			Valid:            valid,
			Message:          msg,
		}
		if valid {
			status.Content = contentStr
		}

		item := &v1alpha1.CatalogItem{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deterministicName(src.Name, entry.Path),
				Namespace: src.Namespace,
				Labels: map[string]string{
					catalogSourceLabel: src.Name,
					catalogTypeLabel:   entry.Type,
				},
				OwnerReferences: []metav1.OwnerReference{
					ownerRef(src),
				},
			},
			Spec: v1alpha1.CatalogItemSpec{
				SourceRef:   src.Name,
				Type:        v1alpha1.CatalogItemType(entry.Type),
				DisplayName: entry.DisplayName,
				Description: entry.Description,
				Path:        entry.Path,
				Version:     entry.Version,
				Tags:        entry.Tags,
				Variables:   convertVariables(entry.Variables),
				Requires:    entry.Requires,
			},
			Status: status,
		}

		warnings = append(warnings, r.writeCatalogItem(ctx, src, item, desired)...)
	}

	// GC stale CatalogItems that were present on the cluster but are no longer
	// in the index.
	r.pruneCatalogItems(ctx, src, desired)

	r.setReady(ctx, src, rev, len(desired), warnings)
}

// markSyncing patches the source to the Syncing phase.
func (r *CatalogReconciler) markSyncing(ctx context.Context, src *v1alpha1.CatalogSource) {
	src.Status.Phase = v1alpha1.CatalogSyncSyncing
	if err := crdstore.PatchStatus[v1alpha1.CatalogSource](ctx, r.store, src.Name, src.Namespace, &src.Status); err != nil {
		r.logger.Error("failed to patch catalog source status to syncing",
			"source", src.Name, "namespace", src.Namespace, "error", err)
	}
}

// teardownReservedSource removes a reserved-name source in the operator
// namespace when the UpdateCenter singleton does not exist. It reports whether
// the source was deleted, in which case the caller must not go on to sync it.
func (r *CatalogReconciler) teardownReservedSource(ctx context.Context, src *v1alpha1.CatalogSource) bool {
	if src.Namespace != r.operatorNamespace {
		return false
	}
	_, err := crdstore.Get[v1alpha1.UpdateCenter](ctx, r.store, updateCenterSingletonName, "")
	if err == nil {
		return false
	}
	if !apierrors.IsNotFound(err) {
		r.logger.Warn("failed to read the UpdateCenter singleton; leaving the reserved source alone",
			"error", err)
		return false
	}
	r.logger.Info("removing the reserved catalog source: the UpdateCenter singleton is absent",
		"namespace", src.Namespace)
	if err := crdstore.Delete[v1alpha1.CatalogSource](ctx, r.store, src.Name, src.Namespace); err != nil {
		r.logger.Error("failed to delete the reserved catalog source", "error", err)
		return false
	}
	return true
}

// ReconcileComposedBundle updates the status of a ComposedBundle by composing
// its referenced items, materializing git inputs, and storing merged unresolved
// content in a ConfigMap.
func (r *CatalogReconciler) ReconcileComposedBundle(ctx context.Context, cb *v1alpha1.ComposedBundle) {
	if cb == nil {
		return
	}

	logger := r.logger.With("name", cb.Name, "namespace", cb.Namespace)

	previousPhase := cb.Status.Phase
	previousContentRef := cb.Status.ContentRef

	// Build inputSummary from spec.inputs for status projection.
	var inputSummary []v1alpha1.InputSummaryEntry
	for _, input := range cb.Spec.Inputs {
		if input.ItemRef != nil {
			entryType := ""
			resolvedNS := ""
			if input.ItemRef.Namespace != "" {
				item, err := crdstore.Get[v1alpha1.CatalogItem](ctx, r.store, input.ItemRef.Name, input.ItemRef.Namespace)
				if err == nil && item != nil {
					entryType, resolvedNS = string(item.Spec.Type), input.ItemRef.Namespace
				}
			} else {
				item, err := crdstore.Get[v1alpha1.CatalogItem](ctx, r.store, input.ItemRef.Name, cb.Namespace)
				if err == nil && item != nil {
					entryType, resolvedNS = string(item.Spec.Type), cb.Namespace
				} else if r.operatorNamespace != "" && r.operatorNamespace != cb.Namespace {
					item, err = crdstore.Get[v1alpha1.CatalogItem](ctx, r.store, input.ItemRef.Name, r.operatorNamespace)
					if err == nil && item != nil {
						entryType, resolvedNS = string(item.Spec.Type), r.operatorNamespace
					}
				}
			}
			inputSummary = append(inputSummary, v1alpha1.InputSummaryEntry{
				Kind:      "itemRef",
				Type:      entryType,
				Namespace: resolvedNS,
			})
		} else if input.GitSource != nil {
			inputSummary = append(inputSummary, v1alpha1.InputSummaryEntry{
				Kind: "gitSource",
				Type: "git",
			})
		} else if input.OCISource != nil {
			inputSummary = append(inputSummary, v1alpha1.InputSummaryEntry{
				Kind: "ociSource",
				Type: "oci",
			})
		}
	}

	// Pre-compute observed revisions for git inputs to detect drift.
	observedRevisions := make(map[string]string)
	if cb.Status.ObservedRevisions != nil {
		for k, v := range cb.Status.ObservedRevisions {
			observedRevisions[k] = v
		}
	}

	// Check for git input drift via ls-remote and itemRef drift via the
	// referenced CatalogItem's content hash. Collect resolved auth for the
	// composition pass. Skip compose entirely if all inputs are unchanged
	// and content exists.
	driftDetected := false
	resolvedAuth := make(map[int]*bundle.GitAuth)
	resolvedOCIAuth := make(map[int]*bundle.OCIAuth)
	for i, input := range cb.Spec.Inputs {
		inputKey := fmt.Sprintf("%d", i)

		if ref := input.ItemRef; ref != nil {
			var item *v1alpha1.CatalogItem
			var err error
			if ref.Namespace != "" {
				item, err = crdstore.Get[v1alpha1.CatalogItem](ctx, r.store, ref.Name, ref.Namespace)
			} else {
				item, err = crdstore.Get[v1alpha1.CatalogItem](ctx, r.store, ref.Name, cb.Namespace)
				if (err != nil || item == nil) && r.operatorNamespace != "" && r.operatorNamespace != cb.Namespace {
					item, err = crdstore.Get[v1alpha1.CatalogItem](ctx, r.store, ref.Name, r.operatorNamespace)
				}
			}
			if err != nil || item == nil {
				// Unresolvable item — the composer reports this via result.Missing.
				continue
			}
			hash := item.Status.ContentHash
			prevHash := observedRevisions[inputKey]
			if prevHash == hash && previousContentRef != "" {
				// Unchanged — skip re-materialization for this input.
				continue
			}
			if prevHash != hash {
				// Covers both an actual content change and a bundle that
				// reached Ready before itemRef hashes were tracked (no
				// observedRevisions entry yet) — either way, force one
				// recompose so the baseline gets persisted via patchStatus
				// instead of being silently discarded by the skip path below.
				driftDetected = true
			}
			observedRevisions[inputKey] = hash
			continue
		}

		if input.GitSource == nil && input.OCISource == nil {
			continue
		}

		// Handle GitSource drift detection.
		if input.GitSource != nil {
			gs := input.GitSource

			// Resolve auth for the git input if a SecretRef is specified.
			var auth *bundle.GitAuth
			if gs.SecretRef != "" {
				secData, err := r.client.GetSecret(ctx, gs.SecretRef, cb.Namespace)
				if err != nil {
					status := r.buildStatus(cb, v1alpha1.ComposedBundleInvalid, 0, "", "",
						observedRevisions,
						[]string{fmt.Sprintf("git input[%d]: read secret %s: %v", i, gs.SecretRef, err)},
						nil, inputSummary)
					r.patchStatus(ctx, cb, status)
					return
				}
				auth, err = bundle.GitAuthFromSecret(secData, "")
				if err != nil {
					status := r.buildStatus(cb, v1alpha1.ComposedBundleInvalid, 0, "", "",
						observedRevisions,
						[]string{fmt.Sprintf("git input[%d]: bad secret %s: %v", i, gs.SecretRef, err)},
						nil, inputSummary)
					r.patchStatus(ctx, cb, status)
					return
				}
				resolvedAuth[i] = auth

				// Enforce host-scoped credential use for basic-auth git secrets.
				// An annotations read failure must fail closed (treat as
				// unannotated) for basic-auth, never skip the check.
				ann, aErr := r.client.GetSecretAnnotations(ctx, gs.SecretRef, cb.Namespace)
				if aErr != nil {
					r.logger.Warn("failed to read git auth secret annotations; treating as unannotated",
						"secret", gs.SecretRef, "error", aErr)
				}
				if hErr := bundle.CheckGitSecretHost(auth, ann, gs.RepoURL); hErr != nil {
					status := r.buildStatus(cb, v1alpha1.ComposedBundleInvalid, 0, "", "",
						observedRevisions,
						[]string{fmt.Sprintf("git input[%d]: %v", i, hErr)},
						nil, inputSummary)
					r.patchStatus(ctx, cb, status)
					return
				}
			}

			// Check remote SHA for drift detection.
			sha, err := r.cloner.RemoteSHA(gs.RepoURL, gs.Revision, auth)
			if err != nil {
				logger.Warn("git ls-remote failed", "input", i, "repo", gs.RepoURL, "error", err)
				continue
			}
			prevSHA := observedRevisions[inputKey]
			if prevSHA == sha && previousContentRef != "" {
				continue
			}
			if prevSHA != "" && prevSHA != sha {
				driftDetected = true
			}
			observedRevisions[inputKey] = sha
		}

		// Handle OCISource drift detection.
		if input.OCISource != nil {
			ocs := input.OCISource

			// Resolve auth for the OCI input if a SecretRef is specified.
			var auth *bundle.OCIAuth
			if ocs.SecretRef != "" {
				secData, err := r.client.GetSecret(ctx, ocs.SecretRef, cb.Namespace)
				if err != nil {
					status := r.buildStatus(cb, v1alpha1.ComposedBundleInvalid, 0, "", "",
						observedRevisions,
						[]string{fmt.Sprintf("OCI input[%d]: read secret %s: %v", i, ocs.SecretRef, err)},
						nil, inputSummary)
					r.patchStatus(ctx, cb, status)
					return
				}
				auth, err = bundle.OCIAuthFromSecret(secData)
				if err != nil {
					status := r.buildStatus(cb, v1alpha1.ComposedBundleInvalid, 0, "", "",
						observedRevisions,
						[]string{fmt.Sprintf("OCI input[%d]: bad secret %s: %v", i, ocs.SecretRef, err)},
						nil, inputSummary)
					r.patchStatus(ctx, cb, status)
					return
				}
				resolvedOCIAuth[i] = auth
			}

			// Build a temp docker config.json for auth (if any) to resolve the manifest digest.
			var configPath string
			var configDir string
			if auth != nil {
				var err error
				configPath, err = bundle.WriteDockerConfigJSON(auth)
				if err != nil {
					status := r.buildStatus(cb, v1alpha1.ComposedBundleInvalid, 0, "", "",
						observedRevisions,
						[]string{fmt.Sprintf("OCI input[%d]: prepare auth: %v", i, err)},
						nil, inputSummary)
					r.patchStatus(ctx, cb, status)
					return
				}
				configDir = filepath.Dir(configPath)
				defer func() { _ = os.RemoveAll(configDir) }()
			}

			store, err := oci.NewRegistryStore(ocs.Ref, oci.RegistryOptions{
				CredentialConfigPath: configPath,
				Insecure:             false,
			})
			if err != nil {
				logger.Warn("OCI registry store failed", "input", i, "ref", ocs.Ref, "error", err)
				continue
			}

			// Resolve CURRENT manifest digest for drift detection.
			desc, err := store.Resolve(ctx, ocs.Ref)
			if err != nil {
				logger.Warn("OCI resolve failed", "input", i, "ref", ocs.Ref, "error", err)
				continue
			}
			digest := desc.Digest
			prevDigest := observedRevisions[inputKey]
			if prevDigest == digest && previousContentRef != "" {
				continue
			}
			if prevDigest != "" && prevDigest != digest {
				driftDetected = true
			}
			observedRevisions[inputKey] = digest
		}
	}

	// If no drift, content ConfigMap exists, and we're not overdue for a
	// full sync, skip the expensive compose+write cycle. The ls-remote loop
	// above always runs, so drift is detected on every reconcile regardless.
	generationChanged := cb.Generation != cb.Status.ObservedGeneration
	if !generationChanged &&
		!r.isComposedBundleSyncDue(cb) &&
		!driftDetected &&
		previousContentRef != "" &&
		!r.hasNewGitInputs(cb, observedRevisions) &&
		!r.hasNewOCIInputs(cb, observedRevisions) {
		if _, err := r.client.GetConfigMap(ctx, previousContentRef, cb.Namespace); err == nil {
			return
		}
		logger.Info("content ConfigMap missing, re-materializing", "contentRef", previousContentRef)
	}

	// Run the composition pipeline with pre-resolved git auth.
	composer := bundle.NewComposer(storeItemLookup{r.store}, r.resolver, r.workDir, "", "", "", r.operatorNamespace)
	result, err := composer.Compose(ctx, cb.Namespace, &cb.Spec, resolvedAuth, resolvedOCIAuth)
	if err != nil {
		status := r.buildStatus(cb, v1alpha1.ComposedBundleInvalid, len(cb.Spec.Inputs), "",
			previousContentRef, observedRevisions,
			[]string{fmt.Sprintf("compose error: %v", err)}, nil, inputSummary)
		// Last-good retention: keep previous content on re-materialization failure.
		r.patchStatus(ctx, cb, status)
		return
	}

	// Determine phase from composition result.
	phase := v1alpha1.ComposedBundleReady
	allErrors := append([]string{}, result.Errors...)
	var allWarnings []string
	allWarnings = append(allWarnings, result.Warnings...)

	if len(result.Missing) > 0 {
		phase = v1alpha1.ComposedBundleInvalid
		allErrors = append(allErrors, fmt.Sprintf("missing items: %s", strings.Join(result.Missing, ", ")))
	} else if len(result.Drifted) > 0 {
		phase = v1alpha1.ComposedBundleDrifted
		allWarnings = append(allWarnings, fmt.Sprintf("drifted items: %s", strings.Join(result.Drifted, ", ")))
	} else if len(result.Errors) > 0 {
		phase = v1alpha1.ComposedBundleInvalid
	}

	// Store materialized content in a ConfigMap owned by the ComposedBundle.
	contentName := cb.Name + "-content"
	if phase == v1alpha1.ComposedBundleReady || phase == v1alpha1.ComposedBundleDrifted {
		if result.Materialized != nil {
			// Enforce size limit (~1MB for ConfigMap data).
			// Serialize variables as key=value lines for storage.
			var varLines []string
			for k, v := range result.Materialized.Variables {
				varLines = append(varLines, k+": "+v)
			}
			contentData := map[string]string{
				"jenkins.yaml":   result.Materialized.JenkinsYAML,
				"plugins.yaml":   result.Materialized.PluginsYAML,
				"items.yaml":     result.Materialized.ItemsYAML,
				"rbac.yaml":      result.Materialized.RbacYAML,
				"variables.yaml": strings.Join(varLines, "\n"),
			}
			totalSize := 0
			for _, v := range contentData {
				totalSize += len(v)
			}
			if totalSize > 900*1024 { // ~900KB, leaving headroom
				status := r.buildStatus(cb, v1alpha1.ComposedBundleInvalid, len(cb.Spec.Inputs), "",
					previousContentRef, observedRevisions,
					[]string{"content too large: merged bundle exceeds ConfigMap size limit"},
					nil, inputSummary)
				r.patchStatus(ctx, cb, status)
				return
			}

			if err := r.client.CreateOrUpdateConfigMap(ctx, contentName, cb.Namespace, contentData, composedBundleOwnerRef(cb)); err != nil {
				logger.Warn("failed to write content configmap", "error", err)
				allErrors = append(allErrors, fmt.Sprintf("write content: %v", err))
				phase = v1alpha1.ComposedBundleInvalid
			}
		}
	}

	// contentRef: only set when we successfully wrote a ConfigMap (Ready/Drifted)
	// or when retaining last-good content from a previously-Ready bundle.
	contentRef := ""
	if phase == v1alpha1.ComposedBundleReady || phase == v1alpha1.ComposedBundleDrifted {
		contentRef = contentName
	} else if phase == v1alpha1.ComposedBundleInvalid && previousPhase == v1alpha1.ComposedBundleReady {
		contentRef = previousContentRef // keep last-good
	}

	resolvedHash := result.ResolvedHash
	if resolvedHash == "" {
		resolvedHash = cb.Status.ResolvedHash
	}
	status := r.buildStatus(cb, phase, len(cb.Spec.Inputs), resolvedHash, contentRef,
		observedRevisions, allErrors, allWarnings, inputSummary)
	r.patchStatus(ctx, cb, status)
}

// hasNewGitInputs returns true if there are git inputs whose revision hasn't
// been observed yet (first-time materialization).
func (r *CatalogReconciler) hasNewGitInputs(cb *v1alpha1.ComposedBundle, observed map[string]string) bool {
	for i, input := range cb.Spec.Inputs {
		if input.GitSource == nil {
			continue
		}
		inputKey := fmt.Sprintf("%d", i)
		if _, ok := observed[inputKey]; !ok {
			return true
		}
	}
	return false
}

// hasNewOCIInputs returns true if there are OCI inputs whose digest hasn't
// been observed yet (first-time materialization).
func (r *CatalogReconciler) hasNewOCIInputs(cb *v1alpha1.ComposedBundle, observed map[string]string) bool {
	for i, input := range cb.Spec.Inputs {
		if input.OCISource == nil {
			continue
		}
		inputKey := fmt.Sprintf("%d", i)
		if _, ok := observed[inputKey]; !ok {
			return true
		}
	}
	return false
}

// isComposedBundleSyncDue checks whether the ComposedBundle needs a full
// re-materialization pass. The ls-remote drift loop always runs regardless;
// this only gates the expensive clone+merge+write cycle.
func (r *CatalogReconciler) isComposedBundleSyncDue(cb *v1alpha1.ComposedBundle) bool {
	if cb.Status.ContentRef == "" {
		return true
	}
	if cb.Status.Phase != v1alpha1.ComposedBundleReady &&
		cb.Status.Phase != v1alpha1.ComposedBundleDrifted &&
		cb.Status.Phase != v1alpha1.ComposedBundleInvalid {
		return true
	}
	return false
}

// buildStatus constructs a ComposedBundleStatus with the given fields.
func (r *CatalogReconciler) buildStatus(cb *v1alpha1.ComposedBundle, phase v1alpha1.ComposedBundlePhase, itemCount int, resolvedHash, contentRef string, observedRevisions map[string]string, errors, warnings []string, inputSummary []v1alpha1.InputSummaryEntry) *v1alpha1.ComposedBundleStatus {
	msg := strings.Join(warnings, "; ")
	if len(errors) > 0 {
		if msg != "" {
			msg = strings.Join(errors, "; ") + "; " + msg
		} else {
			msg = strings.Join(errors, "; ")
		}
	}

	if resolvedHash == "" {
		resolvedHash = cb.Status.ResolvedHash // keep previous if not recalculated
	}

	return &v1alpha1.ComposedBundleStatus{
		Phase:              phase,
		ItemCount:          itemCount,
		ResolvedHash:       resolvedHash,
		ContentRef:         contentRef,
		ObservedRevisions:  observedRevisions,
		Errors:             errors,
		Warnings:           warnings,
		Message:            msg,
		InputSummary:       inputSummary,
		ObservedGeneration: cb.Generation,
	}
}

// patchStatus patches the ComposedBundle status.
func (r *CatalogReconciler) patchStatus(ctx context.Context, cb *v1alpha1.ComposedBundle, status *v1alpha1.ComposedBundleStatus) {
	if err := crdstore.PatchStatus[v1alpha1.ComposedBundle](ctx, r.store, cb.Name, cb.Namespace, status); err != nil {
		r.logger.Warn("failed to patch composed bundle status", "name", cb.Name, "namespace", cb.Namespace, "error", err)
	}
}

// isSyncDue checks whether the CatalogSource is due for a sync based on its
// interval, revision state, and the optional sync-requested-at annotation.
func (r *CatalogReconciler) isSyncDue(src *v1alpha1.CatalogSource) bool {
	interval := src.Spec.SyncIntervalSeconds
	if interval <= 0 {
		interval = defaultSyncIntervalSec
	}
	if interval < minSyncIntervalSec {
		interval = minSyncIntervalSec
	}

	// Never synced.
	if src.Status.LastSyncTime == nil {
		return true
	}
	// No resolved revision yet.
	if src.Status.ObservedRevision == "" {
		return true
	}
	// Interval elapsed.
	if time.Since(src.Status.LastSyncTime.Time) >= time.Duration(interval)*time.Second {
		return true
	}
	// Manual sync requested via annotation.
	if annoVal, ok := src.Annotations[syncRequestedAtAnno]; ok && annoVal != "" {
		requestedAt, err := time.Parse(time.RFC3339, annoVal)
		if err == nil && requestedAt.After(src.Status.LastSyncTime.Time) {
			return true
		}
	}
	return false
}

// setError patches the CatalogSource status to CatalogSyncError with the given
// message and logs the error.
func (r *CatalogReconciler) setError(ctx context.Context, src *v1alpha1.CatalogSource, msg string) {
	r.logger.Error("catalog sync error", "source", src.Name, "namespace", src.Namespace, "error", msg)
	src.Status.Phase = v1alpha1.CatalogSyncError
	src.Status.Message = msg
	if err := crdstore.PatchStatus[v1alpha1.CatalogSource](ctx, r.store, src.Name, src.Namespace, &src.Status); err != nil {
		r.logger.Error("failed to patch catalog source status to error",
			"source", src.Name, "namespace", src.Namespace, "error", err)
	}
}

// getGitRevision returns the full SHA of HEAD in the given git repository.
func getGitRevision(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// storeItemLookup adapts crdstore.Backend to bundle.ItemLookup.
type storeItemLookup struct {
	store crdstore.Backend
}

func (s storeItemLookup) GetCatalogItemCRD(ctx context.Context, name, namespace string) (*v1alpha1.CatalogItem, error) {
	return crdstore.Get[v1alpha1.CatalogItem](ctx, s.store, name, namespace)
}

// deterministicName builds a predictable name for a CatalogItem from the
// CatalogSource name and the file path within the catalog repository.
func deterministicName(sourceName, path string) string {
	slug := strings.ReplaceAll(path, "/", "-")
	slug = strings.ReplaceAll(slug, "_", "-")
	slug = strings.ReplaceAll(slug, ".yaml", "")
	slug = strings.ReplaceAll(slug, ".yml", "")
	return sourceName + "-" + slug
}

// convertVariables converts []bundle.CatalogVarDecl to []v1alpha1.CatalogVariable.
func convertVariables(decls []bundle.CatalogVarDecl) []v1alpha1.CatalogVariable {
	vars := make([]v1alpha1.CatalogVariable, len(decls))
	for i, d := range decls {
		vars[i] = v1alpha1.CatalogVariable{
			Name:          d.Name,
			Default:       d.Default,
			Description:   d.Description,
			Required:      d.Required,
			Type:          d.Type,
			AllowedValues: d.AllowedValues,
		}
	}
	return vars
}

// composedBundleOwnerRef returns an OwnerReference pointing to the
// ComposedBundle so that its materialized content ConfigMap is
// garbage-collected when the bundle is deleted.
func composedBundleOwnerRef(cb *v1alpha1.ComposedBundle) metav1.OwnerReference {
	controller := true
	apiVersion := cb.APIVersion
	kind := cb.Kind
	// Defensive fallback: if the object was constructed without TypeMeta (e.g.
	// in tests), use the known GVK constants so GC still works.
	if apiVersion == "" {
		apiVersion = v1alpha1.SchemeGroupVersion.String()
	}
	if kind == "" {
		kind = "ComposedBundle"
	}
	return metav1.OwnerReference{
		APIVersion:         apiVersion,
		Kind:               kind,
		Name:               cb.Name,
		UID:                cb.UID,
		BlockOwnerDeletion: &controller,
		Controller:         &controller,
	}
}

// ownerRef returns an OwnerReference pointing to the CatalogSource so that
// CatalogItems are garbage-collected when the source is deleted.
func ownerRef(src *v1alpha1.CatalogSource) metav1.OwnerReference {
	controller := true
	apiVersion := src.APIVersion
	kind := src.Kind
	// Defensive fallback: if the object was constructed without TypeMeta (e.g.
	// in tests), use the known GVK constants so GC still works.
	if apiVersion == "" {
		apiVersion = v1alpha1.SchemeGroupVersion.String()
	}
	if kind == "" {
		kind = "CatalogSource"
	}
	return metav1.OwnerReference{
		APIVersion:         apiVersion,
		Kind:               kind,
		Name:               src.Name,
		UID:                src.UID,
		BlockOwnerDeletion: &controller,
		Controller:         &controller,
	}
}

// untarGzipBundle decompresses a gzipped tar stream into the given directory.
func untarGzipBundle(r io.Reader, targetDir string) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("create gzip reader: %w", err)
	}
	defer func() { _ = gzr.Close() }()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}

		cleanPath := filepath.Join(targetDir, filepath.Clean(header.Name))
		if !strings.HasPrefix(cleanPath, filepath.Clean(targetDir)+string(os.PathSeparator)) {
			return fmt.Errorf("tar entry %q escapes target directory", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(cleanPath, 0755); err != nil {
				return fmt.Errorf("create dir %q: %w", cleanPath, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(cleanPath), 0755); err != nil {
				return fmt.Errorf("create parent dir for %q: %w", cleanPath, err)
			}
			f, err := os.Create(cleanPath)
			if err != nil {
				return fmt.Errorf("create file %q: %w", cleanPath, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return fmt.Errorf("write file %q: %w", cleanPath, err)
			}
			_ = f.Close()
		}
	}
	return nil
}
