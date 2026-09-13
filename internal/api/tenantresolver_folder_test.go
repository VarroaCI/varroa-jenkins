package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestResolve_OneFolderAcrossEveryRef asserts the assumption the pipelines
// list envelope rests on: every TenantRef one team resolves to carries the
// same Folder, so one tenantFolder field describes the whole response. It
// covers the three shapes that return refs, and the fourth shape, an
// ambiguous controller name, which returns 409 and no ref.
func TestResolve_OneFolderAcrossEveryRef(t *testing.T) {
	t.Run("a placed Team carries the Team name as its folder", func(t *testing.T) {
		client := newFakeResourceClient()
		client.controllers = map[string]*v1alpha1.Controller{
			"cell-1": {ObjectMeta: metav1.ObjectMeta{Name: "cell-1", Namespace: "test-ns"}},
		}
		seedTeam(t, client.crdStore, placedTeamObject("payments", "core", "test-ns", "cell-1"))

		refs := resolveRefs(t, client, resolverAuthorizer("payments"), "payments")
		assertOneFolder(t, refs, "payments")
	})

	t.Run("a namespace fan-out carries the empty folder on every ref", func(t *testing.T) {
		client := newFakeResourceClient()
		client.controllers = map[string]*v1alpha1.Controller{
			"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "ns-a"}},
			"ctrl-b": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-b", Namespace: "ns-b"}},
		}
		seedTeam(t, client.crdStore, unplacedTeamObject("payments", "ns-a", "ns-b"))

		refs := resolveRefs(t, client, namespaceAuthorizer("ns-a", "ns-b"), "payments")
		if len(refs) != 2 {
			t.Fatalf("refs = %d, want 2", len(refs))
		}
		assertOneFolder(t, refs, "")
	})

	t.Run("a synthetic single-controller tenant carries the empty folder", func(t *testing.T) {
		client := newFakeResourceClient()
		client.controllers = map[string]*v1alpha1.Controller{
			"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "ns-a"}},
		}

		refs := resolveRefs(t, client, namespaceAuthorizer("ns-a"), "ctrl-a")
		assertOneFolder(t, refs, "")
	})

	t.Run("an ambiguous controller name answers 409 and no ref", func(t *testing.T) {
		client := newFakeResourceClient()
		client.controllers = map[string]*v1alpha1.Controller{
			"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "ns-a"}},
		}
		brood := fakeResolverBrood{controllers: []ClusterController{
			clusterController("core", "ns-a", "ctrl-a"),
			clusterController("edge", "ns-a", "ctrl-a"),
		}}
		store := storeFromFake(client)
		resolver := NewTeamTenantResolver(store, brood, namespaceAuthorizer("ns-a"))

		refs, err := resolver.Resolve(context.Background(), resolverClaims, "ctrl-a")
		if len(refs) != 0 {
			t.Fatalf("refs = %+v, want none", refs)
		}
		var serr *ServiceError
		if !errors.As(err, &serr) || serr.Status != http.StatusConflict {
			t.Fatalf("err = %v, want a 409 ServiceError", err)
		}
	})
}

// resolveRefs resolves team through a resolver built over the fake client.
func resolveRefs(t *testing.T, client *fakeResourceClient, authz *Authorizer, team string) []TenantRef {
	t.Helper()
	store := storeFromFake(client)
	resolver := NewTeamTenantResolver(store, newFakeBrood(client), authz)
	refs, err := resolver.Resolve(context.Background(), resolverClaims, team)
	if err != nil {
		t.Fatalf("resolve %q: %v", team, err)
	}
	if len(refs) == 0 {
		t.Fatalf("resolve %q returned no ref", team)
	}
	return refs
}

// assertOneFolder fails when any ref carries a folder other than want.
func assertOneFolder(t *testing.T, refs []TenantRef, want string) {
	t.Helper()
	for _, ref := range refs {
		if ref.Folder != want {
			t.Fatalf("ref %+v carries folder %q, want %q for every ref of one team", ref, ref.Folder, want)
		}
	}
}
