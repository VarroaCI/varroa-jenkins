package cells

import (
	"context"
	"sort"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

const (
	testOperatorNamespace = "varroa-system"
	testProfileName       = "lts-2-555"
	testResolveVersion    = "2.555.3"
)

func newProfile() *v1alpha1.JenkinsVersionProfile {
	return &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: testProfileName},
		Spec: v1alpha1.JenkinsVersionProfileSpec{
			Version:        "2.555",
			Channel:        "lts",
			ResolveVersion: testResolveVersion,
		},
	}
}

func pipelinesTeam() *v1alpha1.Team {
	return &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"default"},
			Pipelines: &v1alpha1.TeamPipelinesSpec{
				Profile:        testProfileName,
				AgentNamespace: "default",
			},
		},
	}
}

func cellControllerNamed(name string, warm bool, tenants []string) *v1alpha1.Controller {
	c := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testOperatorNamespace,
			Labels: map[string]string{
				LabelCellProfile: testProfileName,
			},
		},
		Spec: v1alpha1.ControllerSpec{
			Cell: &v1alpha1.CellSpec{Enabled: true},
		},
	}
	if warm {
		c.Labels[LabelWarm] = "true"
	}
	if len(tenants) > 0 {
		c.Status.Cell = &v1alpha1.CellStatus{Tenants: tenants}
	}
	return c
}

func listCells(t *testing.T, store crdstore.Backend) []*v1alpha1.Controller {
	t.Helper()
	all, err := crdstore.List[v1alpha1.Controller](context.Background(), store, testOperatorNamespace, "")
	if err != nil {
		t.Fatalf("list controllers: %v", err)
	}
	var out []*v1alpha1.Controller
	for _, c := range all {
		if c.Labels[LabelCellProfile] == testProfileName {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TestWarmPoolReconciler_Disabled covers task 7.1's acceptance: a
// pipelines Team creates no cell while the pool is disabled, and the
// reconciler returns before listing anything.
func TestWarmPoolReconciler_Disabled(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store,
		newProfile(),
		pipelinesTeam(),
	)

	r := &WarmPoolReconciler{
		Store:             store,
		OperatorNamespace: testOperatorNamespace,
		Config:            WarmPoolConfig{Enabled: false, PerProfile: 2},
	}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cells := listCells(t, store)
	if len(cells) != 0 {
		t.Fatalf("expected no cell while the pool is disabled, got %d", len(cells))
	}
}

// TestWarmPoolReconciler_FillsForRequestedProfile covers the "pool fills
// for a requested profile" scenario.
func TestWarmPoolReconciler_FillsForRequestedProfile(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store,
		newProfile(),
		pipelinesTeam(),
	)

	r := &WarmPoolReconciler{
		Store:             store,
		OperatorNamespace: testOperatorNamespace,
		Config:            WarmPoolConfig{Enabled: true, PerProfile: 2},
	}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cells := listCells(t, store)
	if len(cells) != 2 {
		t.Fatalf("expected 2 cells, got %d: %v", len(cells), cellNames(cells))
	}
	wantNames := []string{"cell-lts-2-555-0", "cell-lts-2-555-1"}
	for i, want := range wantNames {
		if cells[i].Name != want {
			t.Errorf("cell[%d].Name = %q, want %q", i, cells[i].Name, want)
		}
		if cells[i].Spec.Version != "2.555.3" {
			t.Errorf("cell[%d].Spec.Version = %q, want %q", i, cells[i].Spec.Version, "2.555.3")
		}
		if cells[i].Spec.Cell == nil || !cells[i].Spec.Cell.Enabled {
			t.Errorf("cell[%d].Spec.Cell.Enabled = false, want true", i)
		}
		if cells[i].Namespace != testOperatorNamespace {
			t.Errorf("cell[%d].Namespace = %q, want %q", i, cells[i].Namespace, testOperatorNamespace)
		}
		if cells[i].Labels[LabelWarm] != "true" {
			t.Errorf("cell[%d] missing varroa.dev/warm=true label", i)
		}
	}
}

// TestWarmPoolReconciler_RefillsAfterAssignment covers the "pool refills
// after an assignment" scenario: the only free cell of a profile gets a
// tenant, and the next pass creates one more free cell of that profile.
func TestWarmPoolReconciler_RefillsAfterAssignment(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store,
		newProfile(),
		pipelinesTeam(),
		cellControllerNamed("cell-lts-2-555-0", true, []string{"team-a"}),
	)

	r := &WarmPoolReconciler{
		Store:             store,
		OperatorNamespace: testOperatorNamespace,
		Config:            WarmPoolConfig{Enabled: true, PerProfile: 1},
	}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cells := listCells(t, store)
	if len(cells) != 2 {
		t.Fatalf("expected the assigned cell plus one new free cell, got %d: %v", len(cells), cellNames(cells))
	}
	if cells[1].Name != "cell-lts-2-555-1" {
		t.Errorf("expected the refill to use the next free ordinal, got %q", cells[1].Name)
	}
	if cells[1].Status.Cell != nil && len(cells[1].Status.Cell.Tenants) != 0 {
		t.Errorf("new cell should hold no tenant")
	}
}

func cellNames(cells []*v1alpha1.Controller) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = c.Name
	}
	return out
}

// TestWarmPoolReconciler_SurplusDeleted covers the "a surplus free cell is
// deleted" scenario: three free cells of one profile, perProfile 1, deletes
// two, keeping the lowest ordinal.
func TestWarmPoolReconciler_SurplusDeleted(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store,
		newProfile(),
		pipelinesTeam(),
		cellControllerNamed("cell-lts-2-555-0", true, nil),
		cellControllerNamed("cell-lts-2-555-1", true, nil),
		cellControllerNamed("cell-lts-2-555-2", true, nil),
	)

	r := &WarmPoolReconciler{
		Store:             store,
		OperatorNamespace: testOperatorNamespace,
		Config:            WarmPoolConfig{Enabled: true, PerProfile: 1},
	}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cells := listCells(t, store)
	if len(cells) != 1 {
		t.Fatalf("expected 1 surviving cell, got %d: %v", len(cells), cellNames(cells))
	}
	if cells[0].Name != "cell-lts-2-555-0" {
		t.Errorf("expected the lowest ordinal to survive, got %q", cells[0].Name)
	}
}

// TestWarmPoolReconciler_NeverDeletesAssignedCell covers the "a cell that
// holds a tenant is never deleted by the pool" scenario, whatever
// perProfile says.
func TestWarmPoolReconciler_NeverDeletesAssignedCell(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store,
		newProfile(),
		pipelinesTeam(),
		cellControllerNamed("cell-lts-2-555-0", false, []string{"team-a"}),
		cellControllerNamed("cell-lts-2-555-1", false, []string{"team-b"}),
	)

	r := &WarmPoolReconciler{
		Store:             store,
		OperatorNamespace: testOperatorNamespace,
		Config:            WarmPoolConfig{Enabled: true, PerProfile: 0},
	}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cells := listCells(t, store)
	if len(cells) != 2 {
		t.Fatalf("expected both assigned cells to survive, got %d: %v", len(cells), cellNames(cells))
	}
}

// TestWarmPoolReconciler_NoProfileRequested covers the "no profile is
// requested" scenario.
func TestWarmPoolReconciler_NoProfileRequested(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile())

	r := &WarmPoolReconciler{
		Store:             store,
		OperatorNamespace: testOperatorNamespace,
		Config:            WarmPoolConfig{Enabled: true, PerProfile: 3},
	}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cells := listCells(t, store)
	if len(cells) != 0 {
		t.Fatalf("expected no cell when no Team requests a profile, got %d", len(cells))
	}
}

// TestWarmPoolReconciler_PlacedTenantProfileFollowsCell asserts step 1's
// "plus every profile a placed tenant currently uses": a Team's placement
// keeps the target profile's pool filled by reading the bound cell's
// label, even when nothing else in this pass names the profile directly.
func TestWarmPoolReconciler_PlacedTenantProfileFollowsCell(t *testing.T) {
	store := crdstore.NewFake()
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec:       v1alpha1.TeamSpec{Namespaces: []string{"default"}},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{
				Cell:          "cell-lts-2-555-0",
				CellNamespace: testOperatorNamespace,
			},
		},
	}
	crdstore.MustSeed(store,
		newProfile(),
		team,
		cellControllerNamed("cell-lts-2-555-0", false, []string{"team-a"}),
	)

	r := &WarmPoolReconciler{
		Store:             store,
		OperatorNamespace: testOperatorNamespace,
		Config:            WarmPoolConfig{Enabled: true, PerProfile: 1},
	}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cells := listCells(t, store)
	if len(cells) != 2 {
		t.Fatalf("expected the assigned cell plus one new free cell, got %d: %v", len(cells), cellNames(cells))
	}
}
