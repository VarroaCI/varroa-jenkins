package controller

import (
	"context"
	"errors"
	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/bundles"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// Reserved names for the built-in starter bundle. A Controller that names no
// ComposedBundle resolves StarterBundleName in the operator namespace, so these
// names are a contract: renaming one orphans every zero-config Controller.
const (
	// StarterBundleName is the ComposedBundle a nil spec.composedBundleRef
	// resolves to, in the operator's namespace. Defined in the API package
	// because the BFF and brood-operation filtering need the same answer.
	StarterBundleName = v1alpha1.StarterBundleName

	starterJCasCItemName = "varroa-starter-jcasc"
	starterItemsItemName = "varroa-starter-items"

	// starterSourceRef is recorded as the items' spec.sourceRef. No
	// CatalogSource by this name exists and none is created: sourceRef is
	// provenance for the UI, and the composer resolves itemRefs by name and
	// namespace without consulting it.
	starterSourceRef = "varroa-builtin"

	// starterManagedLabel marks the objects this reconciler owns. It is the
	// whole ownership predicate — these objects have no owning CR, because
	// their source is the operator binary rather than any cluster resource.
	starterManagedLabel = "varroa.dev/starter"

	// starterHashAnnotation carries sha256 of the embedded content that produced
	// the object, so a tick that would write the same bytes can skip the write.
	//
	// An ANNOTATION, not a label: a sha256 hex digest is 64 characters and
	// Kubernetes caps label VALUES at 63, so the API server rejected the whole
	// object and no CatalogItem was ever created. Nothing selects on this, so a
	// label bought nothing and cost everything. Annotations have no such limit.
	starterHashAnnotation = "varroa.dev/starter-hash"
)

// StarterReconciler seeds the built-in starter bundle into the operator's
// namespace: two CatalogItems holding the embedded content, and a ComposedBundle
// composing them.
//
// It writes CatalogItem status itself. That is normally the CatalogSource
// reconciler's job, but the starter content comes from the binary rather than a
// synced source — there is no clone to observe, so there is nothing to sync.
// The ComposedBundle needs no such special case: the catalog ticker already
// reconciles every ComposedBundle cluster-wide and will compose this one like
// any other.
type StarterReconciler struct {
	store             crdstore.Backend
	operatorNamespace string
	logger            *slog.Logger
}

// NewStarterReconciler creates a StarterReconciler seeding into operatorNamespace.
func NewStarterReconciler(store crdstore.Backend, operatorNamespace string, logger *slog.Logger) *StarterReconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &StarterReconciler{store: store, operatorNamespace: operatorNamespace, logger: logger}
}

// Reconcile seeds or repairs the starter bundle. It is idempotent and safe to
// call on a ticker: unchanged objects are skipped without a write, and objects
// that exist but are not ours are left alone.
func (r *StarterReconciler) Reconcile(ctx context.Context) error {
	if r.operatorNamespace == "" {
		return errors.New("starter: operator namespace is unset")
	}

	var errs []error
	for _, item := range r.desiredItems() {
		if err := r.applyItem(ctx, item); err != nil {
			errs = append(errs, err)
		}
	}
	if err := r.applyBundle(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// desiredItems builds the CatalogItems from the embedded content. One item per
// content type: the composer routes an item's status.content by spec.type, so a
// single item cannot carry both JCasC and job definitions.
func (r *StarterReconciler) desiredItems() []*v1alpha1.CatalogItem {
	return []*v1alpha1.CatalogItem{
		r.newItem(starterJCasCItemName, v1alpha1.CatalogItemJCasC,
			"Starter JCasC",
			"Built-in Jenkins configuration: system message and a Kubernetes cloud.",
			"bundles/starter/jenkins.yaml", bundles.StarterJCasC()),
		r.newItem(starterItemsItemName, v1alpha1.CatalogItemItem,
			"Starter items",
			"Built-in sample pipeline proving the bundle path end to end.",
			"bundles/starter/items.yaml", bundles.StarterItems()),
	}
}

func (r *StarterReconciler) newItem(name string, typ v1alpha1.CatalogItemType, display, description, path, content string) *v1alpha1.CatalogItem {
	item := &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   r.operatorNamespace,
			Labels:      map[string]string{starterManagedLabel: "true"},
			Annotations: map[string]string{starterHashAnnotation: sha256Hex([]byte(content))},
		},
		Spec: v1alpha1.CatalogItemSpec{
			SourceRef:   starterSourceRef,
			Type:        typ,
			DisplayName: display,
			Description: description,
			Path:        path,
			Tags:        []string{"starter", "built-in"},
		},
	}
	item.Status = v1alpha1.CatalogItemStatus{
		Content:     content,
		ContentHash: sha256Hex([]byte(content)),
		Valid:       true,
	}
	return item
}

// applyItem writes one CatalogItem and its status, skipping both when the live
// object already carries the same content hash.
func (r *StarterReconciler) applyItem(ctx context.Context, item *v1alpha1.CatalogItem) error {
	live, err := crdstore.Get[v1alpha1.CatalogItem](ctx, r.store, item.Name, item.Namespace)
	if err == nil && live != nil {
		if !starterOwned(live.Labels) {
			r.logger.Warn("skipping catalog item not managed by the starter seeder",
				"item", item.Namespace+"/"+item.Name)
			return nil
		}
		if live.Annotations[starterHashAnnotation] == item.Annotations[starterHashAnnotation] &&
			live.Status.ContentHash == item.Status.ContentHash && live.Status.Valid {
			return nil
		}
	}

	status := item.Status
	if err := crdstore.ApplyOwned(ctx, r.store, item, func(l *unstructured.Unstructured) bool {
		return starterOwned(l.GetLabels())
	}); err != nil {
		if errors.Is(err, crdstore.ErrNotOwned) {
			r.logger.Warn("skipping catalog item not managed by the starter seeder",
				"item", item.Namespace+"/"+item.Name)
			return nil
		}
		return err
	}
	return crdstore.PatchStatus[v1alpha1.CatalogItem](ctx, r.store, item.Name, item.Namespace, &status)
}

// applyBundle writes the ComposedBundle referencing both starter items.
//
// The itemRefs name the operator namespace explicitly. Leaving it unset would
// let a same-named CatalogItem in the ComposedBundle's own namespace shadow the
// built-in one, which for a bundle whose whole purpose is to be predictable is
// the wrong default.
func (r *StarterReconciler) applyBundle(ctx context.Context) error {
	cb := &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name:      StarterBundleName,
			Namespace: r.operatorNamespace,
			Labels:    map[string]string{starterManagedLabel: "true"},
		},
		Spec: v1alpha1.ComposedBundleSpec{
			DisplayName: "Varroa starter",
			Description: "Built-in bundle used by Controllers that name no composedBundleRef.",
			Inputs: []v1alpha1.ComposedInput{
				{ItemRef: &v1alpha1.ComposedItemRef{Name: starterJCasCItemName, Namespace: r.operatorNamespace}},
				{ItemRef: &v1alpha1.ComposedItemRef{Name: starterItemsItemName, Namespace: r.operatorNamespace}},
			},
		},
	}

	live, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, r.store, cb.Name, cb.Namespace)
	if err == nil && live != nil {
		if !starterOwned(live.Labels) {
			r.logger.Warn("skipping composed bundle not managed by the starter seeder",
				"bundle", cb.Namespace+"/"+cb.Name)
			return nil
		}
		if sameStarterInputs(live.Spec.Inputs, cb.Spec.Inputs) {
			return nil
		}
	}

	if err := crdstore.ApplyOwned(ctx, r.store, cb, func(l *unstructured.Unstructured) bool {
		return starterOwned(l.GetLabels())
	}); err != nil {
		if errors.Is(err, crdstore.ErrNotOwned) {
			r.logger.Warn("skipping composed bundle not managed by the starter seeder",
				"bundle", cb.Namespace+"/"+cb.Name)
			return nil
		}
		return err
	}
	return nil
}

// starterOwned is THE ownership predicate for seeded objects, used by the
// pre-read skip and by ApplyOwned's guard. Two predicates would eventually
// disagree.
func starterOwned(labels map[string]string) bool {
	return labels[starterManagedLabel] == "true"
}

// sameStarterInputs compares only what this reconciler sets, so a user field it
// never writes cannot cause a rewrite loop.
func sameStarterInputs(live, want []v1alpha1.ComposedInput) bool {
	if len(live) != len(want) {
		return false
	}
	for i := range want {
		lr, wr := live[i].ItemRef, want[i].ItemRef
		if lr == nil || wr == nil {
			return false
		}
		if lr.Name != wr.Name || lr.Namespace != wr.Namespace {
			return false
		}
	}
	return true
}
