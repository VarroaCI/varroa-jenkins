package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/bundles"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func newStarterTestReconciler() (*StarterReconciler, *crdstore.Fake) {
	store := crdstore.NewFake()
	return NewStarterReconciler(store, "varroa-system", nil), store
}

func TestStarterSeedsItemsAndBundleOnFirstBoot(t *testing.T) {
	ctx := context.Background()
	r, store := newStarterTestReconciler()

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	jcasc, err := crdstore.Get[v1alpha1.CatalogItem](ctx, store, starterJCasCItemName, "varroa-system")
	if err != nil || jcasc == nil {
		t.Fatalf("jcasc item not seeded: %v", err)
	}
	if jcasc.Spec.Type != v1alpha1.CatalogItemJCasC {
		t.Errorf("jcasc item type = %q, want %q", jcasc.Spec.Type, v1alpha1.CatalogItemJCasC)
	}
	// The composer reads status.content, so a seeded item with an empty or
	// invalid status composes to nothing — the failure this seeder exists to
	// avoid.
	if !jcasc.Status.Valid {
		t.Error("jcasc item status.valid must be true or the composer rejects it")
	}
	if jcasc.Status.Content != bundles.StarterJCasC() {
		t.Error("jcasc item status.content must be the embedded content verbatim")
	}
	if jcasc.Status.ContentHash == "" {
		t.Error("jcasc item status.contentHash must be set")
	}

	items, err := crdstore.Get[v1alpha1.CatalogItem](ctx, store, starterItemsItemName, "varroa-system")
	if err != nil || items == nil {
		t.Fatalf("items item not seeded: %v", err)
	}
	if items.Spec.Type != v1alpha1.CatalogItemItem {
		t.Errorf("items item type = %q, want %q", items.Spec.Type, v1alpha1.CatalogItemItem)
	}
	if items.Status.Content != bundles.StarterItems() {
		t.Error("items item status.content must be the embedded content verbatim")
	}

	cb, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, store, StarterBundleName, "varroa-system")
	if err != nil || cb == nil {
		t.Fatalf("composed bundle not seeded: %v", err)
	}
	if len(cb.Spec.Inputs) != 2 {
		t.Fatalf("bundle inputs = %d, want 2", len(cb.Spec.Inputs))
	}
	for i, in := range cb.Spec.Inputs {
		if in.ItemRef == nil {
			t.Fatalf("input %d has no itemRef", i)
		}
		// An unset namespace would let a same-named item in the consuming
		// namespace shadow the built-in one.
		if in.ItemRef.Namespace != "varroa-system" {
			t.Errorf("input %d namespace = %q, want varroa-system", i, in.ItemRef.Namespace)
		}
	}
}

func TestStarterRecreatesDeletedObjects(t *testing.T) {
	ctx := context.Background()
	r, store := newStarterTestReconciler()
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	for _, name := range []string{starterJCasCItemName, starterItemsItemName} {
		if err := crdstore.Delete[v1alpha1.CatalogItem](ctx, store, name, "varroa-system"); err != nil {
			t.Fatalf("delete %s: %v", name, err)
		}
	}
	if err := crdstore.Delete[v1alpha1.ComposedBundle](ctx, store, StarterBundleName, "varroa-system"); err != nil {
		t.Fatalf("delete bundle: %v", err)
	}

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}

	if got, err := crdstore.Get[v1alpha1.CatalogItem](ctx, store, starterJCasCItemName, "varroa-system"); err != nil || got == nil {
		t.Fatal("jcasc item was not re-seeded after deletion")
	}
	if got, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, store, StarterBundleName, "varroa-system"); err != nil || got == nil {
		t.Fatal("composed bundle was not re-seeded after deletion")
	}
}

// A helm upgrade onto a cluster where someone already created objects under the
// reserved names must not overwrite them. Failing safe here matters more than
// seeding: the user's bundle is what their controllers are running.
func TestStarterSkipsForeignObjects(t *testing.T) {
	ctx := context.Background()
	r, store := newStarterTestReconciler()

	crdstore.MustSeed(store, &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{Name: starterJCasCItemName, Namespace: "varroa-system"},
		Spec:       v1alpha1.CatalogItemSpec{SourceRef: "mine", Type: v1alpha1.CatalogItemJCasC, Path: "mine.yaml"},
		Status:     v1alpha1.CatalogItemStatus{Content: "jenkins: {}", Valid: true},
	})
	crdstore.MustSeed(store, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: StarterBundleName, Namespace: "varroa-system"},
		Spec: v1alpha1.ComposedBundleSpec{
			Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "something-else"}}},
		},
	})

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile must not fail on a name collision: %v", err)
	}

	item, err := crdstore.Get[v1alpha1.CatalogItem](ctx, store, starterJCasCItemName, "varroa-system")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if item.Spec.SourceRef != "mine" || item.Status.Content != "jenkins: {}" {
		t.Error("a foreign catalog item under the reserved name was overwritten")
	}

	cb, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, store, StarterBundleName, "varroa-system")
	if err != nil {
		t.Fatalf("get bundle: %v", err)
	}
	if len(cb.Spec.Inputs) != 1 || cb.Spec.Inputs[0].ItemRef.Name != "something-else" {
		t.Error("a foreign composed bundle under the reserved name was overwritten")
	}
}

// countingBackend records the writes a reconciler performs. The Fake does not
// bump resourceVersion on update, so comparing versions before and after would
// pass whether or not a write happened; counting the calls is the only honest
// way to assert a no-op.
type countingBackend struct {
	crdstore.Backend
	creates, updates, statusPatches int
}

func (c *countingBackend) CreateObject(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	c.creates++
	return c.Backend.CreateObject(ctx, gvr, ns, obj)
}

func (c *countingBackend) UpdateObject(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	c.updates++
	return c.Backend.UpdateObject(ctx, gvr, ns, obj)
}

func (c *countingBackend) PatchObjectStatus(ctx context.Context, gvr schema.GroupVersionResource, ns, name string, status any) error {
	c.statusPatches++
	return c.Backend.PatchObjectStatus(ctx, gvr, ns, name, status)
}

// The seeder runs on a ticker. A tick that changes nothing must not write, or
// every install generates a write per object per minute forever.
func TestStarterSecondTickIsAWriteFreeNoop(t *testing.T) {
	ctx := context.Background()
	store := &countingBackend{Backend: crdstore.NewFake()}
	r := NewStarterReconciler(store, "varroa-system", nil)

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if store.creates == 0 {
		t.Fatal("first tick wrote nothing; the rest of this test would be vacuous")
	}

	store.creates, store.updates, store.statusPatches = 0, 0, 0
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if store.creates != 0 || store.updates != 0 || store.statusPatches != 0 {
		t.Errorf("no-change tick wrote: %d creates, %d updates, %d status patches",
			store.creates, store.updates, store.statusPatches)
	}
}

// An operator upgrade ships new embedded content; the seeded item must follow it.
func TestStarterRepairsStaleContent(t *testing.T) {
	ctx := context.Background()
	r, store := newStarterTestReconciler()
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	stale := &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{
			Name:        starterJCasCItemName,
			Namespace:   "varroa-system",
			Labels:      map[string]string{starterManagedLabel: "true"},
			Annotations: map[string]string{starterHashAnnotation: "stale"},
		},
		Spec:   v1alpha1.CatalogItemSpec{SourceRef: starterSourceRef, Type: v1alpha1.CatalogItemJCasC, Path: "old.yaml"},
		Status: v1alpha1.CatalogItemStatus{Content: "jenkins: {}", ContentHash: "stale", Valid: true},
	}
	if err := crdstore.Apply(ctx, store, stale); err != nil {
		t.Fatalf("seed stale item: %v", err)
	}
	if err := crdstore.PatchStatus[v1alpha1.CatalogItem](ctx, store, stale.Name, stale.Namespace, &stale.Status); err != nil {
		t.Fatalf("seed stale status: %v", err)
	}

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}

	got, err := crdstore.Get[v1alpha1.CatalogItem](ctx, store, starterJCasCItemName, "varroa-system")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.Status.Content != bundles.StarterJCasC() {
		t.Error("stale starter content was not repaired to the embedded content")
	}
}

func TestStarterRequiresOperatorNamespace(t *testing.T) {
	r := NewStarterReconciler(crdstore.NewFake(), "", nil)
	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("expected an error when the operator namespace is unset")
	}
}

// The embedded content is only useful if it survives the pipeline it feeds. A
// dollar-brace placeholder anywhere in it — including a comment, which the scan
// does not skip — fails every zero-config Controller at provisioning time, and
// a pinned plugin version drifts against the version-profile lock.
func TestStarterContentHasNoVariablesOrPinnedPlugins(t *testing.T) {
	// Exactly the variables resolveBundleForController injects unconditionally.
	// The OIDC and login-URL vars are deliberately absent: they appear only when
	// a Resolver is configured, so a starter bundle that used one would work in
	// some installs and fail in others.
	alwaysInjected := bundle.Variables{
		"varroa_controller_name":         "c",
		"varroa_controller_namespace":    "ns",
		"varroa_controller_endpoint":     "http://c-svc.ns.svc.cluster.local:8080",
		"varroa_frontend_url":            "https://varroa.example",
		"varroa_controller_external_url": "",
		"varroa_controller_path_prefix":  "",
	}

	var gotImages []string
	for name, content := range map[string]string{
		"jenkins.yaml": bundles.StarterJCasC(),
		"items.yaml":   bundles.StarterItems(),
	} {
		if strings.TrimSpace(content) == "" {
			t.Fatalf("%s is empty", name)
		}
		for _, v := range findUnresolvedVars(true, bundle.ResolveVars(content, alwaysInjected)) {
			t.Errorf("%s references %q, which the operator does not inject", name, v)
		}
		// The key checks below run on the YAML only. The variable check above
		// deliberately does not: the resolver scans comments too.
		yamlOnly := stripYAMLComments(content)
		if strings.Contains(yamlOnly, "\nplugins:") {
			t.Errorf("%s declares plugins; the version-profile lock owns the plugin set", name)
		}
		if strings.Contains(yamlOnly, "authorizationStrategy") {
			t.Errorf("%s sets authorizationStrategy, which Varroa strips", name)
		}
		if strings.Contains(yamlOnly, "securityRealm") {
			t.Errorf("%s sets securityRealm, which needs variables Varroa does not inject", name)
		}
		// Collect every image reference; the approved-set assertion runs once,
		// after both files have been scanned.
		for _, line := range strings.Split(yamlOnly, "\n") {
			if !strings.Contains(line, "image:") {
				continue
			}
			ref := strings.Trim(strings.TrimSpace(strings.SplitN(line, "image:", 2)[1]), `"'`)
			if ref != "" {
				gotImages = append(gotImages, ref)
			}
		}
	}

	// An exact set, not a shape heuristic. Two earlier attempts to infer
	// "is this tag pinned?" from the string both had holes — the first accepted
	// "jenkins/inbound-agent:jdk21", the second accepted "1.2.3-snapshot" — and
	// a deny-list of moving words can never be complete, because the registry
	// owner chooses the words. Immutability is not derivable from a tag.
	//
	// Changing an image therefore requires changing approvedStarterImages, which
	// puts a human in the loop to assert it.
	approved := make(map[string]bool, len(approvedStarterImages))
	for _, img := range approvedStarterImages {
		approved[img] = true
	}
	seen := make(map[string]bool, len(gotImages))
	for _, img := range gotImages {
		seen[img] = true
		if !approved[img] {
			t.Errorf("starter content references unapproved image %q; if this is intentional, "+
				"add it to approvedStarterImages after confirming the ref is immutable and "+
				"mirrorable for air-gapped installs", img)
		}
	}
	for _, img := range approvedStarterImages {
		if !seen[img] {
			t.Errorf("approvedStarterImages lists %q, which the starter content no longer uses; "+
				"remove it so the list keeps meaning what it says", img)
		}
	}
}

// approvedStarterImages is the exact set of container images the starter bundle
// may reference. Refresh alongside the plugin lock; see bundles/AGENTS.md.
// Prefer an @sha256 digest when the upstream publishes one you can pin to.
var approvedStarterImages = []string{
	"jenkins/inbound-agent:3384.v60d89463d9e0-1-jdk21",
}

// stripYAMLComments drops whole-line comments. Good enough for the shape checks
// above, which only need to tell a real key from an explanatory one.
func stripYAMLComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// starterItemLookup adapts the seeded store to the composer's lookup seam.
type starterItemLookup struct{ store crdstore.Backend }

func (l starterItemLookup) GetCatalogItemCRD(ctx context.Context, name, namespace string) (*v1alpha1.CatalogItem, error) {
	return crdstore.Get[v1alpha1.CatalogItem](ctx, l.store, name, namespace)
}

// The seeder and the composer are separate code paths, and seeding something
// the composer then rejects would leave every zero-config Controller waiting in
// Provisioning with no obvious cause. Compose what was seeded and assert it
// produces usable content.
func TestStarterSeededObjectsCompose(t *testing.T) {
	ctx := context.Background()
	r, store := newStarterTestReconciler()
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cb, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, store, StarterBundleName, "varroa-system")
	if err != nil {
		t.Fatalf("get bundle: %v", err)
	}

	composer := bundle.NewComposer(starterItemLookup{store}, nil, t.TempDir(), "", "", "", "varroa-system")
	res, err := composer.Compose(ctx, "varroa-system", &cb.Spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("compose errors: %v", res.Errors)
	}
	if len(res.Missing) > 0 {
		t.Fatalf("compose could not resolve: %v", res.Missing)
	}
	if strings.TrimSpace(res.Materialized.JenkinsYAML) == "" {
		t.Error("composed jenkins.yaml is empty")
	}
	if strings.TrimSpace(res.Materialized.ItemsYAML) == "" {
		t.Error("composed items.yaml is empty")
	}
	// The starter must not contribute a plugin set; the version-profile lock does.
	if strings.Contains(stripYAMLComments(res.Materialized.PluginsYAML), "artifactId") {
		t.Errorf("composed plugins.yaml pins plugins: %s", res.Materialized.PluginsYAML)
	}
}

// applyFilters previously read spec.composedBundleRef directly, so a
// `bundle: varroa-starter` operation silently matched none of the controllers
// actually running the starter — filters drop rather than report, so the
// operation reported success over an empty target set.
func TestBundleFilterMatchesZeroConfigControllers(t *testing.T) {
	zeroConfig := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "team-a"},
	}
	named := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "named", Namespace: "team-a"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "platform-baseline"},
		},
	}
	targets := []ResolvedTarget{
		{Namespace: "team-a", Name: "bare", Applicable: true, Controller: zeroConfig},
		{Namespace: "team-a", Name: "named", Applicable: true, Controller: named},
	}

	starter := v1alpha1.StarterBundleName
	got := applyFilters(targets, &v1alpha1.BroodTargetFilters{Bundle: &starter}, "varroa-system")
	if len(got) != 1 || got[0].Name != "bare" {
		t.Fatalf("bundle=%s should match exactly the zero-config controller, got %+v", starter, got)
	}

	other := "platform-baseline"
	got = applyFilters(targets, &v1alpha1.BroodTargetFilters{Bundle: &other}, "varroa-system")
	if len(got) != 1 || got[0].Name != "named" {
		t.Fatalf("bundle=%s should match exactly the named controller, got %+v", other, got)
	}
}

// A sha256 hex digest is 64 characters and Kubernetes caps label VALUES at 63,
// so carrying the content hash as a label made the API server reject every
// seeded object — no CatalogItem was ever created, and the bundle composed to
// Invalid forever. crdstore.Fake does not validate label length, so the whole
// suite passed while the feature was dead on a real cluster.
//
// This asserts the constraint the Fake does not.
func TestStarterObjectLabelsAreValidForKubernetes(t *testing.T) {
	ctx := context.Background()
	r, store := newStarterTestReconciler()
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	check := func(kind string, labels map[string]string) {
		t.Helper()
		for k, v := range labels {
			for _, msg := range validation.IsValidLabelValue(v) {
				t.Errorf("%s label %q value %q is invalid for Kubernetes: %s", kind, k, v, msg)
			}
			for _, msg := range validation.IsQualifiedName(k) {
				t.Errorf("%s label key %q is invalid for Kubernetes: %s", kind, k, msg)
			}
		}
	}

	for _, name := range []string{starterJCasCItemName, starterItemsItemName} {
		item, err := crdstore.Get[v1alpha1.CatalogItem](ctx, store, name, "varroa-system")
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		check("CatalogItem "+name, item.Labels)
	}
	cb, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, store, StarterBundleName, "varroa-system")
	if err != nil {
		t.Fatalf("get bundle: %v", err)
	}
	check("ComposedBundle", cb.Labels)
}
