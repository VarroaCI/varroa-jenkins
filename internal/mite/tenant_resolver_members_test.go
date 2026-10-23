package mite

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// seedMemberTenant seeds a placed Tenant whose member list names three cells:
// slot 0, slot 1, and one standby. The scalar fields describe slot 0.
func seedMemberTenant(t *testing.T, store *crdstore.Fake) {
	t.Helper()
	slot0, slot1 := int32(0), int32(1)
	tm := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{Namespaces: []string{testCellNamespace}},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{
				Cluster:       testCellCluster,
				Cell:          "cell-a",
				CellNamespace: testCellNamespace,
				Generation:    4,
				Members: []v1alpha1.TenantMemberStatus{
					{Cell: "cell-a", CellNamespace: testCellNamespace, Role: v1alpha1.TenantMemberRoleActive, Slot: &slot0, Applied: true},
					{Cell: "cell-b", CellNamespace: testCellNamespace, Role: v1alpha1.TenantMemberRoleActive, Slot: &slot1, Applied: true},
					{Cell: "cell-c", CellNamespace: testCellNamespace, Role: v1alpha1.TenantMemberRoleStandby, Applied: true},
				},
			},
		},
	}
	crdstore.MustSeed(store, tm)
}

// TestCellTenantResolver_SecondSlotResolves asserts a cell holding slot 1
// answers the tenant. Without this the gateway refuses that cell's claims,
// its log chunks, and its cross-tenant calls before the fence can answer.
func TestCellTenantResolver_SecondSlotResolves(t *testing.T) {
	store := crdstore.NewFake()
	seedCellController(t, store, "cell-b", true)
	seedMemberTenant(t, store)
	r := NewCellTenantResolver(store)

	tenant, gen := r.ResolveJobTenant(testCellCluster, testCellNamespace, "cell-b", "payments/build")
	if tenant != "payments" || gen != 4 {
		t.Fatalf("tenant/gen = %q/%d, want payments/4", tenant, gen)
	}
}

// TestCellTenantResolver_StandbyResolves asserts a standby's cell answers
// the tenant, so the claim fence can answer NOT_HOME and the caller learns
// the reason.
func TestCellTenantResolver_StandbyResolves(t *testing.T) {
	store := crdstore.NewFake()
	seedCellController(t, store, "cell-c", true)
	seedMemberTenant(t, store)
	r := NewCellTenantResolver(store)

	tenant, gen := r.ResolveJobTenant(testCellCluster, testCellNamespace, "cell-c", "payments/build")
	if tenant != "payments" || gen != 4 {
		t.Fatalf("tenant/gen = %q/%d, want payments/4", tenant, gen)
	}
}

// TestCellTenantResolver_ACellOfAnotherTenantStillFailsClosed asserts a
// cell that holds no member of the named tenant answers an empty tenant.
func TestCellTenantResolver_ACellOfAnotherTenantStillFailsClosed(t *testing.T) {
	store := crdstore.NewFake()
	seedCellController(t, store, "cell-z", true)
	seedMemberTenant(t, store)
	r := NewCellTenantResolver(store)

	tenant, gen := r.ResolveJobTenant(testCellCluster, testCellNamespace, "cell-z", "payments/build")
	if tenant != "" || gen != 0 {
		t.Fatalf("tenant/gen = %q/%d, want an empty tenant", tenant, gen)
	}
}

// TestCellTenantResolver_ACellInAnotherNamespaceFailsClosed asserts the
// member key carries the namespace as well.
func TestCellTenantResolver_ACellInAnotherNamespaceFailsClosed(t *testing.T) {
	store := crdstore.NewFake()
	seedCellController(t, store, "cell-b", true)
	seedMemberTenant(t, store)
	r := NewCellTenantResolver(store)

	tenant, _ := r.ResolveJobTenant(testCellCluster, "other-ns", "cell-b", "payments/build")
	if tenant != "" {
		t.Fatalf("tenant = %q, want an empty tenant for a cell in another namespace", tenant)
	}
}

// TestCellTenantResolver_EmptyMemberListReadsAsTheScalarCell asserts a
// tenant the operator has not re-announced since the upgrade resolves on
// its scalar cell alone.
func TestCellTenantResolver_EmptyMemberListReadsAsTheScalarCell(t *testing.T) {
	store := crdstore.NewFake()
	seedCellController(t, store, "cell1", true)
	seedCellController(t, store, "cell2", true)
	seedPlacedTenant(t, store, "payments", testCellNamespace, "cell1", 4)
	r := NewCellTenantResolver(store)

	if tenant, _ := r.ResolveJobTenant(testCellCluster, testCellNamespace, "cell1", "payments/build"); tenant != "payments" {
		t.Fatalf("tenant = %q, want payments on the scalar cell", tenant)
	}
	if tenant, _ := r.ResolveJobTenant(testCellCluster, testCellNamespace, "cell2", "payments/build"); tenant != "" {
		t.Fatalf("tenant = %q, want an empty tenant on a cell the scalar does not name", tenant)
	}
}
