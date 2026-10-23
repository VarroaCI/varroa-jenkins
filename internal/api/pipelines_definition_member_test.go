package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// memberTenantBrood answers GetTenantBundleContent from a map keyed by
// cluster/tenant. It records each member call. Every other ConfigBrood method
// is unused by these reads and panics if reached.
type memberTenantBrood struct {
	ConfigBrood
	replies map[string]memberTenantReply
	// items answers GetCatalogItem, keyed cluster/ns/name. itemErr, when
	// set, answers every GetCatalogItem call instead.
	items   map[string]*v1alpha1.CatalogItem
	itemErr error
	calls   []string
}

// GetCatalogItem answers from items. A miss is a member not-found.
func (f *memberTenantBrood) GetCatalogItem(_ context.Context, cluster, ns, name string) (json.RawMessage, error) {
	key := cluster + "/" + ns + "/" + name
	f.calls = append(f.calls, "item:"+key)
	if f.itemErr != nil {
		return nil, f.itemErr
	}
	item, ok := f.items[key]
	if !ok {
		return nil, &BroodError{Code: bus.CodeNotFound, Msg: "catalog item not found"}
	}
	return json.Marshal(item)
}

// memberCellBrood answers Get for a member cell from cells, keyed
// cluster/ns/name, and records each member call. A call for the local
// cluster is delegated to the embedded Brood.
type memberCellBrood struct {
	Brood
	cells map[string]*v1alpha1.Controller
	err   error
	calls []string
}

func (f *memberCellBrood) Get(ctx context.Context, cluster, ns, name string) (*v1alpha1.Controller, error) {
	if cluster == f.LocalCluster() {
		return f.Brood.Get(ctx, cluster, ns, name)
	}
	key := cluster + "/" + ns + "/" + name
	f.calls = append(f.calls, key)
	if f.err != nil {
		return nil, f.err
	}
	cr, ok := f.cells[key]
	if !ok {
		return nil, &BroodError{Code: bus.CodeNotFound, Msg: "cell not found"}
	}
	return cr, nil
}

type memberTenantReply struct {
	cb      *v1alpha1.ComposedBundle
	content map[string]string
	held    bool
	err     error
}

func (f *memberTenantBrood) GetTenantBundleContent(_ context.Context, cluster, tenant string) (*v1alpha1.ComposedBundle, map[string]string, bool, error) {
	key := cluster + "/" + tenant
	f.calls = append(f.calls, key)
	r, ok := f.replies[key]
	if !ok {
		return nil, nil, false, &BroodError{Code: bus.CodeNotFound, Msg: "tenant not found"}
	}
	return r.cb, r.content, r.held, r.err
}

// memberTenantRef is a placed Tenant that member-a holds and the core does not.
var memberTenantRef = TenantRef{
	Cluster: "member-a", Namespace: "cells", Controller: "cell-a",
	TenantName: "payments", Tenant: "payments", Folder: "payments",
}

func memberTenantServer(t *testing.T, brood *memberTenantBrood) *Server {
	t.Helper()
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{
		Store:             storeFromFake(client),
		Client:            client,
		Brood:             newFakeBrood(client),
		ConfigBrood:       brood,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		OperatorNamespace: "test-ns",
	})
	return srv
}

func memberBundle() *v1alpha1.ComposedBundle {
	return &v1alpha1.ComposedBundle{ObjectMeta: metav1.ObjectMeta{Name: "payments-bundle", Namespace: "cells"}}
}

func TestControllerItemManifest_MemberPlacedTenantReadsMemberBundle(t *testing.T) {
	brood := &memberTenantBrood{replies: map[string]memberTenantReply{
		"member-a/payments": {cb: memberBundle(), content: map[string]string{"items.yaml": tenantItemsYAML}},
	}}
	srv := memberTenantServer(t, brood)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), memberTenantRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest == nil || len(manifest.Items) != 1 {
		t.Fatalf("manifest = %+v, want one re-rooted folder", manifest)
	}
	root := manifest.Items[0]
	if root.Kind != "folder" || root.Name != "payments" || len(root.Items) == 0 {
		t.Fatalf("root = %s %q with %d items, want folder payments holding the member's items", root.Kind, root.Name, len(root.Items))
	}
}

func TestControllerItemManifest_MemberPlacedTenantUnknownKeyGivesNoManifest(t *testing.T) {
	brood := &memberTenantBrood{replies: map[string]memberTenantReply{
		"member-a/payments": {cb: memberBundle(), content: map[string]string{"items.yaml": tenantUnknownKeyItems}},
	}}
	srv := memberTenantServer(t, brood)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), memberTenantRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest != nil {
		t.Fatalf("manifest = %+v, want nil: the strict parse refuses an unknown key", manifest)
	}
}

func TestControllerItemManifest_MemberPlacedTenantVarInKeyKeepsManifest(t *testing.T) {
	brood := &memberTenantBrood{replies: map[string]memberTenantReply{
		"member-a/payments": {cb: memberBundle(), content: map[string]string{
			"items.yaml":     tenantVarInKeyItems,
			"variables.yaml": "field: builders\n",
		}},
	}}
	srv := memberTenantServer(t, brood)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), memberTenantRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest == nil || len(manifest.Items) != 1 {
		t.Fatalf("manifest = %+v, want one item after variable resolution", manifest)
	}
}

func TestTenantPipelineBundleContent_MemberNotFoundAnswersNil(t *testing.T) {
	brood := &memberTenantBrood{replies: map[string]memberTenantReply{
		"member-a/payments": {err: &BroodError{Code: bus.CodeNotFound, Msg: "tenant not found"}},
	}}
	srv := memberTenantServer(t, brood)

	cb, content, _, err := srv.tenantPipelineBundleContent(context.Background(), memberTenantRef)
	if cb != nil || content != nil || err != nil {
		t.Fatalf("got (%v, %v, %v), want nil, nil, nil", cb, content, err)
	}
}

func TestTenantPipelineBundleContent_MemberUnreachableReturnsError(t *testing.T) {
	brood := &memberTenantBrood{replies: map[string]memberTenantReply{
		"member-a/payments": {err: &ErrClusterUnreachable{Cluster: "member-a"}},
	}}
	srv := memberTenantServer(t, brood)

	if _, _, _, err := srv.tenantPipelineBundleContent(context.Background(), memberTenantRef); err == nil {
		t.Fatal("err = nil, want the unreachable member error")
	}
}

func TestTenantPipelineBundleContent_LocalClusterNotFoundDoesNotCallMember(t *testing.T) {
	brood := &memberTenantBrood{}
	srv := memberTenantServer(t, brood)

	localRef := memberTenantRef
	localRef.Cluster = "core"
	cb, content, _, err := srv.tenantPipelineBundleContent(context.Background(), localRef)
	if cb != nil || content != nil || err != nil {
		t.Fatalf("got (%v, %v, %v), want nil, nil, nil", cb, content, err)
	}
	if len(brood.calls) != 0 {
		t.Fatalf("member calls = %v, want none for the local cluster", brood.calls)
	}
}

// memberItemBundle is a member-placed Tenant's bundle with one itemRef input,
// payments-items, that the member resolves in catalog-ns.
func memberItemBundle() *v1alpha1.ComposedBundle {
	return &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-bundle", Namespace: "cells"},
		Spec: v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "payments-items"}},
		}},
		Status: v1alpha1.ComposedBundleStatus{
			InputSummary:      []v1alpha1.InputSummaryEntry{{Kind: "itemRef", Type: "item", Namespace: "catalog-ns"}},
			ObservedRevisions: map[string]string{"0": "sha-item"},
		},
	}
}

// memberTenantServerWithCells builds a server over client's local store, with
// brood as the ConfigBrood and cells as the Brood. The local calls go to a
// fake Brood over client. Seed client before the call.
func memberTenantServerWithCells(t *testing.T, client *fakeResourceClient, brood *memberTenantBrood, cells *memberCellBrood) *Server {
	t.Helper()
	cells.Brood = newFakeBrood(client)
	return NewServer(&Dependencies{
		Store:             storeFromFake(client),
		Client:            client,
		Brood:             cells,
		ConfigBrood:       brood,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		OperatorNamespace: "test-ns",
	})
}

// memberPaymentsItemBrood serves tenantItemsYAML as the member's bundle content
// and as the content of the member's payments-items CatalogItem.
func memberPaymentsItemBrood() *memberTenantBrood {
	return &memberTenantBrood{
		replies: map[string]memberTenantReply{
			"member-a/payments": {cb: memberItemBundle(), content: map[string]string{"items.yaml": tenantItemsYAML}},
		},
		items: map[string]*v1alpha1.CatalogItem{
			"member-a/catalog-ns/payments-items": {
				ObjectMeta: metav1.ObjectMeta{Name: "payments-items", Namespace: "catalog-ns"},
				Spec:       v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemItem},
				Status:     v1alpha1.CatalogItemStatus{Content: tenantItemsYAML},
			},
		},
	}
}

// TestFindDeclaredItem_MemberPlacedTenantReadsCatalogItemFromMember asserts the
// declaredBy itemRef is read on the member that holds the bundle, not on the
// core store, which holds a same-named item that declares another job.
func TestFindDeclaredItem_MemberPlacedTenantReadsCatalogItemFromMember(t *testing.T) {
	brood := memberPaymentsItemBrood()
	client := newFakeResourceClient()
	seedCatalogItem(client, "payments-items", "catalog-ns", "items:\n  - kind: folder\n    name: other\n")
	srv := memberTenantServerWithCells(t, client, brood, &memberCellBrood{})

	_, item, declaredBy, err := srv.findDeclaredItem(context.Background(), memberTenantRef, "payments/release/eu")
	if err != nil {
		t.Fatalf("findDeclaredItem: %v", err)
	}
	if item == nil {
		t.Fatal("item = nil, want the member's declared job")
	}
	want := &declaredByJSON{
		Kind: "itemRef", Name: "payments-items", Revision: "sha-item",
		Bundle:      bundleRefJSON{Namespace: "cells", Name: "payments-bundle"},
		CatalogItem: &catalogItemRefJSON{Name: "payments-items", Namespace: "catalog-ns"},
	}
	if !reflect.DeepEqual(declaredBy, want) {
		t.Fatalf("declaredBy = %+v, want %+v", declaredBy, want)
	}
	if !slices.Contains(brood.calls, "item:member-a/catalog-ns/payments-items") {
		t.Fatalf("member calls = %v, want the catalog item read on member-a", brood.calls)
	}
}

// TestFindDeclaredItem_MemberPlacedTenantReadsCellFromMember asserts the
// profile's cell is read on the member, not on the core store, which holds a
// same-named controller at another version.
func TestFindDeclaredItem_MemberPlacedTenantReadsCellFromMember(t *testing.T) {
	brood := memberPaymentsItemBrood()
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{"cell-a": {
		ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "cells"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.999"},
	}}
	cells := &memberCellBrood{cells: map[string]*v1alpha1.Controller{"member-a/cells/cell-a": {
		ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "cells"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.570"},
	}}}
	srv := memberTenantServerWithCells(t, client, brood, cells)

	cr, _, _, err := srv.findDeclaredItem(context.Background(), memberTenantRef, "payments/release/eu")
	if err != nil {
		t.Fatalf("findDeclaredItem: %v", err)
	}
	if cr == nil || cr.Spec.Version != "2.570" {
		t.Fatalf("controller = %+v, want the member cell at version 2.570", cr)
	}
}

// TestFindDeclaredItem_MemberCatalogItemNotFoundGivesNoDeclarer asserts a
// member item that is gone keeps the job and drops only its provenance.
func TestFindDeclaredItem_MemberCatalogItemNotFoundGivesNoDeclarer(t *testing.T) {
	brood := memberPaymentsItemBrood()
	brood.items = nil
	srv := memberTenantServerWithCells(t, newFakeResourceClient(), brood, &memberCellBrood{})

	_, item, declaredBy, err := srv.findDeclaredItem(context.Background(), memberTenantRef, "payments/release/eu")
	if err != nil {
		t.Fatalf("findDeclaredItem: %v", err)
	}
	if item == nil || declaredBy != nil {
		t.Fatalf("item = %v, declaredBy = %+v, want the job and no declarer", item, declaredBy)
	}
}

// TestFindDeclaredItem_MemberCatalogItemReadErrorGivesNoDeclarer asserts a
// failed member item read keeps the job, drops the declarer, and never names
// a git or OCI input in its place.
func TestFindDeclaredItem_MemberCatalogItemReadErrorGivesNoDeclarer(t *testing.T) {
	brood := memberPaymentsItemBrood()
	brood.itemErr = &ErrClusterUnreachable{Cluster: "member-a"}
	srv := memberTenantServerWithCells(t, newFakeResourceClient(), brood, &memberCellBrood{})

	_, item, declaredBy, err := srv.findDeclaredItem(context.Background(), memberTenantRef, "payments/release/eu")
	if err != nil {
		t.Fatalf("findDeclaredItem: %v", err)
	}
	if item == nil || declaredBy != nil {
		t.Fatalf("item = %v, declaredBy = %+v, want the job and no declarer", item, declaredBy)
	}
}

// TestControllerItemManifest_MemberCellNotFoundKeepsManifest asserts a member
// with no cell for the Tenant keeps the manifest.
func TestControllerItemManifest_MemberCellNotFoundKeepsManifest(t *testing.T) {
	brood := memberPaymentsItemBrood()
	srv := memberTenantServerWithCells(t, newFakeResourceClient(), brood, &memberCellBrood{})

	cr, _, manifest, err := srv.controllerItemManifest(context.Background(), memberTenantRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if cr != nil || manifest == nil {
		t.Fatalf("controller = %+v, manifest = %v, want no controller and a manifest", cr, manifest)
	}
}

// TestControllerItemManifest_MemberCellReadErrorKeepsManifest asserts a failed
// member cell read keeps the manifest and leaves the controller empty.
func TestControllerItemManifest_MemberCellReadErrorKeepsManifest(t *testing.T) {
	brood := memberPaymentsItemBrood()
	cells := &memberCellBrood{err: &ErrClusterUnreachable{Cluster: "member-a"}}
	srv := memberTenantServerWithCells(t, newFakeResourceClient(), brood, cells)

	cr, _, manifest, err := srv.controllerItemManifest(context.Background(), memberTenantRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if cr != nil || manifest == nil {
		t.Fatalf("controller = %+v, manifest = %v, want no controller and a manifest", cr, manifest)
	}
}

// TestControllerItemManifest_LocalClusterTenantNeverReadsMemberCell asserts a
// Tenant on the local cluster never reads a member cell.
func TestControllerItemManifest_LocalClusterTenantNeverReadsMemberCell(t *testing.T) {
	brood := memberPaymentsItemBrood()
	cells := &memberCellBrood{}
	srv := memberTenantServerWithCells(t, newFakeResourceClient(), brood, cells)

	localRef := memberTenantRef
	localRef.Cluster = "core"
	if _, _, _, err := srv.controllerItemManifest(context.Background(), localRef); err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if len(cells.calls) != 0 {
		t.Fatalf("member cell calls = %v, want none for the local cluster", cells.calls)
	}
}
