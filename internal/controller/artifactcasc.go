package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
)

// ConditionArtifactStorage is the Controller condition type task 5.6 adds,
// with reasons "External", "LocalOnly", and "PluginMissing". It is defined
// here rather than alongside the other ControllerConditionType constants in
// api/v1alpha1/types.go, because this batch's file scope excludes
// api/v1alpha1/**. ControllerConditionType is a plain string type, so this
// works, but it breaks that file's single-source-of-truth convention for
// condition types; flagged for the coordinator to relocate it.
const ConditionArtifactStorage v1alpha1.ControllerConditionType = "ArtifactStorage"

// artifactCredentialID is the id of the system-scoped AWS credential the
// artifact-storage fragment declares and injectArtifactCredential supplies.
const artifactCredentialID = "varroa-artifact-store"

// artifactSecretMountPath is where CreateStatefulSet mounts the projected
// artifact Secret on the Jenkins container. The fragment's credential entry
// reads its two keys from here with ${readFile:...} references.
const artifactSecretMountPath = "/run/secrets/varroa-artifacts"

// blobstoreParams is what parseBlobstoreURL derives from VARROA_BLOBSTORE_URL
// for the artifact-storage fragment (task 5.1, design D8.3).
type blobstoreParams struct {
	// IsS3 is false for a pgblob: URL (or any non-"s3" scheme): the record
	// store uses the Postgres blob tier, and the operator emits no fragment.
	IsS3            bool
	Container       string
	CustomEndpoint  string // host[:port]; empty selects AWS itself.
	Region          string
	UseHTTP         bool
	UsePathStyleURL bool
}

// parseBlobstoreURL parses VARROA_BLOBSTORE_URL into the values the
// artifact-storage fragment needs. It returns ok=false only when raw fails
// to parse as a URL at all or an s3 URL carries no bucket host; a pgblob:
// URL (or any other non-s3 scheme) parses fine and yields IsS3=false. The
// host is the bucket. region, endpoint, and s3ForcePathStyle are query
// parameters, the gocloud.dev s3blob URL form the record store already
// uses. customEndpoint is host[:port], not a URL: S3BlobStoreConfig
// prepends the protocol itself and selects it from useHttp, so the scheme
// is stripped from the endpoint parameter here.
func parseBlobstoreURL(raw string) (blobstoreParams, bool) {
	if strings.TrimSpace(raw) == "" {
		return blobstoreParams{}, false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return blobstoreParams{}, false
	}
	if u.Scheme != "s3" {
		return blobstoreParams{}, true
	}
	if u.Host == "" {
		return blobstoreParams{}, false
	}
	q := u.Query()
	p := blobstoreParams{
		IsS3:      true,
		Container: u.Host,
		Region:    q.Get("region"),
	}
	if ep := q.Get("endpoint"); ep != "" {
		epURL, epErr := url.Parse(ep)
		if epErr != nil || epURL.Host == "" {
			return blobstoreParams{}, false
		}
		p.CustomEndpoint = epURL.Host
		p.UseHTTP = epURL.Scheme == "http"
	}
	if v, err := strconv.ParseBool(q.Get("s3ForcePathStyle")); err == nil {
		p.UsePathStyleURL = v
	}
	return p, true
}

// artifactManagerPrefix renders the per-cell object prefix Contract D fixes:
// artifacts/<cluster>/<namespace>/<cell>/. It always ends with a slash,
// because the plugin's doCheckPrefix refuses a non-blank prefix that
// doesn't (task 5.3b).
func artifactManagerPrefix(cluster, namespace, cell string) string {
	return fmt.Sprintf("artifacts/%s/%s/%s/", cluster, namespace, cell)
}

// renderArtifactStorageFragment builds the operator-owned artifact-storage
// JCasC fragment: the jclouds/s3 factory selection and the aws root's
// awsCredentials and s3 settings (task 0.5's export; the keys live under
// "aws", not "unclassified"). It carries no secret value. In static mode
// injectArtifactCredential supplies the credential reference separately.
// In STS mode (sts true) the fragment names no credential and keeps session
// tokens enabled: the plugin then reads the AWS SDK default chain, which
// reads the shared credentials file the operator renews in place.
func renderArtifactStorageFragment(p blobstoreParams, cluster, namespace, cell string, sts bool) (string, error) {
	s3Section := map[string]any{
		"container":           p.Container,
		"prefix":              artifactManagerPrefix(cluster, namespace, cell),
		"usePathStyleUrl":     p.UsePathStyleURL,
		"useHttp":             p.UseHTTP,
		"disableSessionToken": !sts,
	}
	if p.CustomEndpoint != "" {
		s3Section["customEndpoint"] = p.CustomEndpoint
	}

	credentialsID := artifactCredentialID
	if sts {
		credentialsID = ""
	}
	doc := map[string]any{
		"aws": map[string]any{
			"awsCredentials": map[string]any{
				"credentialsId": credentialsID,
				"region":        p.Region,
			},
			"s3": s3Section,
		},
		"unclassified": map[string]any{
			"artifactManager": map[string]any{
				"artifactManagerFactories": []any{
					map[string]any{
						"jclouds": map[string]any{
							"provider": "s3",
						},
					},
				},
			},
		},
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal artifact-storage fragment: %w", err)
	}
	return string(out), nil
}

// artifactManagerStripPaths are the three subtrees stripArtifactManager
// removes from a merged jenkins.yaml. Derived from task 0.6's
// ArtifactManagerJCasCKeys: every key under "unclassified.artifactManager"
// collapses to one delete point (the factories list), and every "aws.*" key
// collapses to two (the awsCredentials block and the s3 block) — the only
// two children the aws-global-configuration plugin's "aws" category has.
var artifactManagerStripPaths = [][]string{
	{"unclassified", "artifactManager", "artifactManagerFactories"},
	{"aws", "awsCredentials"},
	{"aws", "s3"},
}

// stripArtifactManager removes the artifact-storage keys from a merged JCasC
// YAML document, whether the bundle authored them or the version-profile
// overlay did. It mirrors stripAuthorizationStrategy. One
// configuration-as-code export (task 0.5) decided the key set; task 0.6
// holds it in ArtifactManagerJCasCKeys, and artifactManagerStripPaths above
// is the same set collapsed to its three delete points.
func stripArtifactManager(yamlDoc string) string {
	if yamlDoc == "" {
		return ""
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(yamlDoc), &doc); err != nil {
		return yamlDoc
	}
	changed := false
	for _, path := range artifactManagerStripPaths {
		if deleteYAMLPath(doc, path) {
			changed = true
		}
	}
	if !changed {
		return yamlDoc
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return yamlDoc
	}
	return string(out)
}

// deleteYAMLPath removes the map entry named by path's last element,
// walking the earlier elements as nested maps. It cascades the deletion
// upward through any parent map that becomes empty as a result, stopping at
// (and never deleting) the document root. Returns true when it removed
// anything.
func deleteYAMLPath(doc map[string]any, path []string) bool {
	if len(path) == 0 {
		return false
	}
	parents := make([]map[string]any, 0, len(path)-1)
	cur := doc
	for _, seg := range path[:len(path)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			return false // the path does not exist; nothing to strip.
		}
		parents = append(parents, cur)
		cur = next
	}
	leaf := path[len(path)-1]
	if _, has := cur[leaf]; !has {
		return false
	}
	delete(cur, leaf)
	for i := len(parents) - 1; i >= 0; i-- {
		parent := parents[i]
		key := path[i]
		child, _ := parent[key].(map[string]any)
		if len(child) != 0 {
			break
		}
		delete(parent, key)
	}
	return true
}

// injectArtifactCredential parses the merged jenkins.yaml, removes any
// existing credential entry with id artifactCredentialID from every domain
// in credentials.system.domainCredentials[].credentials[], then appends the
// operator's own entry (a JCasC secret-source reference, never a value) to
// the global domain — the domainCredentials entry with no "domain" key,
// created when absent. MergeJenkinsYAML replaces a list wholesale on
// conflict, so this list-aware edit runs on the already-parsed document
// instead (design D6).
func injectArtifactCredential(yamlDoc string) (string, error) {
	var doc map[string]any
	if strings.TrimSpace(yamlDoc) != "" {
		if err := yaml.Unmarshal([]byte(yamlDoc), &doc); err != nil {
			return "", fmt.Errorf("parse jcasc for artifact credential injection: %w", err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}

	credentials, _ := doc["credentials"].(map[string]any)
	if credentials == nil {
		credentials = map[string]any{}
		doc["credentials"] = credentials
	}
	system, _ := credentials["system"].(map[string]any)
	if system == nil {
		system = map[string]any{}
		credentials["system"] = system
	}
	domainCredentials, _ := system["domainCredentials"].([]any)

	for _, d := range domainCredentials {
		dm, ok := d.(map[string]any)
		if !ok {
			continue
		}
		creds, _ := dm["credentials"].([]any)
		dm["credentials"] = removeCredentialByID(creds, artifactCredentialID)
	}

	var globalDomain map[string]any
	for _, d := range domainCredentials {
		dm, ok := d.(map[string]any)
		if !ok {
			continue
		}
		if _, hasDomain := dm["domain"]; !hasDomain {
			globalDomain = dm
			break
		}
	}
	if globalDomain == nil {
		globalDomain = map[string]any{"credentials": []any{}}
		domainCredentials = append(domainCredentials, globalDomain)
	}

	creds, _ := globalDomain["credentials"].([]any)
	creds = append(creds, map[string]any{
		"aws": map[string]any{
			"scope":     "SYSTEM",
			"id":        artifactCredentialID,
			"accessKey": fmt.Sprintf("${readFile:%s/access-key-id}", artifactSecretMountPath),
			"secretKey": fmt.Sprintf("${readFile:%s/secret-access-key}", artifactSecretMountPath),
		},
	})
	globalDomain["credentials"] = creds
	system["domainCredentials"] = domainCredentials

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal jcasc after artifact credential injection: %w", err)
	}
	return string(out), nil
}

// removeCredentialByID drops every entry from creds whose single credential
// map (aws, usernamePassword, etc.) carries the given id, preserving every
// other entry and its position.
func removeCredentialByID(creds []any, id string) []any {
	var out []any
	for _, c := range creds {
		cm, ok := c.(map[string]any)
		if !ok {
			out = append(out, c)
			continue
		}
		match := false
		for _, v := range cm {
			vm, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if idVal, _ := vm["id"].(string); idVal == id {
				match = true
				break
			}
		}
		if !match {
			out = append(out, c)
		}
	}
	return out
}

// artifactStorageCondition builds the ArtifactStorage condition with the
// given status/reason/message (task 5.6).
func artifactStorageCondition(status metav1.ConditionStatus, reason, message string) v1alpha1.ControllerCondition {
	return v1alpha1.ControllerCondition{
		Type:    ConditionArtifactStorage,
		Status:  status,
		Reason:  reason,
		Message: message,
	}
}

// pluginPinned reports whether the resolved core plugin set pins
// artifactID.
func pluginPinned(entries []pluginlock.PluginEntry, artifactID string) bool {
	for _, e := range entries {
		if e.ArtifactID == artifactID {
			return true
		}
	}
	return false
}

// artifactSecretCandidates returns cr's artifact Secret names in lookup
// order: the per-controller name first, then the pool name for a cell that
// carries a pool label. The bare base name never appears (design D2).
func (r *Reconciler) artifactSecretCandidates(cr *v1alpha1.Controller) []string {
	candidates := []string{r.artifactSecretBase + "-" + cr.Name}
	if v1alpha1.IsCell(cr) {
		if pool := cr.Labels[cells.LabelCellPool]; pool != "" {
			candidates = append(candidates, r.artifactSecretBase+"-"+pool)
		}
	}
	return candidates
}

// lookupArtifactSecret reads each candidate in the operator namespace, in
// order, and returns the first with non-empty data (design D1). A NotFound
// answer moves to the next candidate. Any other error stops the lookup at
// once and returns that candidate's name with the error. An exhausted list
// returns an empty name and a nil error.
func (r *Reconciler) lookupArtifactSecret(ctx context.Context, candidates []string) (name string, data map[string][]byte, err error) {
	for _, candidate := range candidates {
		d, getErr := r.client.GetSecret(ctx, candidate, r.getOperatorNamespace())
		if getErr != nil {
			if apierrors.IsNotFound(getErr) {
				continue
			}
			return candidate, nil, getErr
		}
		if len(d) == 0 {
			continue
		}
		return candidate, d, nil
	}
	return "", nil, nil
}

// quotedCandidates renders a candidate list as quoted, comma-separated
// names, in lookup order, for the LocalOnly exhausted-list message.
func quotedCandidates(candidates []string) string {
	quoted := make([]string, len(candidates))
	for i, c := range candidates {
		quoted[i] = strconv.Quote(c)
	}
	return strings.Join(quoted, ", ")
}

// errArtifactSecretRead marks an artifact Secret read fault. The sentinel
// lets handleProvisioning tell a Secret read fault from a bundle fault, so
// it can mark the reconcile blocked instead of waiting on the bundle.
var errArtifactSecretRead = errors.New("artifact secret read failed")

// artifactStorageFragment renders the artifact-storage JCasC fragment for cr
// together with the ArtifactStorage condition to report alongside it
// (task 5.3). It emits an empty fragment — and a False condition — unless
// all four conditions hold (design D6, "When the operator emits nothing"):
// the blob mode is S3, a candidate artifact Secret exists (design D1), the
// resolved profile pins artifact-manager-s3, and VARROA_BLOBSTORE_URL parses.
// Every return carries a nil error except a candidate read fault, which
// wraps errArtifactSecretRead so the caller can tell it apart from an
// ordinary "nothing configured" LocalOnly case.
func (r *Reconciler) artifactStorageFragment(ctx context.Context, cr *v1alpha1.Controller) (string, v1alpha1.ControllerCondition, error) {
	params, parsed := parseBlobstoreURL(r.blobstoreURL)
	if !parsed {
		if r.Logger != nil {
			r.Logger.Warn("artifact storage: VARROA_BLOBSTORE_URL does not parse", "url", r.blobstoreURL)
		}
		return "", artifactStorageCondition(metav1.ConditionFalse, "LocalOnly",
			fmt.Sprintf("VARROA_BLOBSTORE_URL %q does not parse", r.blobstoreURL)), nil
	}
	if !params.IsS3 {
		return "", artifactStorageCondition(metav1.ConditionFalse, "LocalOnly",
			"record-store blob mode is not S3 (artifacts stay in the cell's local storage)"), nil
	}

	if r.artifactSTS.Enabled() {
		return r.artifactSTSFragment(ctx, cr, params)
	}

	if r.artifactSecretBase == "" {
		return "", artifactStorageCondition(metav1.ConditionFalse, "LocalOnly",
			"recordStore.blobs.s3.artifactSecret is not configured"), nil
	}
	candidates := r.artifactSecretCandidates(cr)
	sourceName, _, lookupErr := r.lookupArtifactSecret(ctx, candidates)
	if lookupErr != nil {
		cond := artifactStorageCondition(metav1.ConditionFalse, "LocalOnly",
			fmt.Sprintf("artifact secret %q read failed: %v", sourceName, lookupErr))
		return "", cond, fmt.Errorf("%w: secret %q: %v", errArtifactSecretRead, sourceName, lookupErr)
	}
	if sourceName == "" {
		return "", artifactStorageCondition(metav1.ConditionFalse, "LocalOnly",
			fmt.Sprintf("artifact secret not found: tried %s", quotedCandidates(candidates))), nil
	}

	coreSet := r.coreSetForCr(ctx, cr, r.Logger)
	if !pluginPinned(coreSet, "artifact-manager-s3") {
		return "", artifactStorageCondition(metav1.ConditionFalse, "PluginMissing",
			"resolved plugin set does not pin artifact-manager-s3"), nil
	}

	fragment, err := renderArtifactStorageFragment(params, r.clusterName, cr.Namespace, cr.Name, false)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("artifact storage: failed to render fragment", "controller", cr.Namespace+"/"+cr.Name, "error", err)
		}
		return "", artifactStorageCondition(metav1.ConditionFalse, "LocalOnly", "failed to render artifact-storage fragment"), nil
	}

	prefix := artifactManagerPrefix(r.clusterName, cr.Namespace, cr.Name)
	return fragment, artifactStorageCondition(metav1.ConditionTrue, "External",
		fmt.Sprintf("artifacts write to bucket %q under prefix %q with Secret %q", params.Container, prefix, sourceName)), nil
}

// BlobstoreIsS3 reports whether raw parses to an s3: blob URL. The cells
// package cannot import parseBlobstoreURL, so the pool fill gate consumes
// this one bool instead (design D4).
func BlobstoreIsS3(raw string) bool {
	params, parsed := parseBlobstoreURL(raw)
	return parsed && params.IsS3
}

// VersionPinsPlugin reports whether the plugin set a controller running
// version would resolve pins artifactID. The pool fill gate calls this
// before it creates a cell, resolving by version rather than by the pool's
// own profileRef, because a cell never resolves by profile name (design D4).
func (r *Reconciler) VersionPinsPlugin(ctx context.Context, version, artifactID string) bool {
	cr := &v1alpha1.Controller{Spec: v1alpha1.ControllerSpec{Version: version}}
	coreSet := r.coreSetForCr(ctx, cr, r.Logger)
	return pluginPinned(coreSet, artifactID)
}

// applyArtifactStorage applies design D6's artifact-storage merge step to an
// already profile-overlaid jenkins.yaml: strip, merge the fragment, inject
// the credential. Per the version-jcasc-overlay delta ("The artifact-storage
// step SHALL be a no-op when the operator emits no fragment"), the whole
// step — including the strip — is skipped when artifactStorageFragment
// returns no fragment, so yamlDoc passes through unchanged in that case
// (task 5.3a).
func (r *Reconciler) applyArtifactStorage(ctx context.Context, cr *v1alpha1.Controller, yamlDoc string) (string, v1alpha1.ControllerCondition, error) {
	fragment, cond, fragmentErr := r.artifactStorageFragment(ctx, cr)
	if fragment == "" {
		return yamlDoc, cond, fragmentErr
	}
	stripped := stripArtifactManager(yamlDoc)
	merged, err := bundle.MergeJenkinsYAML(stripped, fragment)
	if err != nil {
		return yamlDoc, cond, fmt.Errorf("merge artifact-storage fragment: %w", err)
	}
	if r.artifactSTS.Enabled() {
		return merged, cond, nil
	}
	withCred, err := injectArtifactCredential(merged)
	if err != nil {
		return yamlDoc, cond, fmt.Errorf("inject artifact credential: %w", err)
	}
	return withCred, cond, nil
}

// controllerOwnerRef returns an OwnerReference pointing at cr, so a
// projected artifact Secret is garbage-collected when the Controller is
// deleted.
func controllerOwnerRef(cr *v1alpha1.Controller) metav1.OwnerReference {
	controllerFlag := true
	apiVersion := cr.APIVersion
	kind := cr.Kind
	if apiVersion == "" {
		apiVersion = v1alpha1.SchemeGroupVersion.String()
	}
	if kind == "" {
		kind = "Controller"
	}
	return metav1.OwnerReference{
		APIVersion:         apiVersion,
		Kind:               kind,
		Name:               cr.Name,
		UID:                cr.UID,
		BlockOwnerDeletion: &controllerFlag,
		Controller:         &controllerFlag,
	}
}

// secretWithOwnerCreator is implemented by ResourceClient backends that can
// stamp an owner reference on a Secret (task 5.1a). It is checked with a
// type assertion rather than added to the ResourceClient interface: that
// interface has explicit-method fakes outside this package's file scope
// (e.g. internal/api's handler tests), and every one would need a matching
// stub the moment this package added a new required method.
type secretWithOwnerCreator interface {
	CreateOrUpdateSecretWithOwner(ctx context.Context, name, namespace string, data map[string][]byte, owner metav1.OwnerReference) error
}

// resolveArtifactSecret resolves and projects a cell's artifact Secret
// (tasks 5.1a, 5.1b, design D1). It reads the candidate list (design D2)
// from the release namespace and, when a candidate is found, projects a
// copy named "<controller>-artifacts" into the controller's namespace,
// owned by the Controller. The copy is rewritten only when the source data
// differs, and dataHash is a content hash of that data so the caller can
// fold a rotation into the controller's desired-state hash. It returns an
// empty name and a nil error — with no I/O beyond the one read — when
// VARROA_ARTIFACT_SECRET is unset or every candidate is absent. Varroa
// projects no fleet-wide fallback: an exhausted candidate list is exactly
// the caller's cue to report ArtifactStorage/LocalOnly. A non-nil error
// means the lookup or the projection write failed; the caller must not
// build a pod while the projection may be stale or missing. In STS mode
// resolveArtifactSTSSecret issues the credential instead, and the data hash
// stays empty.
func (r *Reconciler) resolveArtifactSecret(ctx context.Context, cr *v1alpha1.Controller) (projectedName, dataHash string, err error) {
	if r.artifactSTS.Enabled() {
		name, stsErr := r.resolveArtifactSTSSecret(ctx, cr)
		return name, "", stsErr
	}
	if r.artifactSecretBase == "" {
		return "", "", nil
	}
	candidates := r.artifactSecretCandidates(cr)
	sourceName, data, lookupErr := r.lookupArtifactSecret(ctx, candidates)
	if lookupErr != nil {
		return "", "", fmt.Errorf("read artifact secret %q: %w", sourceName, lookupErr)
	}
	if sourceName == "" {
		return "", "", nil
	}
	hash := secretDataHash(data)
	projectedName = cr.Name + "-artifacts"
	owner := controllerOwnerRef(cr)
	var writeErr error
	if setter, ok := r.client.(secretWithOwnerCreator); ok {
		writeErr = setter.CreateOrUpdateSecretWithOwner(ctx, projectedName, cr.Namespace, data, owner)
	} else {
		writeErr = r.client.CreateOrUpdateSecret(ctx, projectedName, cr.Namespace, data)
	}
	if writeErr != nil {
		if r.Logger != nil {
			r.Logger.Warn("failed to project artifact secret", "controller", cr.Namespace+"/"+cr.Name, "source", sourceName, "error", writeErr)
		}
		return "", "", fmt.Errorf("project artifact secret %q: %w", sourceName, writeErr)
	}
	return projectedName, hash, nil
}

// artifactSecretHashReader is implemented by ResourceClient backends that
// can read the varroa.dev/artifact-secret-hash pod-template annotation of
// the live StatefulSet. It is a type-assertion seam for the same reason as
// secretWithOwnerCreator: the ResourceClient interface has explicit-method
// fakes outside this package.
type artifactSecretHashReader interface {
	GetStatefulSetArtifactSecretHash(ctx context.Context, name, namespace string) (hash string, found bool, err error)
}

// reconcileArtifactSecretRoll compares the content hash of a cell's
// artifact Secret (design D1's candidate list) with the
// varroa.dev/artifact-secret-hash annotation on the live StatefulSet. On a
// difference it transitions a Connected controller to Provisioning, which
// re-projects the copy and re-renders the pod template with the new hash,
// so the rotated credential reaches the running cell. It returns true when
// it transitioned the phase, and the caller then skips the normal phase
// dispatch this tick. An exhausted candidate list, a missing StatefulSet,
// or a backend without the reader seam is a no-op: an absent Secret is the
// LocalOnly case, not a rotation. A read error logs a warning and skips the
// tick, never rolling on a fault. In STS mode a renewal never rolls the
// pod; reconcileArtifactSTSRoll rolls only a pod that predates STS mode.
func (r *Reconciler) reconcileArtifactSecretRoll(ctx context.Context, cr *v1alpha1.Controller) bool {
	if r.artifactSTS.Enabled() {
		return r.reconcileArtifactSTSRoll(ctx, cr)
	}
	if r.artifactSecretBase == "" {
		return false
	}
	reader, ok := r.client.(artifactSecretHashReader)
	if !ok {
		return false
	}
	candidates := r.artifactSecretCandidates(cr)
	sourceName, data, lookupErr := r.lookupArtifactSecret(ctx, candidates)
	if lookupErr != nil {
		if r.Logger != nil {
			r.Logger.Warn("artifact-secret-roll evaluation skipped", "controller", cr.Namespace+"/"+cr.Name, "secret", sourceName, "error", lookupErr)
		}
		return false
	}
	if sourceName == "" {
		return false
	}
	desired := secretDataHash(data)
	live, found, err := reader.GetStatefulSetArtifactSecretHash(ctx, controllerPrefix(cr), cr.Namespace)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("artifact-secret-roll evaluation skipped", "controller", cr.Namespace+"/"+cr.Name, "error", err)
		}
		return false
	}
	if !found || live == desired {
		return false
	}
	now := metav1Now()
	cr.Status.Phase = v1alpha1.ControllerPhaseProvisioning
	cr.Status.ProvisioningStartedAt = &now
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:               v1alpha1.ConditionProvisioning,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "ProvisioningStarted",
		Message:            "re-provisioning to apply a rotated artifact secret",
	})
	return true
}

// secretDataHash returns a deterministic hash of a Secret's Data, keyed so
// that a value change on any key changes the hash regardless of Go's
// randomized map iteration order.
func secretDataHash(data map[string][]byte) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(0)
		b.Write(data[k])
		b.WriteByte(0)
	}
	return sha256Hex([]byte(b.String()))
}
