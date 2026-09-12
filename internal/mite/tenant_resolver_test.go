package mite

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// --- 11.3: CellTenantResolver's production implementation ---

// testCellCluster is the cluster identity every test in this file uses,
// matching the value ResolveJobTenant's cluster argument carries.
const testCellCluster = "core"

// testCellNamespace is the namespace every seeded Controller in this file
// uses.
const testCellNamespace = "ns1"

func seedCellController(t *testing.T, store *crdstore.Fake, name string, isCell bool) {
	t.Helper()
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testCellNamespace},
	}
	if isCell {
		cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	}
	crdstore.MustSeed(store, cr)
}

func seedPlacedTeam(t *testing.T, store *crdstore.Fake, name, cellNamespace, cell string, generation int64) {
	t.Helper()
	tm := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.TeamSpec{Namespaces: []string{cellNamespace}},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{
				Cluster:       testCellCluster,
				Cell:          cell,
				CellNamespace: cellNamespace,
				Generation:    generation,
			},
		},
	}
	crdstore.MustSeed(store, tm)
}

func TestCellTenantResolver_NonCellControllerKeepsCompoundTenant(t *testing.T) {
	store := crdstore.NewFake()
	seedCellController(t, store, "ctrl1", false)
	r := NewCellTenantResolver(store)

	tenant, gen := r.ResolveJobTenant("core", "ns1", "ctrl1", "some/job")
	if tenant != "ns1/ctrl1" || gen != 0 {
		t.Fatalf("tenant/gen = %q/%d, want ns1/ctrl1/0", tenant, gen)
	}
}

func TestCellTenantResolver_CellResolvesPlacedTenant(t *testing.T) {
	store := crdstore.NewFake()
	seedCellController(t, store, "cell1", true)
	seedPlacedTeam(t, store, "payments", "ns1", "cell1", 4)
	r := NewCellTenantResolver(store)

	tenant, gen := r.ResolveJobTenant("core", "ns1", "cell1", "payments/build")
	if tenant != "payments" || gen != 4 {
		t.Fatalf("tenant/gen = %q/%d, want payments/4", tenant, gen)
	}
}

func TestCellTenantResolver_UnknownTenantCases(t *testing.T) {
	store := crdstore.NewFake()
	seedCellController(t, store, "cell1", true)
	// team-nowhere exists but is not placed at all.
	crdstore.MustSeed(store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-nowhere"},
		Spec:       v1alpha1.TeamSpec{Namespaces: []string{"ns1"}},
	})
	// team-elsewhere is placed, but on a different cell.
	seedPlacedTeam(t, store, "team-elsewhere", "ns2", "cell2", 1)
	r := NewCellTenantResolver(store)

	cases := []struct {
		name    string
		jobPath string
	}{
		{"empty job path", ""},
		{"unregistered team", "no-such-team/build"},
		{"unplaced team", "team-nowhere/build"},
		{"placed on a different cell", "team-elsewhere/build"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenant, gen := r.ResolveJobTenant("core", "ns1", "cell1", tc.jobPath)
			if tenant != "" || gen != 0 {
				t.Fatalf("tenant/gen = %q/%d, want empty/0", tenant, gen)
			}
		})
	}
}

func TestCellTenantResolver_UnreadableControllerResolvesUnknown(t *testing.T) {
	store := crdstore.NewFake()
	// No Controller seeded at all: an absent or unreadable Controller
	// must not be guessed as "not a cell".
	r := NewCellTenantResolver(store)

	tenant, gen := r.ResolveJobTenant("core", "ns1", "ghost-cell", "payments/build")
	if tenant != "" || gen != 0 {
		t.Fatalf("tenant/gen = %q/%d, want empty/0 for an unreadable controller", tenant, gen)
	}
}

func TestCellTenantResolver_ResolveTenantAlwaysUnknownForACell(t *testing.T) {
	store := crdstore.NewFake()
	seedCellController(t, store, "cell1", true)
	seedPlacedTeam(t, store, "payments", "ns1", "cell1", 4)
	r := NewCellTenantResolver(store)

	// ResolveTenant carries no job path, so a cell can never answer with
	// a specific tenant through it.
	tenant, gen := r.ResolveTenant("core", "ns1", "cell1")
	if tenant != "" || gen != 0 {
		t.Fatalf("tenant/gen = %q/%d, want empty/0", tenant, gen)
	}
}

func TestCellTenantResolver_CachesAcrossCalls(t *testing.T) {
	store := crdstore.NewFake()
	seedCellController(t, store, "cell1", true)
	seedPlacedTeam(t, store, "payments", "ns1", "cell1", 1)
	r := NewCellTenantResolver(store)

	if tenant, _ := r.ResolveJobTenant("core", "ns1", "cell1", "payments/build"); tenant != "payments" {
		t.Fatalf("first resolve = %q, want payments", tenant)
	}

	// Delete both CRDs from the backend: a cached answer must still be
	// served for the next cellTenantResolveTTL, not re-fetched.
	ctrlGVR, err := crdstore.GVRFor[v1alpha1.Controller]()
	if err != nil {
		t.Fatalf("controller GVR: %v", err)
	}
	if err := store.DeleteObject(t.Context(), ctrlGVR, "ns1", "cell1"); err != nil {
		t.Fatalf("delete controller: %v", err)
	}
	teamGVR, err := crdstore.GVRFor[v1alpha1.Team]()
	if err != nil {
		t.Fatalf("team GVR: %v", err)
	}
	if err := store.DeleteObject(t.Context(), teamGVR, "", "payments"); err != nil {
		t.Fatalf("delete team: %v", err)
	}

	tenant, gen := r.ResolveJobTenant("core", "ns1", "cell1", "payments/build")
	if tenant != "payments" || gen != 1 {
		t.Fatalf("cached resolve = %q/%d, want payments/1", tenant, gen)
	}
}

// TestCellTenantResolver_MoveIsServedStaleWithinTTLThenRefreshesAfterExpiry
// proves D14's rule for a move: the resolver's 30-second cache
// (cellTenantResolveTTL) may serve a tenant's old generation for as long
// as the cache entry is live — the recorder's ledger claim fence
// (internal/recordstore/ledger.go's existingKeyDecision, which vetoes a
// claim whose placement_generation is lower than the stored one with
// ReasonFenced) is the mechanism that rejects a stale generation, not the
// gateway's cache. Once the cache entry's TTL passes, the next resolve
// re-reads the Team and reports the tenant's current placement.
func TestCellTenantResolver_MoveIsServedStaleWithinTTLThenRefreshesAfterExpiry(t *testing.T) {
	store := crdstore.NewFake()
	seedCellController(t, store, "cell1", true)
	seedPlacedTeam(t, store, "payments", "ns1", "cell1", 1)
	crdstore.MustSeed(store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell2", Namespace: "ns2"},
		Spec:       v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
	})
	resolver := NewCellTenantResolver(store)
	r := resolver.(*crdCellTenantResolver)

	tenant, gen := r.ResolveJobTenant("core", "ns1", "cell1", "payments/build")
	if tenant != "payments" || gen != 1 {
		t.Fatalf("first resolve = %q/%d, want payments/1", tenant, gen)
	}

	// The tenant moves: the backing store now names a new cell and a
	// higher generation while the resolver's cache entry is still live.
	seedPlacedTeam(t, store, "payments", "ns2", "cell2", 2)

	// Within the TTL, the resolver still answers from its cache: it
	// keeps reporting the tenant's old generation against its old cell.
	tenant, gen = r.ResolveJobTenant("core", "ns1", "cell1", "payments/build")
	if tenant != "payments" || gen != 1 {
		t.Fatalf("resolve before TTL expiry = %q/%d, want the stale cached payments/1", tenant, gen)
	}

	// Force the cache entry past cellTenantResolveTTL, standing in for
	// the passage of 30 real seconds.
	r.mu.Lock()
	entry := r.teams["payments"]
	entry.expires = time.Now().Add(-time.Second)
	r.teams["payments"] = entry
	r.mu.Unlock()

	// After expiry, the resolver re-reads: the old cell no longer hosts
	// the tenant.
	tenant, gen = r.ResolveJobTenant("core", "ns1", "cell1", "payments/build")
	if tenant != "" || gen != 0 {
		t.Fatalf("resolve after TTL expiry = %q/%d, want empty/0: the old cell no longer hosts payments", tenant, gen)
	}

	// The new cell resolves the refreshed generation.
	tenant, gen = r.ResolveJobTenant("core", "ns2", "cell2", "payments/build")
	if tenant != "payments" || gen != 2 {
		t.Fatalf("resolve on the new cell = %q/%d, want the refreshed payments/2", tenant, gen)
	}
}
