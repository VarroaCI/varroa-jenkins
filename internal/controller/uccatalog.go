package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

const (
	// updateCenterSourceName is the reserved CatalogSource name meaning "backed
	// by the update-center plugin store". It is valid only in the operator
	// namespace — CEL at the root schema cannot read metadata.namespace, so
	// that half is enforced here.
	updateCenterSourceName = v1alpha1.UpdateCenterCatalogSourceName

	// managedByLabel marks objects whose lifecycle the operator owns.
	managedByLabel = "varroa.dev/managed-by"
	// managedByOperator is the value managedByLabel carries.
	managedByOperator = "varroa-operator"

	// pluginNameLabel and pluginVersionLabel let a consumer select derived items
	// by plugin without parsing the item name.
	pluginNameLabel    = "varroa.dev/plugin-name"
	pluginVersionLabel = "varroa.dev/plugin-version"

	// ucItemNameHashPrefix domain-separates the content-addressed name hash.
	ucItemNameHashPrefix = "dev.varroa.uc-item:"

	// maxSourceWarnings caps how many warnings the source's terminal message
	// carries before it degrades to a count.
	maxSourceWarnings = 5
)

// ---------------------------------------------------------------------------
// Reserved source lifecycle
// ---------------------------------------------------------------------------

// desiredUpdateCenterSource is the shape the operator asserts for the reserved
// source: no repoURL, no ociRef, the default sync interval, and trusted left
// false — that flag gates script-bearing item types, and derived items are
// type: plugin carrying no scripts, so true would grant meaning this source
// does not need.
func desiredUpdateCenterSource(namespace string, uc *v1alpha1.UpdateCenter) *v1alpha1.CatalogSource {
	controller := true
	return &v1alpha1.CatalogSource{
		ObjectMeta: metav1.ObjectMeta{
			Name:      updateCenterSourceName,
			Namespace: namespace,
			Labels:    map[string]string{managedByLabel: managedByOperator},
			// A namespaced dependent may have a cluster-scoped owner, so
			// Kubernetes GC removes the source — and transitively every derived
			// item — when the singleton is deleted, with no controller running
			// at all. That matters because UpdateCenterReconciler returns early
			// when the singleton is absent and is not registered at all when
			// VARROA_UPDATE_CENTER_URL is empty: disabling the update center is
			// precisely the case where nothing else would clean up.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         v1alpha1.SchemeGroupVersion.String(),
				Kind:               "UpdateCenter",
				Name:               uc.Name,
				UID:                uc.UID,
				Controller:         &controller,
				BlockOwnerDeletion: &controller,
			}},
		},
		Spec: v1alpha1.CatalogSourceSpec{Trusted: false},
	}
}

// reconcileReservedCatalogSource creates or adopts the reserved CatalogSource
// alongside the UpdateCenter singleton. Adoption is deliberate: if a user raced
// it, or an older install left one behind, the spec is overwritten and the
// labels stamped rather than erroring. Converge, never wedge.
func (r *UpdateCenterReconciler) reconcileReservedCatalogSource(ctx context.Context, uc *v1alpha1.UpdateCenter, logger *slog.Logger) {
	desired := desiredUpdateCenterSource(r.operatorNamespace, uc)
	existing, err := crdstore.Get[v1alpha1.CatalogSource](ctx, r.store, updateCenterSourceName, r.operatorNamespace)
	if err != nil && !apierrors.IsNotFound(err) {
		logger.Warn("failed to read the reserved catalog source", "error", err)
		return
	}
	if existing != nil {
		desired.ResourceVersion = existing.ResourceVersion
		// Preserve any labels the user added; ours win on collision.
		merged := map[string]string{}
		for k, v := range existing.Labels {
			merged[k] = v
		}
		for k, v := range desired.Labels {
			merged[k] = v
		}
		desired.Labels = merged
	}
	if err := crdstore.Apply[v1alpha1.CatalogSource](ctx, r.store, desired); err != nil {
		logger.Warn("failed to assert the reserved catalog source", "error", err)
	}
}

// ---------------------------------------------------------------------------
// Item derivation
// ---------------------------------------------------------------------------

// ucSlug lowercases s and reduces it to the DNS-label alphabet.
func ucSlug(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-':
			b.WriteRune(r)
			prevDash = r == '-'
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-.")
}

// ucItemName is a pure function of (name, version). The hash is ALWAYS
// appended, not only on overflow, so two versions of one plugin can never
// collide after slugging strips a distinguishing character such as '_'.
func ucItemName(pluginName, version string) string {
	h := sha256Hex([]byte(ucItemNameHashPrefix + pluginName + "@" + version))[:10]
	slug := "uc-" + ucSlug(pluginName) + "-" + ucSlug(version)
	if len(slug)+1+len(h) > 253 {
		slug = slug[:253-1-len(h)]
		slug = strings.TrimRight(slug, "-.")
	}
	return slug + "-" + h
}

// ucLabelValue returns a slug safe to use as a label value, or "" when it would
// be wrong. A wrong label is worse than an absent one: consumers select on
// these, and a lossy slug would silently match the wrong plugin.
func ucLabelValue(s string) string {
	slug := ucSlug(s)
	if slug == "" || slug != strings.ToLower(s) || len(slug) > 63 {
		return ""
	}
	return slug
}

// buildUCItem derives the CatalogItem shell for one stored (name, version).
// Status is filled in by the caller.
func buildUCItem(src *v1alpha1.CatalogSource, e inventoryEntry) *v1alpha1.CatalogItem {
	labels := map[string]string{
		catalogSourceLabel: src.Name,
		catalogTypeLabel:   string(v1alpha1.CatalogItemPlugin),
	}
	if v := ucLabelValue(e.Name); v != "" {
		labels[pluginNameLabel] = v
	}
	if v := ucLabelValue(e.Version); v != "" {
		labels[pluginVersionLabel] = v
	}

	display := e.DisplayName
	if display == "" {
		display = e.Name
	}

	tags := append([]string(nil), e.Tags...)
	if !containsString(tags, "update-center") {
		tags = append(tags, "update-center")
	}

	return &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{
			Name:            ucItemName(e.Name, e.Version),
			Namespace:       src.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRef(src)},
		},
		Spec: v1alpha1.CatalogItemSpec{
			SourceRef:   src.Name,
			Type:        v1alpha1.CatalogItemPlugin,
			DisplayName: display,
			Description: e.Description,
			// A provenance string, never a filesystem path: nothing in this arm
			// feeds it to ResolveContainedPath or deterministicName.
			Path:    fmt.Sprintf("uc://%s@%s", e.Name, e.Version),
			Version: e.Version,
			Tags:    tags,
		},
	}
}

// inventoryDigest is order-independent by construction, mirroring oci.LockHash.
//
// It covers every field of the entry, not just the identity triple. The
// metadata fields are what buildUCItem, resolveClosure and evaluateCompat read,
// so a pack republished with corrected dependencies, requiredCore or display
// metadata at an unchanged (name, version, sha256) still moves the digest.
// Narrowing this to the triple would make the skip gate in
// reconcileUpdateCenterSource silently assume those fields are functionally
// determined by the artifact hash.
func inventoryDigest(entries []inventoryEntry) string {
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		// Normalize dependency order first: a reordered listing is not new
		// content. e is a copy, but its slice header is shared, so clone.
		e.Dependencies = append([]pluginDep(nil), e.Dependencies...)
		sort.Slice(e.Dependencies, func(i, j int) bool {
			if e.Dependencies[i].Name != e.Dependencies[j].Name {
				return e.Dependencies[i].Name < e.Dependencies[j].Name
			}
			return e.Dependencies[i].Min < e.Dependencies[j].Min
		})
		// Encode structurally rather than joining fields with a separator:
		// a description or tag containing the separator could otherwise
		// impersonate a field boundary and let two different inventories
		// share a digest. This value decides whether a sync does any work at
		// all, so an ambiguous encoding is a missed update.
		encoded, err := json.Marshal(e)
		if err != nil {
			// Unreachable for this struct. Fail toward doing the work: an
			// empty digest never matches observedRevision, so the pass derives.
			return ""
		}
		lines = append(lines, string(encoded))
	}
	sort.Strings(lines)
	return sha256Hex([]byte(strings.Join(lines, "\n")))
}

// ucSyncDigest fingerprints every input the derived CatalogItems depend on.
//
// It must cover the profiles as well as the inventory: resolveClosure and
// evaluateCompat both read profiles, so a profile edit changes item content
// and compat verdicts while the inventory is untouched. Digesting the
// inventory alone would make such an edit invisible to the skip gate and
// leave every derived item stale until the next repair pass.
func ucSyncDigest(entries []inventoryEntry, skipped []ucSkippedPack, profiles []ucProfile) string {
	lines := make([]string, 0, 2+len(profiles))
	lines = append(lines, inventoryDigest(entries))
	// skippedPacks decides whether this pass prunes and what warning it
	// carries, so a pack becoming readable again is a change even when the
	// readable plugin set happens to be identical.
	refs := make([]string, 0, len(skipped))
	for _, sp := range skipped {
		refs = append(refs, sp.Ref)
	}
	sort.Strings(refs)
	lines = append(lines, "skipped:"+strings.Join(refs, ","))
	for _, p := range profiles {
		locks := make([]string, 0, len(p.Lock))
		for artifactID, version := range p.Lock {
			locks = append(locks, artifactID+"="+version)
		}
		sort.Strings(locks)
		lines = append(lines, fmt.Sprintf("%s|%s|%t|%s",
			p.Name, p.EffectiveCore, p.Eligible, strings.Join(locks, ",")))
	}
	// profiles arrives sorted by name from loadUCProfiles; sort again so the
	// digest cannot depend on that staying true.
	sort.Strings(lines[2:])
	return sha256Hex([]byte(strings.Join(lines, "\n")))
}

// ---------------------------------------------------------------------------
// Inventory and profile inputs
// ---------------------------------------------------------------------------

// ucSkippedPack names one plugin-pack manifest the update center could not
// read, as disclosed in its /api/v1/inventory response.
type ucSkippedPack struct {
	Ref   string `json:"ref"`
	Error string `json:"error"`
}

// ucInventoryResponse is the wire shape of GET /api/v1/inventory.
type ucInventoryResponse struct {
	Plugins      []inventoryEntry `json:"plugins"`
	SkippedPacks []ucSkippedPack  `json:"skippedPacks,omitempty"`
}

// fetchUCInventory GETs the update center's inventory. The second result names
// every plugin-pack the update center could not read.
//
// Every transport failure — timeout, or non-200 — must return before the prune
// step. Prune deletes every item not synced this pass, so pruning against an
// empty or partial inventory would delete the entire derived catalog on one
// transient 503 and break every ComposedBundle referencing it. A 200 that
// decodes to an empty "plugins" list is a legitimate empty store and DOES
// prune: the distinction is transport success, not payload size.
//
// A 200 with a non-empty SkippedPacks is different again: the update center
// itself is reachable and serving what it can, but "plugins" is a LOWER BOUND
// on store contents — any plugin held only by an unreadable pack is
// indistinguishable from having been deleted. The caller must treat that the
// same as a fetch error for prune purposes (see reconcileUpdateCenterSource),
// even though this function returns it as a nil error so the readable subset
// can still be synced.
func (r *CatalogReconciler) fetchUCInventory(ctx context.Context) ([]inventoryEntry, []ucSkippedPack, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.ucBaseURL+"/api/v1/inventory", nil)
	if err != nil {
		return nil, nil, fmt.Errorf("create inventory request: %w", err)
	}
	resp, err := r.ucHTTP.Do(req)
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

// loadUCProfiles reads every JenkinsVersionProfile and its lock.
//
// Eligibility requires BOTH PluginSetReady == True and a set status.contentRef:
// a profile whose plugin set is not ready may carry a stale contentRef, and a
// stale lock voting in the resolver's unanimity test manufactures agreement
// that does not exist. An ineligible profile is still returned — it yields an
// `unknown` verdict — but its lock is never read and never votes.
//
// A listing failure, or an eligible profile's lock failing to read or parse,
// aborts the caller before any write or prune. Resolving against a partial
// profile set could establish unanimity that does not exist and flip items
// valid or invalid on a transient API error.
//
// resolveVersion is READ here and nowhere written: version-profile resolution
// semantics belong to the profile controller.
func (r *CatalogReconciler) loadUCProfiles(ctx context.Context) ([]ucProfile, error) {
	profiles, err := crdstore.List[v1alpha1.JenkinsVersionProfile](ctx, r.store, "", "")
	if err != nil {
		return nil, fmt.Errorf("list JenkinsVersionProfiles: %w", err)
	}
	out := make([]ucProfile, 0, len(profiles))
	for _, p := range profiles {
		up := ucProfile{
			Name:          p.Name,
			EffectiveCore: p.Spec.Version,
		}
		if p.Spec.ResolveVersion != "" {
			up.EffectiveCore = p.Spec.ResolveVersion
		}
		up.Eligible = profileIsPluginSetReady(p) && p.Status.ContentRef != ""
		if !up.Eligible {
			out = append(out, up)
			continue
		}
		cmData, err := r.client.GetConfigMap(ctx, p.Status.ContentRef, r.operatorNamespace)
		if err != nil {
			return nil, fmt.Errorf("read lock ConfigMap %q for profile %q: %w", p.Status.ContentRef, p.Name, err)
		}
		pluginsYAML := cmData["plugins.yaml"]
		if pluginsYAML == "" {
			return nil, fmt.Errorf("lock ConfigMap %q for profile %q has no plugins.yaml", p.Status.ContentRef, p.Name)
		}
		var lockSet struct {
			Plugins []pluginlock.PluginEntry `yaml:"plugins"`
		}
		if err := yaml.Unmarshal([]byte(pluginsYAML), &lockSet); err != nil {
			return nil, fmt.Errorf("parse lock for profile %q: %w", p.Name, err)
		}
		up.Lock = make(map[string]string, len(lockSet.Plugins))
		for _, pe := range lockSet.Plugins {
			up.Lock[pe.ArtifactID] = pe.Version
		}
		out = append(out, up)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ---------------------------------------------------------------------------
// The update-center sync arm
// ---------------------------------------------------------------------------

// reconcileUpdateCenterSource derives one CatalogItem per stored
// (plugin, version) in the operator namespace.
//
// It shares the tail of the ordinary flow — apply, status patch, prune,
// terminal status — and replaces only the fetch and the per-item construction.
// It creates no work directory, clones nothing, and never calls
// bundle.ParseCatalogIndex.
func (r *CatalogReconciler) reconcileUpdateCenterSource(ctx context.Context, src *v1alpha1.CatalogSource) {
	if r.ucBaseURL == "" {
		r.setError(ctx, src, "update center is not enabled; no plugins to derive")
		return
	}

	entries, skipped, err := r.fetchUCInventory(ctx)
	if err != nil {
		r.setError(ctx, src, fmt.Sprintf("read update-center inventory: %v", err))
		return
	}
	profiles, err := r.loadUCProfiles(ctx)
	if err != nil {
		r.setError(ctx, src, fmt.Sprintf("read version profiles: %v", err))
		return
	}

	rev := ucSyncDigest(entries, skipped, profiles)

	// Deriving items is this arm's entire cost: one apply plus one status
	// patch per plugin, every pass, through the single dynamic client that
	// carries all of the operator's CRD traffic. At update-center scale that
	// is hundreds of writes per sync, and it rewrites byte-identical items
	// whenever nothing upstream moved. Skipping that is what keeps the catalog
	// tick short enough that ComposedBundle reconciliation, which runs behind
	// every source in the same tick, is not starved.
	if r.ucSyncUnchanged(src, rev) {
		r.markUCUnchanged(ctx, src, rev)
		return
	}

	inv := newUCInventory(entries)

	desired := make(map[string]bool)
	var warnings []string
	conflicts := 0
	writeFailed := false

	for _, key := range sortedKeys(inv.entries) {
		group := inv.entries[key]
		e := group[0]
		item := buildUCItem(src, e)
		item.Status.ObservedRevision = rev

		switch {
		case len(group) > 1:
			// The store reports more than one canonical entry for this exact
			// (name, version), so it cannot be resolved to bytes. Handling is
			// per plugin: only this item is invalidated, every other plugin
			// derives normally, and the source stays Ready — one bad plugin
			// never takes the catalog offline.
			conflicts++
			item.Status.Valid = false
			item.Status.Message = fmt.Sprintf(
				"the store reports %d conflicting entries for %s@%s; a fixed version's bytes and metadata must never differ",
				len(group), e.Name, e.Version)
		default:
			cl, resErr := resolveClosure(e, inv, profiles)
			if resErr != nil {
				item.Status.Valid = false
				item.Status.Message = resErr.Error()
				break
			}
			content := closureContent(cl)
			item.Status.Valid = true
			item.Status.Content = content
			item.Status.ContentHash = sha256Hex([]byte(content))
			item.Status.Closure = closureStatus(cl)
			compat := evaluateCompat(cl, inv, profiles)
			item.Status.Compat = compat
			item.Status.Conditions = []v1alpha1.TemplateCatalogCondition{compatWarning(compat)}
		}

		itemWarnings, failed := r.writeCatalogItem(ctx, src, item, desired)
		warnings = append(warnings, itemWarnings...)
		if failed {
			writeFailed = true
		}
	}

	// A non-empty skipped means "plugins" was a lower bound, not a full
	// listing: pruning against it would delete every item backed solely by an
	// unreadable pack. Withhold prune for this pass instead — the readable
	// subset still syncs above, so one bad pack degrades coverage rather than
	// taking the whole catalog offline (compare the per-item "conflicts"
	// handling above: one bad entry never fails the rest of the sync).
	if len(skipped) > 0 {
		refs := make([]string, len(skipped))
		for i, sp := range skipped {
			refs[i] = sp.Ref
		}
		warnings = append(warnings, fmt.Sprintf(
			"%d plugin-pack manifest(s) could not be read and were excluded from this sync "+
				"(pruning skipped to avoid deleting items they may back): %s",
			len(skipped), strings.Join(refs, ", ")))
	} else if !r.pruneCatalogItems(ctx, src, desired) {
		// An incomplete GC leaves a stale item selectable; retry next tick
		// rather than waiting for the repair pass.
		writeFailed = true
	}

	if conflicts > 0 {
		warnings = append(warnings, fmt.Sprintf("%d plugin(s) have conflicting store entries and derived no content", conflicts))
	}
	// Arm the skip gate only for a pass that actually landed every item. A
	// failed write is retried on the next tick, as it was before the gate
	// existed; arming here would defer that retry to the repair pass.
	if writeFailed {
		// Disarm rather than merely decline to arm: setReady is about to
		// record this revision's digest, so an earlier revision's timestamp
		// left standing would let the next pass skip on a digest that was
		// never fully applied.
		r.ucLastFullPass = time.Time{}
		r.ucLastFullPassMessage = ""
	} else {
		r.ucLastFullPass = time.Now()
		r.ucLastFullPassMessage = joinWarnings(warnings)
	}
	r.setReady(ctx, src, rev, len(desired), warnings)
}

// ucSyncUnchanged reports whether this pass can skip deriving every item.
//
// The skip is safe only when the digest matches what the last *successful*
// pass recorded, which is exactly what status.observedRevision holds: setReady
// is the sole writer, and the error paths in this arm all return before any
// item is written, so an errored pass leaves the previous digest in place
// rather than a half-applied one.
//
// The digest cannot see items deleted or edited out of band, so it is paired
// with ucFullPassInterval: steady state costs one inventory fetch, and any
// drift is still repaired within that bound.
func (r *CatalogReconciler) ucSyncUnchanged(src *v1alpha1.CatalogSource, digest string) bool {
	if digest == "" || src.Status.ObservedRevision != digest {
		return false
	}
	// An explicitly requested sync always derives: it is the operator's lever
	// for exactly the out-of-band drift the digest is blind to.
	if manualSyncRequested(src) {
		return false
	}
	if r.ucLastFullPass.IsZero() {
		return false
	}
	return time.Since(r.ucLastFullPass) < ucFullPassInterval
}

// markUCUnchanged advances lastSyncTime for a skipped pass without touching
// itemCount or message, which still describe the last pass that derived items.
// Advancing the timestamp matters: isSyncDue keys off it, so leaving it stale
// would re-enter this arm on every 15s tick and re-fetch the inventory each
// time, which is most of what the skip is meant to avoid.
func (r *CatalogReconciler) markUCUnchanged(ctx context.Context, src *v1alpha1.CatalogSource, digest string) {
	now := metav1.Now()
	// Replay the last deriving pass's message rather than whatever is on src:
	// this pass derived nothing, and src may carry a setError text that this
	// successful fetch just superseded.
	src.Status.Message = r.ucLastFullPassMessage
	src.Status.Phase = v1alpha1.CatalogSyncReady
	src.Status.ObservedRevision = digest
	src.Status.LastSyncTime = &now
	if err := crdstore.PatchStatus[v1alpha1.CatalogSource](ctx, r.store, src.Name, src.Namespace, &src.Status); err != nil {
		r.logger.Error("failed to patch catalog source status for an unchanged update-center sync",
			"source", src.Name, "namespace", src.Namespace, "error", err)
	}
}

// ---------------------------------------------------------------------------
// Shared write / prune / terminal-status discipline
// ---------------------------------------------------------------------------

// itemOwnedBy is THE ownership predicate, used identically by the write guard
// and the prune step. Two predicates would guarantee they eventually disagree.
//
// Both conjuncts are required. The label alone is user-writable and is what the
// prune selector already keys on; the ownerRef UID alone survives a label edit
// but not a delete-and-recreate of the source.
func itemOwnedBy(labels map[string]string, refs []metav1.OwnerReference, src *v1alpha1.CatalogSource) bool {
	if labels[catalogSourceLabel] != src.Name {
		return false
	}
	for _, ref := range refs {
		if ref.Controller != nil && *ref.Controller && ref.UID == src.UID {
			return true
		}
	}
	return false
}

// writeCatalogItem applies one item under the ownership guard and records it in
// desired.
//
// desired is recorded BEFORE the write attempt, not after it. Keying prune on
// write success made a transient apply error delete that very item in the same
// pass; keying on intent fixes that, and it is a prerequisite for the guard —
// a foreign item that was skipped must not be deleted either.
// It reports warnings for the caller's status message, and whether the write
// failed in a way worth retrying soon. The update-center arm uses that second
// return to decide whether its skip gate may arm: arming after a failed write
// would defer the retry to the repair pass instead of the next tick.
func (r *CatalogReconciler) writeCatalogItem(ctx context.Context, src *v1alpha1.CatalogSource, item *v1alpha1.CatalogItem, desired map[string]bool) ([]string, bool) {
	desired[item.Name] = true

	status := item.Status
	err := crdstore.ApplyOwned(ctx, r.store, item, func(live *unstructured.Unstructured) bool {
		return itemOwnedBy(live.GetLabels(), live.GetOwnerReferences(), src)
	})
	if errors.Is(err, crdstore.ErrNotOwned) {
		r.logger.Warn("skipping catalog item owned by another source",
			"source", src.Name, "item", item.Name)
		// Not a failed write: another source owns this name and will keep
		// owning it, so retrying sooner changes nothing.
		return []string{fmt.Sprintf("item %s is owned by another source; skipped", item.Name)}, false
	}
	if err != nil {
		r.logger.Error("failed to apply catalog item",
			"source", src.Name, "item", item.Name, "error", err)
		return []string{fmt.Sprintf("item %s failed to apply: %v", item.Name, err)}, true
	}
	// PatchStatus needs no ownership guard: it runs only immediately after a
	// successful ApplyOwned, so ownership was just established against the live
	// object, and it is a status-subresource merge patch rather than a
	// full-object replace, so it cannot resurrect or reshape a foreign spec.
	if err := crdstore.PatchStatus[v1alpha1.CatalogItem](ctx, r.store, item.Name, item.Namespace, &status); err != nil {
		r.logger.Warn("failed to patch catalog item status",
			"source", src.Name, "item", item.Name, "error", err)
		return []string{fmt.Sprintf("item %s status patch failed: %v", item.Name, err)}, true
	}
	return nil, false
}

// pruneCatalogItems deletes items this pass did not desire. It re-checks
// ownership even though the list is already label-selected, because the label
// is the weaker of the two conjuncts.
//
// It reports whether the pass completed. A listing or delete failure leaves a
// stale item selectable, which the update-center arm treats like a failed
// write: retry on the next tick rather than at the repair interval.
func (r *CatalogReconciler) pruneCatalogItems(ctx context.Context, src *v1alpha1.CatalogSource, desired map[string]bool) bool {
	labelSelector := fmt.Sprintf("%s=%s", catalogSourceLabel, src.Name)
	existing, err := crdstore.List[v1alpha1.CatalogItem](ctx, r.store, src.Namespace, labelSelector)
	if err != nil {
		r.logger.Error("failed to list catalog items for GC",
			"source", src.Name, "namespace", src.Namespace, "error", err)
		return false
	}
	complete := true
	for _, item := range existing {
		if !itemOwnedBy(item.Labels, item.OwnerReferences, src) {
			continue // never delete what we do not own
		}
		if desired[item.Name] {
			continue
		}
		if err := crdstore.Delete[v1alpha1.CatalogItem](ctx, r.store, item.Name, src.Namespace); err != nil {
			r.logger.Error("failed to delete stale catalog item",
				"source", src.Name, "item", item.Name, "error", err)
			complete = false
		}
	}
	return complete
}

// setReady writes the terminal status.
//
// The message carries the pass's warnings — ownership skips and store integrity
// failures — rather than being unconditionally cleared, which would erase them.
// The phase stays Ready: neither is a sync failure.
func (r *CatalogReconciler) setReady(ctx context.Context, src *v1alpha1.CatalogSource, rev string, itemCount int, warnings []string) {
	now := metav1.Now()
	src.Status.Phase = v1alpha1.CatalogSyncReady
	src.Status.ObservedRevision = rev
	src.Status.LastSyncTime = &now
	src.Status.ItemCount = itemCount
	src.Status.Message = joinWarnings(warnings)

	if err := crdstore.PatchStatus[v1alpha1.CatalogSource](ctx, r.store, src.Name, src.Namespace, &src.Status); err != nil {
		r.logger.Error("failed to patch catalog source status to ready",
			"source", src.Name, "namespace", src.Namespace, "error", err)
	}
}

// joinWarnings renders at most maxSourceWarnings warnings, counting the rest.
func joinWarnings(warnings []string) string {
	if len(warnings) == 0 {
		return ""
	}
	shown := warnings
	suffix := ""
	if len(shown) > maxSourceWarnings {
		shown = shown[:maxSourceWarnings]
		suffix = fmt.Sprintf(" (+%d more)", len(warnings)-maxSourceWarnings)
	}
	return strings.Join(shown, "; ") + suffix
}
