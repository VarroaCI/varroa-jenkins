package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// ---------------------------------------------------------------------------
// fakeBlobStore — a minimal oci.BlobStore for tests, backed by a real
// *oci.LayoutStore so that oci.Copy (which requires hasTarget) works.
// ---------------------------------------------------------------------------

// fakeBlobStore embeds a real LayoutStore to inherit target() for oci.Copy,
// and overrides Resolve to return a caller-controlled digest.
type fakeBlobStore struct {
	*oci.LayoutStore
	resolveDigest string
}

func (f *fakeBlobStore) Resolve(_ context.Context, _ string) (oci.Descriptor, error) {
	return oci.Descriptor{Digest: f.resolveDigest, Size: 123, MediaType: "application/vnd.oci.image.manifest.v1+json"}, nil
}

// newFakeBlobStore creates a fake blob store backed by a temp OCI layout
// directory, with a dummy manifest pre-pushed so that oci.Copy can succeed.
func newFakeBlobStore(t *testing.T, ref, digest string) *fakeBlobStore {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "varroa-uc-test-oci-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	ls, err := oci.NewLayoutStore(tmpDir)
	if err != nil {
		t.Fatalf("new layout store: %v", err)
	}

	// Push a dummy config blob first so we know its real digest.
	configDigest, _, err := ls.PushBlob(context.Background(), "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("push config blob: %v", err)
	}

	// Push a dummy manifest at the test ref referencing the real config blob.
	manifest := oci.Manifest{
		ArtifactType: "application/vnd.varroa.plugin.v1",
		Config:       oci.Descriptor{MediaType: "application/json", Digest: configDigest, Size: 2},
		Layers:       []oci.Descriptor{},
	}
	if err := ls.Push(context.Background(), ref, manifest); err != nil {
		t.Fatalf("push dummy manifest: %v", err)
	}

	return &fakeBlobStore{LayoutStore: ls, resolveDigest: digest}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// updateCenter returns a minimal UpdateCenter CR for tests.
func updateCenter(name string, mods ...func(*v1alpha1.UpdateCenter)) *v1alpha1.UpdateCenter {
	uc := &v1alpha1.UpdateCenter{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.UpdateCenterSpec{
			Storage: v1alpha1.UpdateCenterStorage{
				Type: "local",
				Local: &v1alpha1.UpdateCenterLocalStorage{
					StorageClassName: "standard",
					Size:             "10Gi",
				},
			},
		},
		Status: v1alpha1.UpdateCenterStatus{
			Conditions: []v1alpha1.UpdateCenterCondition{},
		},
	}
	for _, m := range mods {
		m(uc)
	}
	return uc
}

// withSeedRefs sets spec.seed.refs.
func withSeedRefs(refs ...string) func(*v1alpha1.UpdateCenter) {
	return func(uc *v1alpha1.UpdateCenter) {
		uc.Spec.Seed.Refs = refs
	}
}

// withExistingDigests sets status.seedImportedDigests.
func withExistingDigests(digests ...string) func(*v1alpha1.UpdateCenter) {
	return func(uc *v1alpha1.UpdateCenter) {
		uc.Status.SeedImportedDigests = digests
	}
}

// withExistingGaps sets status.gaps.
func withExistingGaps(gaps ...v1alpha1.UpdateCenterGap) func(*v1alpha1.UpdateCenter) {
	return func(uc *v1alpha1.UpdateCenter) {
		uc.Status.Gaps = gaps
	}
}

// withOCIStorage switches to OCI storage.
func withOCIStorage(ref string) func(*v1alpha1.UpdateCenter) {
	return func(uc *v1alpha1.UpdateCenter) {
		uc.Spec.Storage.Type = "oci"
		uc.Spec.Storage.Local = nil
		uc.Spec.Storage.OCI = &v1alpha1.UpdateCenterOCIStorage{
			Ref:      ref,
			Insecure: true,
		}
	}
}

// boundPVC returns a PVC in Bound phase.
func boundPVC() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "varroa-updatecenter"},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
	}
}

// pendingPVC returns a PVC in Pending phase.
func pendingPVC() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "varroa-updatecenter"},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimPending,
		},
	}
}

// importTokenSecret sets up the import token secret on the test client.
func importTokenSecret(client *testClient, token string) {
	if client.existingSecrets == nil {
		client.existingSecrets = make(map[string]map[string][]byte)
	}
	client.existingSecrets["varroa-updatecenter-import-token"] = map[string][]byte{
		"token": []byte(token),
	}
}

// ---------------------------------------------------------------------------
// TestUpdateCenterReconciler — §3.6a + §3.6b
// ---------------------------------------------------------------------------

func TestUpdateCenterReconciler(t *testing.T) {
	t.Run("singleton-name-guard", testSingletonNameGuard)
	t.Run("storage-not-ready-local", testStorageNotReadyLocal)
	t.Run("storage-not-ready-oci", testStorageNotReadyOCI)
	t.Run("inventory-failure-degraded", testInventoryFailureDegraded)
	t.Run("pullthrough-gaps-ready", testPullThroughGapsReady)
	t.Run("pullthrough-inventory-failure-degraded", testPullThroughInventoryFailureDegraded)
	t.Run("all-ready", testAllReady)
	t.Run("seed-import-idempotency", testSeedImportIdempotency)
	t.Run("seed-ref-removal", testSeedRefRemoval)
	t.Run("gap-truncation", testGapTruncation)
	t.Run("lts-metadata-derivation", testLTSMetadataDerivation)
	t.Run("lts-metadata-stable-no-rewrite", testLTSMetadataStableNoRewrite)
	t.Run("lts-metadata-disabled-clears", testLTSMetadataDisabledClears)
	t.Run("lts-metadata-storage-unready-untouched", testLTSMetadataStorageUnreadyUntouched)
	t.Run("lts-metadata-list-failure-untouched", testLTSMetadataListFailureUntouched)
	t.Run("declared-plugins-written-with-pullthrough-disabled", testDeclaredPluginsWrittenPullThroughDisabled)
	t.Run("declared-plugins-retained-when-lts-cleared", testDeclaredPluginsRetainedWhenLTSCleared)
	t.Run("declared-plugins-no-rewrite-when-unchanged", testDeclaredPluginsNoRewriteWhenUnchanged)
	t.Run("declared-plugins-untouched-on-build-failure", testDeclaredPluginsUntouchedOnBuildFailure)
}

// addProfileWithPlugins registers a JenkinsVersionProfile whose materialized
// plugins.yaml declares real plugin pins.
func addProfileWithPlugins(client *testClient, name, resolveVersion string, pins map[string]string) {
	client.profiles[name] = &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{ResolveVersion: resolveVersion},
		Status:     v1alpha1.JenkinsVersionProfileStatus{ContentRef: name + "-content"},
	}
	crdstore.MustSeed(client.store, client.profiles[name])
	var b strings.Builder
	b.WriteString("core:\n  - 2.479.1\nplugins:\n")
	names := make([]string, 0, len(pins))
	for n := range pins {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "  - artifactId: %s\n    version: %q\n", n, pins[n])
	}
	client.configMapData[name+"-content"] = map[string]string{"plugins.yaml": b.String()}
}

// testDeclaredPluginsWrittenPullThroughDisabled is the air-gap case: the declared
// set is needed MOST when pull-through is off, so it must not be gated on it.
func testDeclaredPluginsWrittenPullThroughDisabled(t *testing.T) {
	client := newTestClient()
	client.updateCenter = ucWithPullThrough(false)
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()
	ts := emptyInventoryServer(t)
	addProfileWithPlugins(client, "prof", "2.555.3", map[string]string{
		"workflow-api": "1413.v2ff1a_5e720fa_",
		"mailer":       "472.vf7c289a_4b_420",
	})

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cm := client.configMapData[metadataConfigMapName]
	want := "mailer@472.vf7c289a_4b_420\nworkflow-api@1413.v2ff1a_5e720fa_"
	if cm[declaredPluginsKey] != want {
		t.Fatalf("declared-plugins = %q, want %q", cm[declaredPluginsKey], want)
	}
}

// testDeclaredPluginsRetainedWhenLTSCleared pins that the two keys are independent:
// disabling pull-through clears lts-metadata-urls and leaves declared-plugins.
func testDeclaredPluginsRetainedWhenLTSCleared(t *testing.T) {
	client := newTestClient()
	client.updateCenter = ucWithPullThrough(false)
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()
	ts := emptyInventoryServer(t)
	addProfileWithPlugins(client, "prof", "2.555.3", map[string]string{"mailer": "472"})
	client.configMapData[metadataConfigMapName] = map[string]string{
		metadataConfigMapKey: "https://updates.jenkins.io/dynamic-stable-2.555.3/update-center.actual.json",
	}

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cm := client.configMapData[metadataConfigMapName]
	if cm[metadataConfigMapKey] != "" {
		t.Errorf("lts-metadata-urls = %q, want cleared", cm[metadataConfigMapKey])
	}
	if cm[declaredPluginsKey] != "mailer@472" {
		t.Errorf("declared-plugins = %q, want mailer@472", cm[declaredPluginsKey])
	}
}

func testDeclaredPluginsNoRewriteWhenUnchanged(t *testing.T) {
	client := newTestClient()
	client.updateCenter = ucWithPullThrough(false)
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()
	ts := emptyInventoryServer(t)
	addProfileWithPlugins(client, "prof", "2.555.3", map[string]string{"mailer": "472"})
	client.configMapData[metadataConfigMapName] = map[string]string{
		metadataConfigMapKey: "",
		declaredPluginsKey:   "mailer@472",
	}

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, w := range client.configMaps {
		if w == metadataConfigMapName {
			t.Fatalf("metadata ConfigMap rewritten despite both keys being unchanged")
		}
	}
}

// testDeclaredPluginsUntouchedOnBuildFailure covers the leave-unchanged-on-failure
// invariant: a failed buildDeclaredSet must not blank the key.
func testDeclaredPluginsUntouchedOnBuildFailure(t *testing.T) {
	client := newTestClient()
	client.updateCenter = ucWithPullThrough(false)
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()
	ts := emptyInventoryServer(t)
	client.configMapData[metadataConfigMapName] = map[string]string{
		metadataConfigMapKey: "",
		declaredPluginsKey:   "mailer@472",
	}
	client.listProfilesErr = errors.New("apiserver unavailable")
	vpGVR, gvrErr := crdstore.GVRFor[v1alpha1.JenkinsVersionProfile]()
	if gvrErr != nil {
		t.Fatal(gvrErr)
	}
	client.store.FailAlways("list", vpGVR, errors.New("apiserver unavailable"))

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, w := range client.configMaps {
		if w == metadataConfigMapName {
			t.Fatalf("metadata ConfigMap rewritten despite a declared-set build failure")
		}
	}
	if cm := client.configMapData[metadataConfigMapName]; cm[declaredPluginsKey] != "mailer@472" {
		t.Fatalf("declared-plugins = %q, want the prior value", cm[declaredPluginsKey])
	}
}

// ucWithPullThrough builds a storage-ready UpdateCenter with pull-through enabled.
func ucWithPullThrough(enabled bool) *v1alpha1.UpdateCenter {
	uc := updateCenter(updateCenterSingletonName)
	uc.Spec.PullThrough = v1alpha1.UpdateCenterPullThrough{
		Enabled:     enabled,
		UpstreamURL: "https://updates.jenkins.io",
	}
	return uc
}

// emptyInventoryServer serves an empty /api/v1/inventory so computeGaps succeeds.
func emptyInventoryServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/inventory" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ucInventoryResponse{Plugins: []inventoryEntry{}})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// addProfile registers a JenkinsVersionProfile with the given resolveVersion plus an
// empty plugins.yaml content ConfigMap so computeGaps can read it.
func addProfile(client *testClient, name, resolveVersion string) {
	client.profiles[name] = &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{ResolveVersion: resolveVersion},
		Status:     v1alpha1.JenkinsVersionProfileStatus{ContentRef: name + "-content"},
	}
	crdstore.MustSeed(client.store, client.profiles[name])
	client.configMapData[name+"-content"] = map[string]string{"plugins.yaml": "core:\n  - 2.479.1\nplugins: []\n"}
}

func mustUCGVR(t *testing.T) schema.GroupVersionResource {
	t.Helper()
	gvr, err := crdstore.GVRFor[v1alpha1.UpdateCenter]()
	if err != nil {
		t.Fatal(err)
	}
	return gvr
}

func lastUCStatus(t *testing.T, client *testClient) v1alpha1.UpdateCenterStatus {
	t.Helper()
	gvr, err := crdstore.GVRFor[v1alpha1.UpdateCenter]()
	if err != nil {
		t.Fatal(err)
	}
	ps := client.store.StatusPatches(gvr)
	if len(ps) == 0 {
		t.Fatal("expected a status patch")
	}
	st, ok := ps[len(ps)-1].Status.(*v1alpha1.UpdateCenterStatus)
	if !ok {
		t.Fatal("unexpected status patch payload type")
	}
	return *st
}

func testLTSMetadataDerivation(t *testing.T) {
	client := newTestClient()
	client.updateCenter = ucWithPullThrough(true)
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()
	ts := emptyInventoryServer(t)

	addProfile(client, "prof-lts", "2.555.3")   // LTS patch → derived
	addProfile(client, "prof-latest", "latest") // non-LTS → skipped
	addProfile(client, "prof-line", "2.555")    // bare line, non-patch → skipped

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	const ltsURL = "https://updates.jenkins.io/dynamic-stable-2.555.3/update-center.actual.json"
	const weekly = "https://updates.jenkins.io/update-center.actual.json"

	cm := client.configMapData[metadataConfigMapName]
	if cm == nil || cm[metadataConfigMapKey] != ltsURL {
		t.Fatalf("metadata ConfigMap = %v, want single LTS URL %q", cm, ltsURL)
	}
	status := lastUCStatus(t, client)
	want := []string{weekly, ltsURL}
	if !reflect.DeepEqual(status.ResolvedMetadataSources, want) {
		t.Fatalf("resolvedMetadataSources = %v, want %v", status.ResolvedMetadataSources, want)
	}
}

func testLTSMetadataStableNoRewrite(t *testing.T) {
	client := newTestClient()
	client.updateCenter = ucWithPullThrough(true)
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()
	ts := emptyInventoryServer(t)
	addProfile(client, "prof-lts", "2.555.3")

	// Pre-seed the ConfigMap with the exact value the derivation will compute, for
	// BOTH keys — they are compared independently, so a stale declared-plugins key
	// would legitimately trigger a rewrite on its own.
	const ltsURL = "https://updates.jenkins.io/dynamic-stable-2.555.3/update-center.actual.json"
	client.configMapData[metadataConfigMapName] = map[string]string{
		metadataConfigMapKey: ltsURL,
		declaredPluginsKey:   "",
	}

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, w := range client.configMaps {
		if w == metadataConfigMapName {
			t.Fatalf("metadata ConfigMap was rewritten despite an unchanged derived set")
		}
	}
}

func testLTSMetadataDisabledClears(t *testing.T) {
	client := newTestClient()
	client.updateCenter = ucWithPullThrough(false) // pull-through disabled
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()
	ts := emptyInventoryServer(t)
	addProfile(client, "prof-lts", "2.555.3")

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	status := lastUCStatus(t, client)
	if len(status.ResolvedMetadataSources) != 0 {
		t.Fatalf("resolvedMetadataSources = %v, want empty when pull-through disabled", status.ResolvedMetadataSources)
	}
	if cm := client.configMapData[metadataConfigMapName]; cm != nil && cm[metadataConfigMapKey] != "" {
		t.Fatalf("metadata ConfigMap = %q, want empty when disabled", cm[metadataConfigMapKey])
	}
}

func testLTSMetadataStorageUnreadyUntouched(t *testing.T) {
	client := newTestClient()
	client.updateCenter = ucWithPullThrough(true)
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = nil // storage NOT ready → reconcile returns early before derivation
	addProfile(client, "prof-lts", "2.555.3")

	rec := NewUpdateCenterReconciler(client, client.store, "default", "http://unused", testLogger())
	if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, w := range client.configMaps {
		if w == metadataConfigMapName {
			t.Fatalf("metadata ConfigMap written on a storage-unready tick")
		}
	}
	status := lastUCStatus(t, client)
	if len(status.ResolvedMetadataSources) != 0 {
		t.Fatalf("resolvedMetadataSources set on a storage-unready tick: %v", status.ResolvedMetadataSources)
	}
}

func testLTSMetadataListFailureUntouched(t *testing.T) {
	client := newTestClient()
	uc := ucWithPullThrough(true)
	// Prior state: a previously-derived LTS source persisted on the CR status.
	const ltsURL = "https://updates.jenkins.io/dynamic-stable-2.555.3/update-center.actual.json"
	const weekly = "https://updates.jenkins.io/update-center.actual.json"
	uc.Status.ResolvedMetadataSources = []string{weekly, ltsURL}
	client.updateCenter = uc
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()
	client.configMapData[metadataConfigMapName] = map[string]string{metadataConfigMapKey: ltsURL}
	client.listProfilesErr = errors.New("apiserver unavailable")
	vpGVR, gvrErr := crdstore.GVRFor[v1alpha1.JenkinsVersionProfile]()
	if gvrErr != nil {
		t.Fatal(gvrErr)
	}
	client.store.FailAlways("list", vpGVR, errors.New("apiserver unavailable"))
	ts := emptyInventoryServer(t)

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// A transient profile-list failure must NOT drop the prior sources.
	for _, w := range client.configMaps {
		if w == metadataConfigMapName {
			t.Fatalf("metadata ConfigMap rewritten despite a profile-list failure")
		}
	}
	if cm := client.configMapData[metadataConfigMapName]; cm[metadataConfigMapKey] != ltsURL {
		t.Fatalf("metadata ConfigMap content = %q, want unchanged %q", cm[metadataConfigMapKey], ltsURL)
	}
	status := lastUCStatus(t, client)
	want := []string{weekly, ltsURL}
	if !reflect.DeepEqual(status.ResolvedMetadataSources, want) {
		t.Fatalf("resolvedMetadataSources = %v, want prior %v preserved", status.ResolvedMetadataSources, want)
	}
}

func TestBoundSources(t *testing.T) {
	// <= cap: unchanged.
	in := []string{"a", "b", "c"}
	if got := boundSources(in); !reflect.DeepEqual(got, in) {
		t.Fatalf("boundSources(small) = %v, want %v", got, in)
	}
	// > cap: first (cap-1) + marker.
	big := make([]string, maxResolvedMetadataSources+5)
	for i := range big {
		big[i] = fmt.Sprintf("u%d", i)
	}
	got := boundSources(big)
	if len(got) != maxResolvedMetadataSources {
		t.Fatalf("len = %d, want %d", len(got), maxResolvedMetadataSources)
	}
	if got[maxResolvedMetadataSources-1] != "…(+6 more)" {
		t.Fatalf("marker = %q, want …(+6 more)", got[maxResolvedMetadataSources-1])
	}
}

// testSingletonNameGuard: reconciling a non-singleton name writes no status.
func testSingletonNameGuard(t *testing.T) {
	client := newTestClient()
	uc := updateCenter("not-the-singleton")
	client.updateCenter = uc
	crdstore.MustSeed(client.store, client.updateCenter)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected call to %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(ts.Close)

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	_, err := rec.Reconcile(context.Background(), reconcileRequest("not-the-singleton"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := len(client.store.StatusPatches(mustUCGVR(t))); n > 0 {
		t.Fatalf("expected no status patches for non-singleton, got %d", n)
	}
}

// testStorageNotReadyLocal: PVC not Bound → phase=Error, all conditions False.
func testStorageNotReadyLocal(t *testing.T) {
	client := newTestClient()
	client.updateCenter = updateCenter(updateCenterSingletonName)
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = pendingPVC()

	rec := NewUpdateCenterReconciler(client, client.store, "default", "http://test", testLogger())
	_, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	status := lastUCStatus(t, client)

	if status.Phase != v1alpha1.UpdateCenterPhase("Error") {
		t.Errorf("expected phase Error, got %s", status.Phase)
	}
	for _, ct := range []string{condTypeStorageReady, condTypeSeedImported, condTypeCoverageComplete, condTypeReady} {
		cond := findUCCondition(status.Conditions, ct)
		if cond == nil {
			t.Errorf("missing condition %s", ct)
			continue
		}
		if cond.Status != metav1.ConditionFalse {
			t.Errorf("condition %s expected False, got %s", ct, cond.Status)
		}
		if cond.Reason != reasonStorageUnavailable {
			t.Errorf("condition %s reason expected %s, got %s", ct, reasonStorageUnavailable, cond.Reason)
		}
	}
}

// testStorageNotReadyOCI: OCI registry unreachable → phase=Error.
func testStorageNotReadyOCI(t *testing.T) {
	client := newTestClient()
	client.updateCenter = updateCenter(updateCenterSingletonName, withOCIStorage("registry.example.org/repo:latest"))
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = nil
	client.pvcErr = fmt.Errorf("no PVC needed for OCI")

	rec := NewUpdateCenterReconciler(client, client.store, "default", "http://test", testLogger())
	rec.newRegistryStore = func(ref string, opts oci.RegistryOptions) (oci.BlobStore, error) {
		return nil, fmt.Errorf("registry unreachable: connection refused")
	}

	_, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	status := lastUCStatus(t, client)

	if status.Phase != v1alpha1.UpdateCenterPhase("Error") {
		t.Errorf("expected phase Error, got %s", status.Phase)
	}
}

// testInventoryFailureDegraded: inventory endpoint returns 500 → Degraded,
// CoverageComplete=False, reason InventoryUnavailable, gaps preserved.
func testInventoryFailureDegraded(t *testing.T) {
	client := newTestClient()
	existingGaps := []v1alpha1.UpdateCenterGap{
		{Plugin: "plugin-a", Version: "1.0", RequiredBy: "profile:test"},
	}
	client.updateCenter = updateCenter(updateCenterSingletonName,
		withExistingGaps(existingGaps...),
	)
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()

	var inventoryCalls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/inventory" {
			inventoryCalls++
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("internal error"))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	_, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(client.store.StatusPatches(mustUCGVR(t))) == 0 {
		t.Fatal("expected a status patch")
	}
	status := lastUCStatus(t, client)

	if status.Phase != v1alpha1.UpdateCenterPhase("Degraded") {
		t.Errorf("expected phase Degraded, got %s", status.Phase)
	}

	cc := findUCCondition(status.Conditions, condTypeCoverageComplete)
	if cc == nil {
		t.Fatal("missing CoverageComplete condition")
	}
	if cc.Status != metav1.ConditionFalse {
		t.Errorf("CoverageComplete expected False, got %s", cc.Status)
	}
	if cc.Reason != reasonInventoryUnavailable {
		t.Errorf("expected reason %s, got %s", reasonInventoryUnavailable, cc.Reason)
	}

	// Gaps must be preserved from the original CR.
	if len(status.Gaps) != 1 || status.Gaps[0].Plugin != "plugin-a" {
		t.Fatalf("expected gaps to be preserved, got %+v", status.Gaps)
	}

	// F8: Ready condition reason must be InventoryUnavailable, not generic NotReady.
	rd := findUCCondition(status.Conditions, condTypeReady)
	if rd == nil {
		t.Fatal("missing Ready condition")
	}
	if rd.Status != metav1.ConditionFalse {
		t.Errorf("Ready expected False, got %s", rd.Status)
	}
	if rd.Reason != reasonInventoryUnavailable {
		t.Errorf("Ready reason expected %s, got %s", reasonInventoryUnavailable, rd.Reason)
	}

	if inventoryCalls != 1 {
		t.Errorf("expected exactly 1 inventory call, got %d", inventoryCalls)
	}
}

// testAllReady: StorageReady=T ∧ CoverageComplete=T → phase=Ready.
func testAllReady(t *testing.T) {
	client := newTestClient()
	client.updateCenter = updateCenter(updateCenterSingletonName)
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()

	// Return an inventory with one entry to exercise PluginCount/StoreBytes.
	entry := inventoryEntry{Name: "test-p", Version: "1.0", SHA256: "abc", SizeBytes: 12345}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/inventory" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ucInventoryResponse{Plugins: []inventoryEntry{entry}})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	_, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(client.store.StatusPatches(mustUCGVR(t))) == 0 {
		t.Fatal("expected a status patch")
	}
	status := lastUCStatus(t, client)

	if status.Phase != v1alpha1.UpdateCenterPhase("Ready") {
		t.Errorf("expected phase Ready, got %s", status.Phase)
	}

	sr := findUCCondition(status.Conditions, condTypeStorageReady)
	if sr == nil || sr.Status != metav1.ConditionTrue {
		t.Errorf("StorageReady expected True, got %v", sr)
	}
	rd := findUCCondition(status.Conditions, condTypeReady)
	if rd == nil || rd.Status != metav1.ConditionTrue {
		t.Errorf("Ready expected True, got %v", rd)
	}

	// F6: PluginCount and StoreBytes must be populated from the inventory.
	if status.PluginCount != 1 {
		t.Errorf("PluginCount expected 1, got %d", status.PluginCount)
	}
	if status.StoreBytes != 12345 {
		t.Errorf("StoreBytes expected 12345, got %d", status.StoreBytes)
	}
}

// testPullThroughGapsReady: pull-through enabled + genuine coverage gaps →
// phase=Ready, Ready=True reason PullThroughServing. Gaps are served on demand,
// so a cold-cache pull-through UC must report Ready (otherwise the native path
// never engages and the store never warms).
func testPullThroughGapsReady(t *testing.T) {
	client := newTestClient()
	client.updateCenter = ucWithPullThrough(true)
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()

	// A profile declaring a plugin the (empty) inventory does not hold → a gap.
	addProfile(client, "prof-gap", "2.479.1")
	client.configMapData["prof-gap-content"]["plugins.yaml"] =
		"core:\n  - 2.479.1\nplugins:\n  - artifactId: foo\n    version: \"1.0\"\n"

	ts := emptyInventoryServer(t)

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	status := lastUCStatus(t, client)

	if status.Phase != v1alpha1.UpdateCenterPhase("Ready") {
		t.Errorf("expected phase Ready, got %s", status.Phase)
	}

	// Coverage is genuinely incomplete — the gap is still reported.
	cc := findUCCondition(status.Conditions, condTypeCoverageComplete)
	if cc == nil || cc.Status != metav1.ConditionFalse || cc.Reason != reasonGapAnalysisComplete {
		t.Fatalf("CoverageComplete expected False/GapAnalysisComplete, got %+v", cc)
	}
	if len(status.Gaps) != 1 || status.Gaps[0].Plugin != "foo" {
		t.Fatalf("expected the foo gap to be reported, got %+v", status.Gaps)
	}

	// ...but the UC is Ready via pull-through so controllers route to it.
	rd := findUCCondition(status.Conditions, condTypeReady)
	if rd == nil || rd.Status != metav1.ConditionTrue {
		t.Fatalf("Ready expected True, got %+v", rd)
	}
	if rd.Reason != "PullThroughServing" {
		t.Errorf("Ready reason expected PullThroughServing, got %s", rd.Reason)
	}
}

// testPullThroughInventoryFailureDegraded: pull-through must NOT mask a failure
// to compute coverage (inventory endpoint down) — that signals an unhealthy UC
// HTTP API, so it stays Degraded / not Ready even with pull-through enabled.
func testPullThroughInventoryFailureDegraded(t *testing.T) {
	client := newTestClient()
	client.updateCenter = ucWithPullThrough(true)
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/inventory" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	status := lastUCStatus(t, client)

	if status.Phase != v1alpha1.UpdateCenterPhase("Degraded") {
		t.Errorf("expected phase Degraded, got %s", status.Phase)
	}
	rd := findUCCondition(status.Conditions, condTypeReady)
	if rd == nil || rd.Status != metav1.ConditionFalse {
		t.Fatalf("Ready expected False, got %+v", rd)
	}
	if rd.Reason != reasonInventoryUnavailable {
		t.Errorf("Ready reason expected %s, got %s", reasonInventoryUnavailable, rd.Reason)
	}
}

// testSeedImportIdempotency: first reconcile POSTs to /api/v1/import once;
// second reconcile (digest already imported) does NOT re-POST.
func testSeedImportIdempotency(t *testing.T) {
	client := newTestClient()
	importTokenSecret(client, "test-token")

	const testRef = "registry.example.org/plugins/plugin-a:1.0"
	const testDigest = "sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	uc := updateCenter(updateCenterSingletonName,
		withSeedRefs(testRef),
	)
	client.updateCenter = uc
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()

	var importCalls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/inventory":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ucInventoryResponse{Plugins: []inventoryEntry{}})
		case "/api/v1/import":
			importCalls++
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)

	fbs := newFakeBlobStore(t, testRef, testDigest)

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	rec.newRegistryStore = func(ref string, opts oci.RegistryOptions) (oci.BlobStore, error) {
		return fbs, nil
	}

	// First reconcile.
	_, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName))
	if err != nil {
		t.Fatalf("first reconcile: unexpected error: %v", err)
	}
	if importCalls != 1 {
		t.Errorf("expected 1 import POST on first reconcile, got %d", importCalls)
	}

	// The digest should now be in status.seedImportedDigests.
	status := lastUCStatus(t, client)
	found := false
	for _, d := range status.SeedImportedDigests {
		if d == testDigest {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected digest %s in SeedImportedDigests, got %v", testDigest, status.SeedImportedDigests)
	}

	// Update the client's UC with the new status.
	updatedUC := uc.DeepCopy()
	updatedUC.Status.SeedImportedDigests = status.SeedImportedDigests
	client.updateCenter = updatedUC
	crdstore.MustSeed(client.store, updatedUC)

	// Second reconcile — should skip import.
	_, err = rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName))
	if err != nil {
		t.Fatalf("second reconcile: unexpected error: %v", err)
	}
	if importCalls != 1 {
		t.Errorf("expected no new import POST on second reconcile (still 1), got %d", importCalls)
	}
}

// testSeedRefRemoval: a ref removed from spec.seed.refs has its digest fall
// OUT of status.seedImportedDigests.
func testSeedRefRemoval(t *testing.T) {
	client := newTestClient()
	importTokenSecret(client, "test-token")

	const testRef = "registry.example.org/plugins/plugin-a:1.0"
	const testDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"

	uc := updateCenter(updateCenterSingletonName,
		withSeedRefs(testRef),
		withExistingDigests(testDigest, "sha256:9999999999999999999999999999999999999999999999999999999999999999"),
	)
	client.updateCenter = uc
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/inventory":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ucInventoryResponse{Plugins: []inventoryEntry{}})
		case "/api/v1/import":
			t.Errorf("unexpected import POST after ref removal")
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)

	fbs := newFakeBlobStore(t, testRef, testDigest)

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	rec.newRegistryStore = func(ref string, opts oci.RegistryOptions) (oci.BlobStore, error) {
		return fbs, nil
	}

	_, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(client.store.StatusPatches(mustUCGVR(t))) == 0 {
		t.Fatal("expected a status patch")
	}
	status := lastUCStatus(t, client)

	// Only the testRef's digest should appear in SeedImportedDigests.
	if len(status.SeedImportedDigests) != 1 {
		t.Fatalf("expected exactly 1 digest in SeedImportedDigests, got %v", status.SeedImportedDigests)
	}
	if status.SeedImportedDigests[0] != testDigest {
		t.Errorf("expected digest %s, got %s", testDigest, status.SeedImportedDigests[0])
	}
}

// testGapTruncation: >50 gaps → status.gaps truncated to 50 and
// CoverageComplete message contains "N more gaps not shown".
func testGapTruncation(t *testing.T) {
	client := newTestClient()
	client.updateCenter = updateCenter(updateCenterSingletonName)
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()

	// Inventory returns 0 entries.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/inventory" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ucInventoryResponse{Plugins: []inventoryEntry{}})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	// Seed 60 declared plugins via a JenkinsVersionProfile ConfigMap.
	const profileName = "test-profile"
	var lockSet struct {
		Core    []string `yaml:"core"`
		Plugins []struct {
			ArtifactID string `yaml:"artifactId"`
			Version    string `yaml:"version"`
		} `yaml:"plugins"`
	}
	lockSet.Core = []string{"2.479.1"}
	for i := 0; i < 60; i++ {
		lockSet.Plugins = append(lockSet.Plugins, struct {
			ArtifactID string `yaml:"artifactId"`
			Version    string `yaml:"version"`
		}{ArtifactID: fmt.Sprintf("plugin-%d", i), Version: "1.0.0"})
	}
	pluginsBytes, err := yaml.Marshal(lockSet)
	if err != nil {
		t.Fatalf("marshal plugins.yaml: %v", err)
	}

	client.profiles[profileName] = &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: profileName},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			ContentRef: "test-profile-content",
		},
	}
	crdstore.MustSeed(client.store, client.profiles[profileName])

	client.configMapData["test-profile-content"] = map[string]string{
		"plugins.yaml": string(pluginsBytes),
	}

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	_, err = rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(client.store.StatusPatches(mustUCGVR(t))) == 0 {
		t.Fatal("expected a status patch")
	}
	status := lastUCStatus(t, client)

	// Gaps must be truncated to maxGaps.
	if len(status.Gaps) > maxGaps {
		t.Errorf("expected gaps truncated to max %d, got %d", maxGaps, len(status.Gaps))
	}
	if len(status.Gaps) != maxGaps {
		t.Errorf("expected exactly %d gaps, got %d", maxGaps, len(status.Gaps))
	}

	cc := findUCCondition(status.Conditions, condTypeCoverageComplete)
	if cc == nil {
		t.Fatal("missing CoverageComplete condition")
	}
	if cc.Status != metav1.ConditionFalse {
		t.Errorf("CoverageComplete expected False, got %s", cc.Status)
	}
	if !strings.Contains(cc.Message, "more gaps not shown") {
		t.Errorf("expected 'more gaps not shown' in message, got: %s", cc.Message)
	}
}

// TestUpdateCenterStorageFailureSkipsSeedAndInventory: PVC not ClaimBound →
// all 4 conditions False, phase=Error, seed/gap steps SKIPPED (no inventory/import).
func TestUpdateCenterStorageFailureSkipsSeedAndInventory(t *testing.T) {
	client := newTestClient()
	client.updateCenter = updateCenter(updateCenterSingletonName,
		withSeedRefs("example.org/plugin:1.0"),
	)
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = pendingPVC() // Not bound

	var inventoryCalls, importCalls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/inventory":
			inventoryCalls++
		case "/api/v1/import":
			importCalls++
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	_, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if inventoryCalls > 0 {
		t.Errorf("inventory should NOT have been called when storage is not ready, called %d times", inventoryCalls)
	}
	if importCalls > 0 {
		t.Errorf("import should NOT have been called when storage is not ready, called %d times", importCalls)
	}

	if len(client.store.StatusPatches(mustUCGVR(t))) == 0 {
		t.Fatal("expected a status patch")
	}
	status := lastUCStatus(t, client)

	if status.Phase != v1alpha1.UpdateCenterPhase("Error") {
		t.Errorf("expected phase Error, got %s", status.Phase)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func reconcileRequest(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
}

func findUCCondition(conditions []v1alpha1.UpdateCenterCondition, condType string) *v1alpha1.UpdateCenterCondition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}
