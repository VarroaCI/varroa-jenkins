package controller

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

const testBFFServiceAccount = "varroa-varroa-bff"

// grantReconciler builds a reconciler that knows the BFF ServiceAccount and
// runs in operator namespace "varroa-system".
func grantReconciler(tc *catalogTestClient, t *testing.T) *CatalogReconciler {
	t.Helper()
	rec := newComposedBundleReconciler(tc, t)
	rec.operatorNamespace = "varroa-system"
	rec.SetBFFServiceAccount(testBFFServiceAccount)
	return rec
}

// readyBundle builds a ComposedBundle that composes one seeded JCasC item.
func readyBundle(name string) *v1alpha1.ComposedBundle {
	return &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: v1alpha1.ComposedBundleSpec{
			Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "jcasc-1"}}},
		},
	}
}

// TestContentGrantWrittenForReadyBundle asserts a reconcile of a Ready bundle
// writes a grant naming status.contentRef, in the bundle's namespace, with the
// bundle's owner reference.
func TestContentGrantWrittenForReadyBundle(t *testing.T) {
	tc := newCatalogTestClient()
	tc.seedItem(jcascItem("jcasc-1", "jenkins:\n  systemMessage: \"hello\"\n"))
	cb := readyBundle("cb1")

	grantReconciler(tc, t).ReconcileComposedBundle(context.Background(), cb)

	if len(tc.contentGrants) != 1 {
		t.Fatalf("want exactly one grant, got %d", len(tc.contentGrants))
	}
	g := tc.contentGrants[0]
	if g.Name != "cb1-content-reader" {
		t.Errorf("grant name = %q, want cb1-content-reader", g.Name)
	}
	if g.Namespace != "ns" {
		t.Errorf("grant namespace = %q, want ns", g.Namespace)
	}
	if g.ConfigMapName != "cb1-content" {
		t.Errorf("configMapName = %q, want cb1-content", g.ConfigMapName)
	}
	if g.Owner.Kind != "ComposedBundle" || g.Owner.Name != "cb1" {
		t.Errorf("owner = %+v, want the ComposedBundle cb1", g.Owner)
	}
}

// TestContentGrantNamesConfiguredServiceAccount asserts the RoleBinding subject
// is the configured BFF ServiceAccount, resolved in the operator namespace.
func TestContentGrantNamesConfiguredServiceAccount(t *testing.T) {
	tc := newCatalogTestClient()
	tc.seedItem(jcascItem("jcasc-1", "jenkins:\n  systemMessage: \"hello\"\n"))

	grantReconciler(tc, t).ReconcileComposedBundle(context.Background(), readyBundle("cb1"))

	if len(tc.contentGrants) != 1 {
		t.Fatalf("want exactly one grant, got %d", len(tc.contentGrants))
	}
	g := tc.contentGrants[0]
	if g.SubjectName != testBFFServiceAccount {
		t.Errorf("subjectName = %q, want %q", g.SubjectName, testBFFServiceAccount)
	}
	if g.SubjectNamespace != "varroa-system" {
		t.Errorf("subjectNamespace = %q, want varroa-system", g.SubjectNamespace)
	}
}

// TestContentGrantSkippedWhenNeverComposed asserts a bundle with no resolvable
// content gets no grant. An empty contentRef names no ConfigMap to grant.
func TestContentGrantSkippedWhenNeverComposed(t *testing.T) {
	tc := newCatalogTestClient()
	// The itemRef names no seeded CatalogItem, so the bundle stays Invalid and
	// never sets a contentRef.
	cb := &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "cb1", Namespace: "ns"},
		Spec: v1alpha1.ComposedBundleSpec{
			Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "absent"}}},
		},
	}

	grantReconciler(tc, t).ReconcileComposedBundle(context.Background(), cb)

	if got := tc.lastStatus(); got == nil || got.ContentRef != "" {
		t.Fatalf("precondition: want an empty contentRef, got %+v", got)
	}
	if len(tc.contentGrants) != 0 {
		t.Errorf("want no grant for a bundle that never composed, got %d", len(tc.contentGrants))
	}
}

// TestContentGrantSkippedWithoutBFFServiceAccount asserts an operator that
// knows no BFF writes no grant, and warns once for the whole process rather
// than once per bundle.
func TestContentGrantSkippedWithoutBFFServiceAccount(t *testing.T) {
	missingBFFServiceAccountOnce = sync.Once{}
	tc := newCatalogTestClient()
	tc.seedItem(jcascItem("jcasc-1", "jenkins:\n  systemMessage: \"hello\"\n"))

	var logs bytes.Buffer
	rec := newComposedBundleReconciler(tc, t)
	rec.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	rec.SetBFFServiceAccount("")

	rec.ReconcileComposedBundle(context.Background(), readyBundle("cb1"))
	rec.ReconcileComposedBundle(context.Background(), readyBundle("cb2"))

	if len(tc.contentGrants) != 0 {
		t.Errorf("want no grant without a BFF service account, got %d", len(tc.contentGrants))
	}
	if n := strings.Count(logs.String(), "VARROA_BFF_SERVICE_ACCOUNT"); n != 1 {
		t.Errorf("warning count = %d, want exactly 1 for the process", n)
	}
}

// TestContentGrantOnSkipPathNamesPreviousContentRef asserts the skip path
// reconciles the grant against the bundle's live contentRef. The reconciler
// performs no compose there, so the grant must come from the persisted value.
func TestContentGrantOnSkipPathNamesPreviousContentRef(t *testing.T) {
	tc := newCatalogTestClient()
	cb := skippedBundle(tc)

	grantReconciler(tc, t).ReconcileComposedBundle(context.Background(), cb)

	if tc.lastStatus() != nil {
		t.Fatalf("precondition: this must take the skip path, which patches no status, got %+v", tc.lastStatus())
	}
	if len(tc.contentGrants) != 1 {
		t.Fatalf("want one grant on the skip path, got %d", len(tc.contentGrants))
	}
	if got := tc.contentGrants[0].ConfigMapName; got != "cb1-content" {
		t.Errorf("configMapName = %q, want the persisted cb1-content", got)
	}
}

// TestContentGrantFallsBackToPersistedContentRef asserts an early exit that
// reports no contentRef still grants read on the last-known-good ConfigMap.
//
// Six exits pass a literal "" contentRef to buildStatus. That value never
// reaches etcd: ContentRef is omitempty, the patch is a JSON merge patch, and
// clearStatusFields does not list contentRef. The persisted value therefore
// stays live and BFF-readable, so the grant must name it. Without the fallback
// in applyStatusAndGrant the bundle would keep readable content with no grant.
func TestContentGrantFallsBackToPersistedContentRef(t *testing.T) {
	tc := newCatalogTestClient()
	// A git input whose auth Secret cannot be read exits before any compose,
	// reporting an empty contentRef.
	cb := &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "cb1", Namespace: "ns", Generation: 2},
		Spec: v1alpha1.ComposedBundleSpec{
			Inputs: []v1alpha1.ComposedInput{{GitSource: &v1alpha1.GitBundleSource{
				RepoURL:   "https://example.invalid/repo.git",
				SecretRef: "absent-secret",
			}}},
		},
		Status: v1alpha1.ComposedBundleStatus{
			Phase:              v1alpha1.ComposedBundleReady,
			ContentRef:         "cb1-content",
			ObservedGeneration: 1,
		},
	}

	grantReconciler(tc, t).ReconcileComposedBundle(context.Background(), cb)

	st := tc.lastStatus()
	if st == nil {
		t.Fatal("precondition: the early exit must patch status")
	}
	if st.ContentRef != "" {
		t.Fatalf("precondition: this exit must report an empty contentRef, got %q", st.ContentRef)
	}
	if len(tc.contentGrants) != 1 {
		t.Fatalf("want one grant from the persisted contentRef, got %d", len(tc.contentGrants))
	}
	if got := tc.contentGrants[0].ConfigMapName; got != "cb1-content" {
		t.Errorf("configMapName = %q, want the persisted cb1-content", got)
	}
}

// skipPathItemHash is the content hash the skip-path fixtures pin. The
// reconciler keys observedRevisions by input index, and it skips the compose
// cycle only when the recorded revision equals the item's current hash.
const skipPathItemHash = "sha256:seeded"

// skippedBundle builds a bundle that takes the recompose skip path: its
// generation matches observedGeneration, it has a live contentRef, its content
// ConfigMap exists, and its recorded revision matches the seeded item.
//
// The revision key is the input index, not the item name. A mismatched key
// leaves the recorded value empty, which happens to equal an unset hash and so
// takes the skip path for the wrong reason. Seeding both sides explicitly keeps
// the fixture honest.
func skippedBundle(tc *catalogTestClient) *v1alpha1.ComposedBundle {
	tc.configMapData["cb1-content"] = map[string]string{"jenkins.yaml": "jenkins: {}\n"}
	item := jcascItem("jcasc-1", "jenkins:\n  systemMessage: \"hello\"\n")
	item.Status.ContentHash = skipPathItemHash
	tc.seedItem(item)
	return &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "cb1", Namespace: "ns", Generation: 1},
		Spec: v1alpha1.ComposedBundleSpec{
			Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "jcasc-1"}}},
		},
		Status: v1alpha1.ComposedBundleStatus{
			Phase:              v1alpha1.ComposedBundleReady,
			ContentRef:         "cb1-content",
			ObservedGeneration: 1,
			ObservedRevisions:  map[string]string{"0": skipPathItemHash},
		},
	}
}

// TestContentGrantReconciledOnSkippedTick asserts an unchanged bundle still
// reconciles its grant. The skip path performs no recompose, so a grant removed
// by hand would otherwise never converge.
func TestContentGrantReconciledOnSkippedTick(t *testing.T) {
	tc := newCatalogTestClient()
	tc.seedItem(jcascItem("jcasc-1", "jenkins:\n  systemMessage: \"hello\"\n"))
	cb := skippedBundle(tc)

	grantReconciler(tc, t).ReconcileComposedBundle(context.Background(), cb)

	if len(tc.contentGrants) != 1 {
		t.Fatalf("want one grant on a skipped tick, got %d", len(tc.contentGrants))
	}
	if got := tc.contentGrants[0].ConfigMapName; got != "cb1-content" {
		t.Errorf("configMapName = %q, want cb1-content", got)
	}
	if tc.lastStatus() != nil {
		t.Errorf("a healthy skipped tick must patch no status, got %+v", tc.lastStatus())
	}
}

// TestContentGrantFailureOnSkippedTickPatchesWarning asserts a grant failure on
// an otherwise silent tick still reaches status. A steady bundle takes the skip
// path on almost every tick, so the warning would never persist otherwise.
func TestContentGrantFailureOnSkippedTickPatchesWarning(t *testing.T) {
	tc := newCatalogTestClient()
	tc.seedItem(jcascItem("jcasc-1", "jenkins:\n  systemMessage: \"hello\"\n"))
	cb := skippedBundle(tc)
	cb.Status.Errors = []string{"an earlier content error"}
	tc.contentGrantErr = errors.New("forbidden")

	grantReconciler(tc, t).ReconcileComposedBundle(context.Background(), cb)

	st := tc.lastStatus()
	if st == nil {
		t.Fatal("a failed grant on a skipped tick must patch status")
	}
	if st.Phase != v1alpha1.ComposedBundleReady {
		t.Errorf("phase = %q, want the preserved Ready", st.Phase)
	}
	if st.ContentRef != "cb1-content" {
		t.Errorf("contentRef = %q, want the preserved cb1-content", st.ContentRef)
	}
	if len(st.Errors) != 1 || st.Errors[0] != "an earlier content error" {
		t.Errorf("errors = %v, want the existing errors preserved", st.Errors)
	}
	var found bool
	for _, w := range st.Warnings {
		if strings.Contains(w, "content read grant") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want one naming the content read grant", st.Warnings)
	}
}

// countGrantWarnings returns how many grant warnings a status carries.
func countGrantWarnings(st *v1alpha1.ComposedBundleStatus) int {
	n := 0
	for _, w := range st.Warnings {
		if strings.HasPrefix(w, "content read grant: ") {
			n++
		}
	}
	return n
}

// TestContentGrantWarningNeverAccumulates asserts a persistent grant failure
// carries exactly one warning, however many ticks it lasts, and that the
// warning clears itself once the write succeeds again.
//
// The skip path carries the previous warnings forward, because no compose runs
// to rebuild them. Appending the grant entry without stripping the old one
// would grow status.warnings without bound until the object hit its size limit.
func TestContentGrantWarningNeverAccumulates(t *testing.T) {
	tc := newCatalogTestClient()
	tc.seedItem(jcascItem("jcasc-1", "jenkins:\n  systemMessage: \"hello\"\n"))
	cb := skippedBundle(tc)
	cb.Status.Warnings = []string{"an unrelated composer warning"}
	tc.contentGrantErr = errors.New("forbidden")
	rec := grantReconciler(tc, t)

	// Three consecutive failing ticks. Each one feeds the previous status back,
	// exactly as the informer does on a live cluster.
	for i := 1; i <= 3; i++ {
		rec.ReconcileComposedBundle(context.Background(), cb)
		st := tc.lastStatus()
		if st == nil {
			t.Fatalf("tick %d: a failed grant must patch status", i)
		}
		if n := countGrantWarnings(st); n != 1 {
			t.Fatalf("tick %d: grant warnings = %d, want exactly 1 (warnings: %v)", i, n, st.Warnings)
		}
		if len(st.Warnings) != 2 {
			t.Fatalf("tick %d: warnings = %v, want the composer warning plus one grant entry", i, st.Warnings)
		}
		cb.Status = *st
	}

	// The grant write recovers. The stale warning must clear itself.
	tc.contentGrantErr = nil
	rec.ReconcileComposedBundle(context.Background(), cb)

	st := tc.lastStatus()
	if st == nil {
		t.Fatal("a recovered grant must patch status to clear the stale warning")
	}
	if n := countGrantWarnings(st); n != 0 {
		t.Errorf("grant warnings = %d, want 0 after the write succeeds (warnings: %v)", n, st.Warnings)
	}
	if len(st.Warnings) != 1 || st.Warnings[0] != "an unrelated composer warning" {
		t.Errorf("warnings = %v, want only the unrelated composer warning kept", st.Warnings)
	}
}

// TestContentGrantRecoveredTickWritesNothingWhenClean asserts a healthy steady
// state still performs no status write. Only a new warning, or clearing a stale
// one, justifies a patch on the skip path.
func TestContentGrantRecoveredTickWritesNothingWhenClean(t *testing.T) {
	tc := newCatalogTestClient()
	tc.seedItem(jcascItem("jcasc-1", "jenkins:\n  systemMessage: \"hello\"\n"))
	cb := skippedBundle(tc)
	cb.Status.Warnings = []string{"an unrelated composer warning"}

	grantReconciler(tc, t).ReconcileComposedBundle(context.Background(), cb)

	if tc.lastStatus() != nil {
		t.Errorf("a healthy skipped tick must patch no status, got %+v", tc.lastStatus())
	}
}

// TestContentGrantFailureWarnsAndKeepsPhase asserts a failed grant write adds a
// warning, leaves status.Errors alone, and never changes the phase. The bundle
// content is correct; only the BFF's read of it is blocked.
func TestContentGrantFailureWarnsAndKeepsPhase(t *testing.T) {
	tc := newCatalogTestClient()
	tc.seedItem(jcascItem("jcasc-1", "jenkins:\n  systemMessage: \"hello\"\n"))
	tc.contentGrantErr = errors.New("forbidden")

	grantReconciler(tc, t).ReconcileComposedBundle(context.Background(), readyBundle("cb1"))

	st := tc.lastStatus()
	if st == nil {
		t.Fatal("no status patched")
	}
	if st.Phase != v1alpha1.ComposedBundleReady {
		t.Errorf("phase = %q, want Ready: a grant failure must not fail the bundle", st.Phase)
	}
	if len(st.Errors) != 0 {
		t.Errorf("errors = %v, want none: a grant failure is not a content error", st.Errors)
	}
	var found bool
	for _, w := range st.Warnings {
		if strings.Contains(w, "content read grant") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want one naming the content read grant", st.Warnings)
	}
}

// TestVersionProfileGetsNoContentGrant asserts the grant covers composed
// bundles alone. A version profile's plugin set lives in the operator
// namespace, which the BFF's release-namespace Role already covers.
func TestVersionProfileGetsNoContentGrant(t *testing.T) {
	client := newTestClient()
	client.profiles["test-profile"] = &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-profile",
			UID:  "00000000-0000-0000-0000-000000000001",
		},
		Spec: v1alpha1.JenkinsVersionProfileSpec{
			Version:      "2.479.3",
			Channel:      "lts",
			PluginSetRef: &v1alpha1.ConfigMapRef{Name: "source-plugins"},
		},
	}
	crdstore.MustSeed(client.store, client.profiles["test-profile"])
	client.configMapData["source-plugins"] = map[string]string{
		"plugins.yaml": "core:\n  - \"2.479.3\"\nplugins:\n  - artifactId: \"git\"\n    version: \"5.6.0\"\n",
	}

	rec := NewJenkinsVersionProfileReconciler(client, client.store, profileReconcilerNamespace, discardLogger())
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test-profile"},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, ok := client.configMapData["test-profile-pluginset-content"]; !ok {
		t.Fatal("precondition: the plugin set ConfigMap was not written")
	}
	if len(client.contentGrants) != 0 {
		t.Errorf("want no grant for a version profile plugin set, got %d", len(client.contentGrants))
	}
}
