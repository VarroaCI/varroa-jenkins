package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// fakePlacementReader answers a fixed placement row set for every tenant,
// or a fixed error.
type fakePlacementReader struct {
	rows  map[string][]recordstore.TenantPlacement
	err   error
	calls int
}

func (f *fakePlacementReader) ListTenantPlacements(_ context.Context, tenant string) ([]recordstore.TenantPlacement, error) {
	f.calls++
	return f.rows[tenant], f.err
}

// localBrood is a fakeResolverBrood that names its local cluster.
type localBrood struct {
	fakeResolverBrood
	local string
}

func (b localBrood) LocalCluster() string { return b.local }

func TestTenantResolver_MemberPlacedTenantResolvesFromThePlacementRow(t *testing.T) {
	placements := &fakePlacementReader{rows: map[string][]recordstore.TenantPlacement{
		"shop": {{Cluster: "member", Namespace: "varroa-system", Cell: "cell-m-0", Generation: 1}},
	}}
	resolver := NewTenantTenantResolver(crdstore.NewFake(), localBrood{local: "core"}, resolverAuthorizer("shop"),
		WithMemberPlacements(placements))

	refs, err := resolver.Resolve(context.Background(), resolverClaims, "shop")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := TenantRef{Cluster: "member", Namespace: "varroa-system", Controller: "cell-m-0", Tenant: "shop", TenantName: "shop", Folder: "shop"}
	if len(refs) != 1 || refs[0] != want {
		t.Fatalf("refs = %+v, want [%+v]", refs, want)
	}
}

func TestTenantResolver_MemberPlacementIgnoresLocalAndEmptyRows(t *testing.T) {
	// A row of the local cluster belongs to a Tenant the local API server no
	// longer holds, and a row with no cell names no tenant home. Neither
	// resolves, so the name falls through to the synthetic step.
	placements := &fakePlacementReader{rows: map[string][]recordstore.TenantPlacement{
		"shop": {
			{Cluster: "core", Namespace: "varroa-system", Cell: "cell-c-0", Generation: 1},
			{Cluster: "member", Namespace: "varroa-system", Cell: "", Generation: 1},
		},
	}}
	resolver := NewTenantTenantResolver(crdstore.NewFake(), localBrood{local: "core"}, resolverAuthorizer("shop"),
		WithMemberPlacements(placements))

	_, err := resolver.Resolve(context.Background(), resolverClaims, "shop")
	assertNotFound(t, err)
}

func TestTenantResolver_MemberPlacementAtGenerationZeroDoesNotResolve(t *testing.T) {
	// A teardown resets the row's generation to 0 and keeps the row. The
	// row names no live tenant, so the name falls through to the
	// synthetic step and answers 404.
	placements := &fakePlacementReader{rows: map[string][]recordstore.TenantPlacement{
		"shop": {{Cluster: "member", Namespace: "varroa-system", Cell: "cell-m-0", Generation: 0}},
	}}
	resolver := NewTenantTenantResolver(crdstore.NewFake(), localBrood{local: "core"}, resolverAuthorizer("shop"),
		WithMemberPlacements(placements))

	_, err := resolver.Resolve(context.Background(), resolverClaims, "shop")
	assertNotFound(t, err)
}

func TestTenantResolver_MemberPlacementOnTwoClustersIs409(t *testing.T) {
	placements := &fakePlacementReader{rows: map[string][]recordstore.TenantPlacement{
		"shop": {
			{Cluster: "member-a", Namespace: "varroa-system", Cell: "cell-a-0", Generation: 1},
			{Cluster: "member-b", Namespace: "varroa-system", Cell: "cell-b-0", Generation: 1},
		},
	}}
	resolver := NewTenantTenantResolver(crdstore.NewFake(), localBrood{local: "core"}, resolverAuthorizer("shop"),
		WithMemberPlacements(placements))

	_, err := resolver.Resolve(context.Background(), resolverClaims, "shop")
	var serr *ServiceError
	if !errors.As(err, &serr) || serr.Status != http.StatusConflict {
		t.Fatalf("err = %v, want a 409 ServiceError", err)
	}
}

func TestTenantResolver_LocalTenantSkipsThePlacementRead(t *testing.T) {
	store := crdstore.NewFake()
	seedTenant(t, store, placedTenantObject("payments", "core", "varroa-cells", "cell-a"))
	placements := &fakePlacementReader{}
	resolver := NewTenantTenantResolver(store, failListAllBrood{t: t}, resolverAuthorizer("payments"),
		WithMemberPlacements(placements))

	if _, err := resolver.Resolve(context.Background(), resolverClaims, "payments"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if placements.calls != 0 {
		t.Errorf("placement reads = %d, want 0 for a Tenant the local API server holds", placements.calls)
	}
}

func TestTenantResolver_MemberPlacementReadErrorFails(t *testing.T) {
	placements := &fakePlacementReader{err: errors.New("boom")}
	resolver := NewTenantTenantResolver(crdstore.NewFake(), localBrood{local: "core"}, resolverAuthorizer("shop"),
		WithMemberPlacements(placements))

	if _, err := resolver.Resolve(context.Background(), resolverClaims, "shop"); err == nil {
		t.Fatal("Resolve: nil error, want the placement read error")
	}
}
