package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestListPlacedTenants_OneReadPerTenant is task 8.11a's acceptance: one Tenant
// read per name in status.cell.tenants, and a missing Tenant leaves every
// other tenant unaffected.
func TestListPlacedTenants_OneReadPerTenant(t *testing.T) {
	client := newTestClient()
	crdstore.MustSeed(client.store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{Namespaces: []string{"payments-ns"}},
	})
	crdstore.MustSeed(client.store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "search"},
		Spec:       v1alpha1.TenantSpec{Namespaces: []string{"search-ns"}},
	})
	// "ghost" is named in status.cell.tenants but has no backing Tenant.

	rec := newTestReconciler(client)
	cr := cellControllerFixture()
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"payments", "ghost", "search"}}

	tenants := rec.listPlacedTenants(context.Background(), cr)

	names := map[string]bool{}
	for _, tm := range tenants {
		names[tm.Name] = true
	}
	if len(tenants) != 2 || !names["payments"] || !names["search"] {
		t.Fatalf("expected exactly [payments search], got %+v", names)
	}
}

// TestListPlacedTenants_NilCellStatus asserts a cell with no Cell status yet
// reads no Tenant and does not panic.
func TestListPlacedTenants_NilCellStatus(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	tenants := rec.listPlacedTenants(context.Background(), cr)
	if len(tenants) != 0 {
		t.Errorf("expected no tenants, got %+v", tenants)
	}
}
