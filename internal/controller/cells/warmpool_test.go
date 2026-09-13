package cells

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

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

// newPool returns a hand-applied pool for the test profile. The pool name
// equals the profile name, so it is that profile's default pool.
func newPool(warm int32) *v1alpha1.CellPool {
	return &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: testProfileName},
		Spec: v1alpha1.CellPoolSpec{
			ProfileRef:        testProfileName,
			Warm:              warm,
			MaxTenantsPerCell: 10,
		},
	}
}

// poolCellNamed returns a cell owned by pool, carrying the three pool labels.
func poolCellNamed(name, pool string, tenants []string) *v1alpha1.Controller {
	c := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testOperatorNamespace,
			Labels: map[string]string{
				LabelCellProfile: testProfileName,
				LabelCellPool:    pool,
				LabelWarm:        "true",
			},
		},
		Spec: v1alpha1.ControllerSpec{
			Version: testResolveVersion,
			Cell:    &v1alpha1.CellSpec{Enabled: true},
		},
	}
	if len(tenants) > 0 {
		c.Status.Cell = &v1alpha1.CellStatus{Tenants: tenants}
	}
	return c
}

// unlabelledCellNamed returns a cell of the test profile carrying no pool
// label, the shape adoption looks for.
func unlabelledCellNamed(name string, tenants []string) *v1alpha1.Controller {
	c := poolCellNamed(name, "", tenants)
	delete(c.Labels, LabelCellPool)
	return c
}

func newReconciler(store *crdstore.Fake, cfg WarmPoolConfig) *WarmPoolReconciler {
	return &WarmPoolReconciler{
		Store:             store,
		OperatorNamespace: testOperatorNamespace,
		Config:            cfg,
		Now:               func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}
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

func cellNames(cells []*v1alpha1.Controller) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = c.Name
	}
	return out
}

func getPool(t *testing.T, store crdstore.Backend, name string) *v1alpha1.CellPool {
	t.Helper()
	pool, err := crdstore.Get[v1alpha1.CellPool](context.Background(), store, name, "")
	if err != nil {
		t.Fatalf("get CellPool %s: %v", name, err)
	}
	return pool
}

func poolReady(t *testing.T, pool *v1alpha1.CellPool) *metav1.Condition {
	t.Helper()
	for i := range pool.Status.Conditions {
		if pool.Status.Conditions[i].Type == v1alpha1.CellPoolConditionPoolReady {
			return &pool.Status.Conditions[i]
		}
	}
	return nil
}

// TestCellPool_FillsToWarm covers "The pool fills to its warm target".
func TestCellPool_FillsToWarm(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), pipelinesTeam(), newPool(2))

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cells := listCells(t, store)
	want := []string{"cell-lts-2-555-0", "cell-lts-2-555-1"}
	if got := cellNames(cells); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("cells = %v, want %v", got, want)
	}
	for _, c := range cells {
		if c.Labels[LabelCellPool] != testProfileName {
			t.Errorf("%s: missing pool label, got %v", c.Name, c.Labels)
		}
		if c.Spec.Version != testResolveVersion {
			t.Errorf("%s: version = %q, want %q", c.Name, c.Spec.Version, testResolveVersion)
		}
		if c.Spec.Cell == nil || !c.Spec.Cell.Enabled || c.Spec.Cell.MaxTenants != 0 {
			t.Errorf("%s: cell spec = %+v, want enabled with maxTenants 0", c.Name, c.Spec.Cell)
		}
	}
}

// TestCellPool_DrainsToWarm covers "Lowering warm drains only free cells".
func TestCellPool_DrainsToWarm(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store,
		newProfile(), pipelinesTeam(), newPool(1),
		poolCellNamed("cell-lts-2-555-0", testProfileName, nil),
		poolCellNamed("cell-lts-2-555-1", testProfileName, nil),
		poolCellNamed("cell-lts-2-555-2", testProfileName, nil),
		poolCellNamed("cell-lts-2-555-3", testProfileName, []string{"team-a"}),
		poolCellNamed("cell-lts-2-555-4", testProfileName, []string{"team-b"}),
	)

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := cellNames(listCells(t, store))
	want := []string{"cell-lts-2-555-0", "cell-lts-2-555-3", "cell-lts-2-555-4"}
	if len(got) != 3 {
		t.Fatalf("cells = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cells = %v, want %v", got, want)
		}
	}
}

// TestCellPool_NeverDeletesAPlacedCell covers "A cell that holds a tenant is
// never deleted by the pool", whatever warm says.
func TestCellPool_NeverDeletesAPlacedCell(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store,
		newProfile(), newPool(0),
		poolCellNamed("cell-lts-2-555-0", testProfileName, []string{"team-a"}),
		poolCellNamed("cell-lts-2-555-1", testProfileName, []string{"team-b"}),
	)

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := listCells(t, store); len(got) != 2 {
		t.Fatalf("expected both placed cells to survive, got %v", cellNames(got))
	}
}

// TestCellPool_WarmZeroColdStart covers the warm: 0 cold-start cycle over
// three ticks with no churn: one cell is created for the waiting tenant, the
// next tick creates and deletes nothing, and the cell is deleted once the
// tenant leaves.
func TestCellPool_WarmZeroColdStart(t *testing.T) {
	ctx := context.Background()
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), pipelinesTeam(), newPool(0))
	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})

	// Tick 1: one unplaced tenant, no free cell — create one.
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if got := cellNames(listCells(t, store)); len(got) != 1 {
		t.Fatalf("tick 1: cells = %v, want one", got)
	}

	// Tick 2: the tenant is still unplaced and the new cell counts as free,
	// so the pass creates and deletes nothing.
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if got := cellNames(listCells(t, store)); len(got) != 1 {
		t.Fatalf("tick 2: cells = %v, want the same single cell", got)
	}

	// Tick 3: the tenant is placed, so pending falls to zero, the cell is no
	// longer free, and desiredFree falls back to warm.
	placed := poolCellNamed("cell-lts-2-555-0", testProfileName, []string{"team-a"})
	crdstore.MustSeed(store, placed)
	team := pipelinesTeam()
	team.Status.Placement = &v1alpha1.TeamPlacementStatus{
		Cell: "cell-lts-2-555-0", CellNamespace: testOperatorNamespace,
	}
	crdstore.MustSeed(store, team)
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	if got := cellNames(listCells(t, store)); len(got) != 1 {
		t.Fatalf("tick 3: cells = %v, want the placed cell alone", got)
	}

	// Tick 4: the tenant leaves its only cell, so the pass deletes it.
	free := poolCellNamed("cell-lts-2-555-0", testProfileName, nil)
	crdstore.MustSeed(store, free)
	empty := pipelinesTeam()
	empty.Spec.Pipelines = nil
	crdstore.MustSeed(store, empty)
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("tick 4: %v", err)
	}
	if got := cellNames(listCells(t, store)); len(got) != 0 {
		t.Fatalf("tick 4: cells = %v, want none", got)
	}
}

// TestCellPool_CapStopsTheFill covers "The cap stops the fill".
func TestCellPool_CapStopsTheFill(t *testing.T) {
	store := crdstore.NewFake()
	pool := newPool(5)
	pool.Spec.MaxCells = 3
	crdstore.MustSeed(store,
		newProfile(), pipelinesTeam(), pool,
		poolCellNamed("cell-lts-2-555-0", testProfileName, nil),
		poolCellNamed("cell-lts-2-555-1", testProfileName, nil),
		poolCellNamed("cell-lts-2-555-2", testProfileName, nil),
	)

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := listCells(t, store); len(got) != 3 {
		t.Fatalf("the cap must stop the fill, got %v", cellNames(got))
	}
	cond := poolReady(t, getPool(t, store, testProfileName))
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.CellPoolReasonCellCapReached {
		t.Fatalf("PoolReady = %+v, want False/CellCapReached", cond)
	}
}

// TestCellPool_DefaultPoolAdoptsAnUnlabelledCell covers "A default pool
// adopts an unlabelled cell".
func TestCellPool_DefaultPoolAdoptsAnUnlabelledCell(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store,
		newProfile(), newPool(1),
		unlabelledCellNamed("cell-hand-made", nil),
	)

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	adopted, err := crdstore.Get[v1alpha1.Controller](context.Background(), store, "cell-hand-made", testOperatorNamespace)
	if err != nil {
		t.Fatalf("get adopted cell: %v", err)
	}
	if adopted.Labels[LabelCellPool] != testProfileName {
		t.Fatalf("adoption did not stamp the pool label, got %v", adopted.Labels)
	}
	// The adopted cell satisfies warm: 1, so the pass creates no second cell.
	if got := listCells(t, store); len(got) != 1 {
		t.Fatalf("the adopted cell must count toward warm, got %v", cellNames(got))
	}
}

// TestCellPool_NonDefaultPoolAdoptsNothing covers "A non-default pool adopts
// nothing".
func TestCellPool_NonDefaultPoolAdoptsNothing(t *testing.T) {
	store := crdstore.NewFake()
	big := &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "big"},
		Spec:       v1alpha1.CellPoolSpec{ProfileRef: testProfileName, Warm: 0, MaxTenantsPerCell: 10},
	}
	crdstore.MustSeed(store, newProfile(), big, unlabelledCellNamed("cell-hand-made", nil))

	r := newReconciler(store, WarmPoolConfig{Enabled: false, MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cell, err := crdstore.Get[v1alpha1.Controller](context.Background(), store, "cell-hand-made", testOperatorNamespace)
	if err != nil {
		t.Fatalf("get cell: %v", err)
	}
	if _, ok := cell.Labels[LabelCellPool]; ok {
		t.Fatalf("a non-default pool must stamp no label, got %v", cell.Labels)
	}
}

// TestCellPool_StaleLabelStaysOutOfANewPool covers "A stale label stays out
// of a new pool of the same name": the profile test keeps a cell of another
// profile out of the new pool's accounting.
func TestCellPool_StaleLabelStaysOutOfANewPool(t *testing.T) {
	store := crdstore.NewFake()
	otherProfile := &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "lts-2-500"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.500", ResolveVersion: "2.500.1"},
	}
	stale := poolCellNamed("cell-big-0", "big", []string{"team-a"})
	stale.Labels[LabelCellProfile] = "lts-2-500"
	big := &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "big"},
		Spec:       v1alpha1.CellPoolSpec{ProfileRef: testProfileName, Warm: 1, MaxTenantsPerCell: 10},
	}
	crdstore.MustSeed(store, newProfile(), otherProfile, big, stale)

	r := newReconciler(store, WarmPoolConfig{Enabled: false, MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	pool := getPool(t, store, "big")
	if pool.Status.PlacedCells != 0 {
		t.Errorf("placedCells = %d, want 0: the stale cell runs another profile", pool.Status.PlacedCells)
	}
	if pool.Status.WarmCells != 1 {
		t.Errorf("warmCells = %d, want 1: the pass fills to warm for the matching profile", pool.Status.WarmCells)
	}
}

// TestCellPool_MissingProfileStopsThePass covers "A missing profile stops the
// pass".
func TestCellPool_MissingProfileStopsThePass(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newPool(2), poolCellNamed("cell-lts-2-555-0", testProfileName, nil))

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := listCells(t, store); len(got) != 1 {
		t.Fatalf("a missing profile must create and delete nothing, got %v", cellNames(got))
	}
	cond := poolReady(t, getPool(t, store, testProfileName))
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.CellPoolReasonProfileMissing {
		t.Fatalf("PoolReady = %+v, want False/ProfileMissing", cond)
	}
}

// TestCellPool_TemplateEditRollsFreeCellsOnly covers "A template edit rolls
// free cells only".
func TestCellPool_TemplateEditRollsFreeCellsOnly(t *testing.T) {
	store := crdstore.NewFake()
	pool := newPool(1)
	pool.Spec.CellTemplate = &v1alpha1.CellTemplate{
		PodOverrides: &v1alpha1.PodOverrides{NodeSelector: map[string]string{"pool": "cells"}},
	}
	crdstore.MustSeed(store,
		newProfile(), pool,
		poolCellNamed("cell-lts-2-555-0", testProfileName, nil),
		poolCellNamed("cell-lts-2-555-1", testProfileName, []string{"team-a"}),
	)

	r := newReconciler(store, WarmPoolConfig{Enabled: false, MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	ctx := context.Background()
	free, err := crdstore.Get[v1alpha1.Controller](ctx, store, "cell-lts-2-555-0", testOperatorNamespace)
	if err != nil {
		t.Fatalf("get free cell: %v", err)
	}
	if free.Spec.PodOverrides == nil || free.Spec.PodOverrides.NodeSelector["pool"] != "cells" {
		t.Errorf("the free cell must carry the template, got %+v", free.Spec.PodOverrides)
	}
	placed, err := crdstore.Get[v1alpha1.Controller](ctx, store, "cell-lts-2-555-1", testOperatorNamespace)
	if err != nil {
		t.Fatalf("get placed cell: %v", err)
	}
	if placed.Spec.PodOverrides != nil {
		t.Errorf("the placed cell must stay unchanged, got %+v", placed.Spec.PodOverrides)
	}
}

// ---------------------------------------------------------------------------
// task 2.15: auto-creation
// ---------------------------------------------------------------------------

// TestCellPool_FirstTenantCreatesAndSizesAPool covers "The first pipelines
// tenant creates a pool": the pass seeds the pool from the environment and
// sizes it in the same tick.
func TestCellPool_FirstTenantCreatesAndSizesAPool(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), pipelinesTeam())

	r := newReconciler(store, WarmPoolConfig{
		Enabled:            true,
		PerProfile:         2,
		MaxTenantsPerCell:  7,
		UnreadyGracePeriod: 9 * time.Minute,
	})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	pool := getPool(t, store, testProfileName)
	if pool.Spec.ProfileRef != testProfileName || pool.Spec.Warm != 2 || pool.Spec.MaxCells != 0 {
		t.Fatalf("seeded spec = %+v", pool.Spec)
	}
	if pool.Spec.MaxTenantsPerCell != 7 {
		t.Errorf("maxTenantsPerCell = %d, want 7", pool.Spec.MaxTenantsPerCell)
	}
	if pool.Spec.UnreadyGracePeriod == nil || pool.Spec.UnreadyGracePeriod.Duration != 9*time.Minute {
		t.Errorf("unreadyGracePeriod = %+v, want 9m", pool.Spec.UnreadyGracePeriod)
	}
	if pool.Spec.CellTemplate != nil {
		t.Errorf("cellTemplate must stay unset, got %+v", pool.Spec.CellTemplate)
	}
	if got := listCells(t, store); len(got) != 2 {
		t.Fatalf("the pass must size the pool it created, got %v", cellNames(got))
	}
}

// TestCellPool_SeededMaxTenantsClampedToOne asserts a chart value of zero is
// clamped up, so the operator never writes a spec the CRD refuses.
func TestCellPool_SeededMaxTenantsClampedToOne(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), pipelinesTeam())

	r := newReconciler(store, WarmPoolConfig{Enabled: true, PerProfile: 0, MaxTenantsPerCell: 0})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := getPool(t, store, testProfileName).Spec.MaxTenantsPerCell; got != 1 {
		t.Fatalf("maxTenantsPerCell = %d, want 1", got)
	}
}

// TestCellPool_OperatorEditIsNeverOverwritten covers "An operator edit is
// never overwritten".
func TestCellPool_OperatorEditIsNeverOverwritten(t *testing.T) {
	store := crdstore.NewFake()
	edited := newPool(5)
	crdstore.MustSeed(store, newProfile(), pipelinesTeam(), edited)

	r := newReconciler(store, WarmPoolConfig{Enabled: true, PerProfile: 1, MaxTenantsPerCell: 10})
	for i := 0; i < 2; i++ {
		if err := r.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	if got := getPool(t, store, testProfileName).Spec.Warm; got != 5 {
		t.Fatalf("warm = %d, want the operator's own 5", got)
	}
}

// TestCellPool_AutoCreationOffCreatesNothing covers "Auto-creation off
// creates nothing".
func TestCellPool_AutoCreationOffCreatesNothing(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), pipelinesTeam())

	r := newReconciler(store, WarmPoolConfig{Enabled: false, PerProfile: 3, MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	pools, err := crdstore.List[v1alpha1.CellPool](context.Background(), store, "", "")
	if err != nil {
		t.Fatalf("list pools: %v", err)
	}
	if len(pools) != 0 {
		t.Fatalf("expected no pool, got %d", len(pools))
	}
	if got := listCells(t, store); len(got) != 0 {
		t.Fatalf("expected no cell, got %v", cellNames(got))
	}
}

// TestCellPool_HandAppliedPoolWorksWithAutoCreationOff covers "A hand-applied
// pool works with auto-creation off": an explicit object is an explicit
// request.
func TestCellPool_HandAppliedPoolWorksWithAutoCreationOff(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), pipelinesTeam(), newPool(2))

	r := newReconciler(store, WarmPoolConfig{Enabled: false, PerProfile: 0, MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := listCells(t, store); len(got) != 2 {
		t.Fatalf("a hand-applied pool must still be sized, got %v", cellNames(got))
	}
}

// TestCellPool_NoProfileRequested covers "No profile is requested".
func TestCellPool_NoProfileRequested(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile())

	r := newReconciler(store, WarmPoolConfig{Enabled: true, PerProfile: 3, MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	pools, err := crdstore.List[v1alpha1.CellPool](context.Background(), store, "", "")
	if err != nil {
		t.Fatalf("list pools: %v", err)
	}
	if len(pools) != 0 || len(listCells(t, store)) != 0 {
		t.Fatalf("expected no pool and no cell, got %d pools", len(pools))
	}
}

// ---------------------------------------------------------------------------
// task 2.16: the status computation and the delete path
// ---------------------------------------------------------------------------

// TestCellPool_StatusCounts covers "The counts follow the cells", "The tenant
// list is sorted and deduplicated", and "An unready cell is counted".
func TestCellPool_StatusCounts(t *testing.T) {
	store := crdstore.NewFake()
	placed := poolCellNamed("cell-lts-2-555-2", testProfileName, []string{"beta", "alpha"})
	placed.Status.Conditions = []v1alpha1.ControllerCondition{
		{Type: v1alpha1.ConditionCellReady, Status: metav1.ConditionTrue},
	}
	second := poolCellNamed("cell-lts-2-555-3", testProfileName, []string{"alpha"})
	second.Status.Conditions = []v1alpha1.ControllerCondition{
		{Type: v1alpha1.ConditionCellReady, Status: metav1.ConditionFalse},
	}
	crdstore.MustSeed(store,
		newProfile(), newPool(2),
		poolCellNamed("cell-lts-2-555-0", testProfileName, nil),
		poolCellNamed("cell-lts-2-555-1", testProfileName, nil),
		placed, second,
	)

	r := newReconciler(store, WarmPoolConfig{Enabled: false, MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	pool := getPool(t, store, testProfileName)
	if pool.Status.WarmCells != 2 {
		t.Errorf("warmCells = %d, want 2", pool.Status.WarmCells)
	}
	if pool.Status.PlacedCells != 2 {
		t.Errorf("placedCells = %d, want 2", pool.Status.PlacedCells)
	}
	// Capacity 10 per placed cell: (10-2) + (10-1) = 17.
	if pool.Status.FreeTenantSlots != 17 {
		t.Errorf("freeTenantSlots = %d, want 17", pool.Status.FreeTenantSlots)
	}
	// The two free cells report no CellReady condition, and one placed cell
	// reports False.
	if pool.Status.UnreadyCells != 3 {
		t.Errorf("unreadyCells = %d, want 3", pool.Status.UnreadyCells)
	}
	want := []string{"alpha", "beta"}
	if len(pool.Status.Tenants) != 2 || pool.Status.Tenants[0] != want[0] || pool.Status.Tenants[1] != want[1] {
		t.Errorf("tenants = %v, want %v", pool.Status.Tenants, want)
	}
	cond := poolReady(t, pool)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.CellPoolReasonReady {
		t.Fatalf("PoolReady = %+v, want True/Ready", cond)
	}
}

// TestCellPool_LastTenantLeavingClearsTheList covers "The last tenant leaving
// clears the list".
func TestCellPool_LastTenantLeavingClearsTheList(t *testing.T) {
	ctx := context.Background()
	store := crdstore.NewFake()
	crdstore.MustSeed(store,
		newProfile(), newPool(1),
		poolCellNamed("cell-lts-2-555-0", testProfileName, []string{"alpha"}),
	)
	r := newReconciler(store, WarmPoolConfig{Enabled: false, MaxTenantsPerCell: 10})
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := getPool(t, store, testProfileName).Status.Tenants; len(got) != 1 {
		t.Fatalf("tenants = %v, want one entry", got)
	}

	crdstore.MustSeed(store, poolCellNamed("cell-lts-2-555-0", testProfileName, nil))
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// tenants is omitempty, so the pass sends a status that omits it and the
	// crdstore clear entry force-nulls the key. The Fake applies no clear
	// entry, so the assertion reads the status the pass actually sent.
	gvr, err := crdstore.GVRFor[v1alpha1.CellPool]()
	if err != nil {
		t.Fatalf("GVRFor[CellPool]: %v", err)
	}
	patches := store.StatusPatches(gvr)
	if len(patches) == 0 {
		t.Fatal("expected a status patch")
	}
	last, ok := patches[len(patches)-1].Status.(*v1alpha1.CellPoolStatus)
	if !ok {
		t.Fatalf("status patch = %T, want *CellPoolStatus", patches[len(patches)-1].Status)
	}
	if len(last.Tenants) != 0 {
		t.Fatalf("tenants = %v, want empty", last.Tenants)
	}
	if _, cleared := crdstore.ClearStatusFields(gvr)["tenants"]; !cleared {
		t.Error("cellpools must clear the tenants key, or the stale list survives in etcd")
	}
}

// TestCellPool_FinalizerAddedOnFirstTick asserts the drain finalizer appears
// on the first tick that sees a pool.
func TestCellPool_FinalizerAddedOnFirstTick(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), newPool(0))

	r := newReconciler(store, WarmPoolConfig{Enabled: false, MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	pool := getPool(t, store, testProfileName)
	found := false
	for _, f := range pool.Finalizers {
		if f == v1alpha1.CellPoolFinalizerDrain {
			found = true
		}
	}
	if !found {
		t.Fatalf("finalizers = %v, want the drain finalizer", pool.Finalizers)
	}
}

// TestCellPool_DeleteDrainsFreeCellsOnly covers "Deleting a pool keeps its
// tenants running" and "The finalizer holds until the drain finishes": the
// placed cell keeps every label after its pool is deleted.
func TestCellPool_DeleteDrainsFreeCellsOnly(t *testing.T) {
	store := crdstore.NewFake()
	deleting := newPool(2)
	now := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{v1alpha1.CellPoolFinalizerDrain}
	crdstore.MustSeed(store,
		newProfile(), deleting,
		poolCellNamed("cell-lts-2-555-0", testProfileName, nil),
		poolCellNamed("cell-lts-2-555-1", testProfileName, []string{"team-a"}),
	)

	r := newReconciler(store, WarmPoolConfig{Enabled: false, MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := cellNames(listCells(t, store))
	if len(got) != 1 || got[0] != "cell-lts-2-555-1" {
		t.Fatalf("cells = %v, want the placed cell alone", got)
	}
	placed, err := crdstore.Get[v1alpha1.Controller](context.Background(), store, "cell-lts-2-555-1", testOperatorNamespace)
	if err != nil {
		t.Fatalf("get placed cell: %v", err)
	}
	for _, label := range []string{LabelCellPool, LabelCellProfile, LabelWarm} {
		if _, ok := placed.Labels[label]; !ok {
			t.Errorf("the placed cell lost the label %q: %v", label, placed.Labels)
		}
	}
	pool := getPool(t, store, testProfileName)
	for _, f := range pool.Finalizers {
		if f == v1alpha1.CellPoolFinalizerDrain {
			t.Fatalf("the drain finalizer must be removed once the drain finishes, got %v", pool.Finalizers)
		}
	}
}

// ---------------------------------------------------------------------------
// The stale-snapshot drain race
// ---------------------------------------------------------------------------

// versionedCell returns a pool cell carrying an explicit uid and
// resourceVersion, so a test can model the store and an earlier snapshot
// disagreeing.
func versionedCell(name, uid, resourceVersion string, tenants []string) *v1alpha1.Controller {
	c := poolCellNamed(name, testProfileName, tenants)
	c.UID = types.UID(uid)
	c.ResourceVersion = resourceVersion
	return c
}

// placedTeam returns a Team whose status.placement fences the named cell.
func placedTeam(name, cell string) *v1alpha1.Team {
	return &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"default"},
			Pipelines: &v1alpha1.TeamPipelinesSpec{
				Profile:        testProfileName,
				AgentNamespace: "default",
			},
		},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{
				Cell: cell, CellNamespace: testOperatorNamespace,
			},
		},
	}
}

func cellExists(t *testing.T, store crdstore.Backend, name string) bool {
	t.Helper()
	_, err := crdstore.Get[v1alpha1.Controller](context.Background(), store, name, testOperatorNamespace)
	return err == nil
}

// TestCellPool_DrainSkipsACellThatGainedATenant covers the interleaving where
// the placement pass added the tenant between this pass's list and its drain.
func TestCellPool_DrainSkipsACellThatGainedATenant(t *testing.T) {
	store := crdstore.NewFake()
	// The store holds the newer state: a tenant landed and bumped the version.
	crdstore.MustSeed(store, newProfile(), newPool(0),
		versionedCell("cell-lts-2-555-1", "uid-1", "2", []string{"payments"}))
	// The pass still holds the older snapshot, which saw the cell free.
	stale := versionedCell("cell-lts-2-555-1", "uid-1", "1", nil)

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	pool := getPool(t, store, testProfileName)
	if got := r.drainSurplus(context.Background(), pool, []*v1alpha1.Controller{stale}, 1); got != 0 {
		t.Fatalf("drainSurplus deleted %d cells, want 0", got)
	}
	if !cellExists(t, store, "cell-lts-2-555-1") {
		t.Fatal("the drain deleted a cell that holds a tenant")
	}
}

// TestCellPool_DrainSkipsACellAPlacementFenceNames covers the narrower
// window: the fence is written and the cell's own tenant list has not caught
// up, so nothing about the cell object has changed yet. Only the fresh
// placement read stops this delete.
func TestCellPool_DrainSkipsACellAPlacementFenceNames(t *testing.T) {
	store := crdstore.NewFake()
	cell := versionedCell("cell-lts-2-555-1", "uid-1", "1", nil)
	crdstore.MustSeed(store, newProfile(), newPool(0), cell,
		placedTeam("payments", "cell-lts-2-555-1"))

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	pool := getPool(t, store, testProfileName)
	// The snapshot matches the store exactly: the cell is untouched.
	if got := r.drainSurplus(context.Background(), pool, []*v1alpha1.Controller{cell}, 1); got != 0 {
		t.Fatalf("drainSurplus deleted %d cells, want 0", got)
	}
	if !cellExists(t, store, "cell-lts-2-555-1") {
		t.Fatal("the drain deleted a cell a placement fence names")
	}
}

// TestCellPool_DrainSkipsACellThatChangedDuringThePass covers the delete
// precondition: a write that lands between the re-read and the delete makes
// the apiserver answer Conflict, and the drain then reports the cell not
// deleted rather than failing the pass. The fake answers the same Conflict
// the precondition produces, because the window itself cannot be opened from
// a single-threaded test.
func TestCellPool_DrainSkipsACellThatChangedDuringThePass(t *testing.T) {
	store := crdstore.NewFake()
	cell := versionedCell("cell-lts-2-555-1", "uid-1", "1", nil)
	crdstore.MustSeed(store, newProfile(), newPool(0), cell)

	ctrlGVR, err := crdstore.GVRFor[v1alpha1.Controller]()
	if err != nil {
		t.Fatalf("GVRFor[Controller]: %v", err)
	}
	store.FailNext("delete", ctrlGVR, apierrors.NewConflict(
		schema.GroupResource{Group: ctrlGVR.Group, Resource: ctrlGVR.Resource},
		"cell-lts-2-555-1", errStaleResourceVersion))

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	pool := getPool(t, store, testProfileName)
	if got := r.drainSurplus(context.Background(), pool, []*v1alpha1.Controller{cell}, 1); got != 0 {
		t.Fatalf("drainSurplus deleted %d cells, want 0", got)
	}
	if !cellExists(t, store, "cell-lts-2-555-1") {
		t.Fatal("the drain deleted a cell whose delete answered Conflict")
	}
}

// errStaleResourceVersion is the cause a precondition failure carries.
var errStaleResourceVersion = errors.New("resourceVersion precondition failed")

// TestCellPool_DeleteIfUnchangedRefusesAMovedResourceVersion proves the
// precondition itself reaches the store: a delete naming an older
// resourceVersion answers Conflict and removes nothing.
func TestCellPool_DeleteIfUnchangedRefusesAMovedResourceVersion(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, versionedCell("cell-lts-2-555-1", "uid-1", "2", nil))

	err := crdstore.DeleteIfUnchanged[v1alpha1.Controller](context.Background(), store,
		"cell-lts-2-555-1", testOperatorNamespace, "uid-1", "1")
	if !apierrors.IsConflict(err) {
		t.Fatalf("err = %v, want a Conflict", err)
	}
	if !cellExists(t, store, "cell-lts-2-555-1") {
		t.Fatal("a refused precondition must delete nothing")
	}

	// The same delete at the observed resourceVersion goes through.
	if err := crdstore.DeleteIfUnchanged[v1alpha1.Controller](context.Background(), store,
		"cell-lts-2-555-1", testOperatorNamespace, "uid-1", "2"); err != nil {
		t.Fatalf("delete at the observed resourceVersion: %v", err)
	}
	if cellExists(t, store, "cell-lts-2-555-1") {
		t.Fatal("the cell must be gone")
	}
}

// TestCellPool_DrainDeletesAGenuinelyFreeCell is the positive control: an
// unchanged, unfenced, tenant-free cell is still drained.
func TestCellPool_DrainDeletesAGenuinelyFreeCell(t *testing.T) {
	store := crdstore.NewFake()
	cell := versionedCell("cell-lts-2-555-1", "uid-1", "1", nil)
	crdstore.MustSeed(store, newProfile(), newPool(0), cell)

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	pool := getPool(t, store, testProfileName)
	if got := r.drainSurplus(context.Background(), pool, []*v1alpha1.Controller{cell}, 1); got != 1 {
		t.Fatalf("drainSurplus deleted %d cells, want 1", got)
	}
	if cellExists(t, store, "cell-lts-2-555-1") {
		t.Fatal("a genuinely free cell must be drained")
	}
}

// TestCellPool_DeletedPoolHoldsTheFinalizerWhileACellSurvives asserts the
// delete path applies the same guard, and keeps the finalizer when it could
// not drain every free cell. Releasing it early would let the pool object go
// while a cell still names it, and no later tick would revisit that cell.
func TestCellPool_DeletedPoolHoldsTheFinalizerWhileACellSurvives(t *testing.T) {
	store := crdstore.NewFake()
	deleting := newPool(0)
	now := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{v1alpha1.CellPoolFinalizerDrain}
	crdstore.MustSeed(store, newProfile(), deleting,
		versionedCell("cell-lts-2-555-0", "uid-0", "1", nil),
		versionedCell("cell-lts-2-555-1", "uid-1", "1", nil),
		placedTeam("payments", "cell-lts-2-555-1"))

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if cellExists(t, store, "cell-lts-2-555-0") {
		t.Error("the unfenced free cell must be drained")
	}
	if !cellExists(t, store, "cell-lts-2-555-1") {
		t.Fatal("the drain deleted a cell a placement fence names")
	}
	pool := getPool(t, store, testProfileName)
	held := false
	for _, f := range pool.Finalizers {
		if f == v1alpha1.CellPoolFinalizerDrain {
			held = true
		}
	}
	if !held {
		t.Errorf("the finalizer must hold while a cell survives, got %v", pool.Finalizers)
	}
}
