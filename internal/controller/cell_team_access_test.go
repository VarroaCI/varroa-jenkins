package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestListPlacedTeams_OneReadPerTenant is task 8.11a's acceptance: one Team
// read per name in status.cell.tenants, and a missing Team leaves every
// other tenant unaffected.
func TestListPlacedTeams_OneReadPerTenant(t *testing.T) {
	client := newTestClient()
	crdstore.MustSeed(client.store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TeamSpec{Namespaces: []string{"payments-ns"}},
	})
	crdstore.MustSeed(client.store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "search"},
		Spec:       v1alpha1.TeamSpec{Namespaces: []string{"search-ns"}},
	})
	// "ghost" is named in status.cell.tenants but has no backing Team.

	rec := newTestReconciler(client)
	cr := cellControllerFixture()
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"payments", "ghost", "search"}}

	teams := rec.listPlacedTeams(context.Background(), cr)

	names := map[string]bool{}
	for _, tm := range teams {
		names[tm.Name] = true
	}
	if len(teams) != 2 || !names["payments"] || !names["search"] {
		t.Fatalf("expected exactly [payments search], got %+v", names)
	}
}

// TestListPlacedTeams_NilCellStatus asserts a cell with no Cell status yet
// reads no Team and does not panic.
func TestListPlacedTeams_NilCellStatus(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	teams := rec.listPlacedTeams(context.Background(), cr)
	if len(teams) != 0 {
		t.Errorf("expected no teams, got %+v", teams)
	}
}
