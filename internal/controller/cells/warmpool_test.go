package cells

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	testOperatorNamespace  = "varroa-system"
	testProfileName        = "lts-2-555"
	testResolveVersion     = "2.555.3"
	testArtifactSecretBase = "varroa-cell-artifacts"
)

// fakeSecretReader is a map-backed cells.SecretReader and cells.SecretWriter
// test fake. A name absent from the data map answers NotFound, as the live
// client does.
type fakeSecretReader struct {
	data      map[string]map[string][]byte
	errs      map[string]error
	labels    map[string]map[string]string
	writeErrs map[string]error
	// merges records each CreateOrUpdateSecretWithLabels call by name.
	merges []string
	// mints records each CreateSecretExclusive call by name.
	mints []string
	// deleteOnWrite names Secrets that a merge deletes before it answers
	// NotFound, the way a delete that lands during the write does.
	deleteOnWrite map[string]bool
	// raceSeed holds, per name, the store another writer creates just before
	// the first CreateSecretExclusive for that name. That create answers
	// AlreadyExists.
	raceSeed map[string]map[string][]byte
}

func (f *fakeSecretReader) GetSecret(_ context.Context, name, _ string) (map[string][]byte, error) {
	if err, ok := f.errs[name]; ok {
		return nil, err
	}
	data, ok := f.data[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
	}
	return data, nil
}

// CreateOrUpdateSecretWithLabels makes the fake a cells.SecretWriter too. It
// merges data and labels onto its own map, the way the live client merges
// them onto a live Secret. It answers NotFound for an absent Secret and never
// creates one.
func (f *fakeSecretReader) CreateOrUpdateSecretWithLabels(_ context.Context, name, _ string, labels map[string]string, data map[string][]byte) error {
	if err, ok := f.writeErrs[name]; ok {
		return err
	}
	if f.deleteOnWrite[name] {
		delete(f.data, name)
		delete(f.labels, name)
		return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
	}
	if _, ok := f.data[name]; !ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
	}
	if f.labels == nil {
		f.labels = map[string]map[string]string{}
	}
	if f.labels[name] == nil {
		f.labels[name] = map[string]string{}
	}
	for k, v := range data {
		f.data[name][k] = v
	}
	for k, v := range labels {
		f.labels[name][k] = v
	}
	f.merges = append(f.merges, name)
	return nil
}

// CreateSecretExclusive makes the fake create an absent Secret with the given
// labels and data. It answers AlreadyExists when the Secret exists. When
// raceSeed holds a store for the name, that store lands first, once.
func (f *fakeSecretReader) CreateSecretExclusive(_ context.Context, name, _ string, labels map[string]string, data map[string][]byte) error {
	if err, ok := f.writeErrs[name]; ok {
		return err
	}
	if f.data == nil {
		f.data = map[string]map[string][]byte{}
	}
	if f.labels == nil {
		f.labels = map[string]map[string]string{}
	}
	f.mints = append(f.mints, name)
	if seed, ok := f.raceSeed[name]; ok {
		delete(f.raceSeed, name)
		f.data[name] = seed
		return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, name)
	}
	if _, ok := f.data[name]; ok {
		return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, name)
	}
	f.data[name] = map[string][]byte{}
	for k, v := range data {
		f.data[name][k] = v
	}
	f.labels[name] = map[string]string{}
	for k, v := range labels {
		f.labels[name][k] = v
	}
	return nil
}

// seededSecretReader answers non-empty data for <base>-<pool> for every
// pool named in pools, the shape the fill gate's Secret read expects.
func seededSecretReader(base string, pools ...string) *fakeSecretReader {
	data := map[string]map[string][]byte{}
	for _, pool := range pools {
		data[base+"-"+pool] = map[string][]byte{"access-key-id": []byte("a"), "secret-access-key": []byte("b")}
	}
	return &fakeSecretReader{data: data}
}

// fakePluginPinReader is a cells.PluginPinReader test fake answering one
// fixed bool for every version.
type fakePluginPinReader struct {
	pinned bool
}

func (f *fakePluginPinReader) VersionPinsPlugin(_ context.Context, _, _ string) bool {
	return f.pinned
}

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

func pipelinesTenant() *v1alpha1.Tenant {
	return &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"default"},
			Pipelines: &v1alpha1.TenantPipelinesSpec{
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

// newReconciler builds a WarmPoolReconciler whose fill gate passes for
// every pool this file names ("lts-2-555" and "big"): ClaimGate "1",
// BlobTierS3 true, an ArtifactSecretBase, a Secrets fake seeded for both
// pools, and a Plugins fake that always answers true. A test of the gate
// itself overrides these fields on the returned reconciler.
func newReconciler(store *crdstore.Fake, cfg WarmPoolConfig) *WarmPoolReconciler {
	cfg.ClaimGate = "1"
	cfg.BlobTierS3 = true
	cfg.ArtifactSecretBase = testArtifactSecretBase
	secrets := seededSecretReader(testArtifactSecretBase, testProfileName, "big")
	return &WarmPoolReconciler{
		Store:             store,
		OperatorNamespace: testOperatorNamespace,
		Config:            cfg,
		Now:               func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
		Secrets:           secrets,
		SecretWriter:      secrets,
		Plugins:           &fakePluginPinReader{pinned: true},
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
	crdstore.MustSeed(store, newProfile(), pipelinesTenant(), newPool(2))

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
		newProfile(), pipelinesTenant(), newPool(1),
		poolCellNamed("cell-lts-2-555-0", testProfileName, nil),
		poolCellNamed("cell-lts-2-555-1", testProfileName, nil),
		poolCellNamed("cell-lts-2-555-2", testProfileName, nil),
		poolCellNamed("cell-lts-2-555-3", testProfileName, []string{"team-a"}),
		poolCellNamed("cell-lts-2-555-4", testProfileName, []string{"tenant-b"}),
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
		poolCellNamed("cell-lts-2-555-1", testProfileName, []string{"tenant-b"}),
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
	crdstore.MustSeed(store, newProfile(), pipelinesTenant(), newPool(0))
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
	tenant := pipelinesTenant()
	tenant.Status.Placement = &v1alpha1.TenantPlacementStatus{
		Cell: "cell-lts-2-555-0", CellNamespace: testOperatorNamespace,
	}
	crdstore.MustSeed(store, tenant)
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	if got := cellNames(listCells(t, store)); len(got) != 1 {
		t.Fatalf("tick 3: cells = %v, want the placed cell alone", got)
	}

	// Tick 4: the tenant leaves its only cell, so the pass deletes it.
	free := poolCellNamed("cell-lts-2-555-0", testProfileName, nil)
	crdstore.MustSeed(store, free)
	empty := pipelinesTenant()
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
		newProfile(), pipelinesTenant(), pool,
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
	crdstore.MustSeed(store, newProfile(), pipelinesTenant())

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
	crdstore.MustSeed(store, newProfile(), pipelinesTenant())

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
	crdstore.MustSeed(store, newProfile(), pipelinesTenant(), edited)

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
	crdstore.MustSeed(store, newProfile(), pipelinesTenant())

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
	crdstore.MustSeed(store, newProfile(), pipelinesTenant(), newPool(2))

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

	// tenants is omitempty, so the pass sends a status that omits it. The
	// Fake applies the crdstore clear entry, so the stored list is empty.
	if got := getPool(t, store, testProfileName).Status.Tenants; len(got) != 0 {
		t.Fatalf("stored tenants = %v, want empty", got)
	}
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
	clears := map[string]any{}
	crdstore.ApplyStatusClears(gvr, clears)
	if _, cleared := clears["tenants"]; !cleared {
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

// placedTenant returns a Tenant whose status.placement fences the named cell.
func placedTenant(name, cell string) *v1alpha1.Tenant {
	return &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"default"},
			Pipelines: &v1alpha1.TenantPipelinesSpec{
				Profile:        testProfileName,
				AgentNamespace: "default",
			},
		},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{
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
		placedTenant("payments", "cell-lts-2-555-1"))

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
		placedTenant("payments", "cell-lts-2-555-1"))

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

// ---------------------------------------------------------------------------
// Fill precondition gate (design D3/D4, tasks 2.3, 2.7, 2.8)
// ---------------------------------------------------------------------------

// gatePool returns a pool named "big" for the test profile, used only by
// the direct fillGateReason unit tests below.
func gatePool() *v1alpha1.CellPool {
	return &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "big"},
		Spec:       v1alpha1.CellPoolSpec{ProfileRef: testProfileName},
	}
}

func TestFillGateReason_RecordStoreUnsupported(t *testing.T) {
	r := newReconciler(crdstore.NewFake(), WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	r.Config.ClaimGate = "0"
	if got := r.fillGateReason(context.Background(), gatePool(), testResolveVersion, true); got != v1alpha1.CellPoolReasonRecordStoreUnsupported {
		t.Errorf("fillGateReason = %q, want %q", got, v1alpha1.CellPoolReasonRecordStoreUnsupported)
	}
}

func TestFillGateReason_BlobTierUnsupported(t *testing.T) {
	r := newReconciler(crdstore.NewFake(), WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	r.Config.BlobTierS3 = false
	if got := r.fillGateReason(context.Background(), gatePool(), testResolveVersion, true); got != v1alpha1.CellPoolReasonBlobTierUnsupported {
		t.Errorf("fillGateReason = %q, want %q", got, v1alpha1.CellPoolReasonBlobTierUnsupported)
	}
}

func TestFillGateReason_ArtifactSecretBaseEmpty(t *testing.T) {
	r := newReconciler(crdstore.NewFake(), WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	r.Config.ArtifactSecretBase = ""
	if got := r.fillGateReason(context.Background(), gatePool(), testResolveVersion, true); got != v1alpha1.CellPoolReasonArtifactSecretMissing {
		t.Errorf("fillGateReason = %q, want %q", got, v1alpha1.CellPoolReasonArtifactSecretMissing)
	}
}

func TestFillGateReason_SecretAbsent(t *testing.T) {
	r := newReconciler(crdstore.NewFake(), WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	r.Secrets = &fakeSecretReader{}
	r.SecretWriter = r.Secrets.(*fakeSecretReader)
	if got := r.fillGateReason(context.Background(), gatePool(), testResolveVersion, true); got != v1alpha1.CellPoolReasonArtifactSecretMissing {
		t.Errorf("fillGateReason = %q, want %q", got, v1alpha1.CellPoolReasonArtifactSecretMissing)
	}
}

func TestFillGateReason_SecretReadError(t *testing.T) {
	r := newReconciler(crdstore.NewFake(), WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	r.Secrets = &fakeSecretReader{errs: map[string]error{testArtifactSecretBase + "-big": errors.New("etcd unavailable")}}
	r.SecretWriter = r.Secrets.(*fakeSecretReader)
	if got := r.fillGateReason(context.Background(), gatePool(), testResolveVersion, true); got != v1alpha1.CellPoolReasonArtifactSecretMissing {
		t.Errorf("fillGateReason = %q, want %q", got, v1alpha1.CellPoolReasonArtifactSecretMissing)
	}
}

// TestFillGateReason_SecretReadLogging contrasts the two Secret-read-error
// paths the design distinguishes: a real apierrors.NotFound answer (the
// ordinary "no pool Secret yet" case, the production shape ClientsetClient
// returns) logs nothing, while any other error is unexpected and logs a
// warning. Both still answer ArtifactSecretMissing.
func TestFillGateReason_SecretReadLogging(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantLog bool
	}{
		{
			name:    "a NotFound answer logs nothing",
			err:     apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, testArtifactSecretBase+"-big"),
			wantLog: false,
		},
		{
			name:    "a non-NotFound error logs a warning",
			err:     errors.New("etcd unavailable"),
			wantLog: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			r := newReconciler(crdstore.NewFake(), WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
			r.Logger = slog.New(slog.NewTextHandler(&logBuf, nil))
			r.Secrets = &fakeSecretReader{errs: map[string]error{testArtifactSecretBase + "-big": tc.err}}
			r.SecretWriter = r.Secrets.(*fakeSecretReader)

			if got := r.fillGateReason(context.Background(), gatePool(), testResolveVersion, true); got != v1alpha1.CellPoolReasonArtifactSecretMissing {
				t.Errorf("fillGateReason = %q, want %q", got, v1alpha1.CellPoolReasonArtifactSecretMissing)
			}
			if logged := logBuf.Len() != 0; logged != tc.wantLog {
				t.Errorf("logged = %v, want %v (log content: %q)", logged, tc.wantLog, logBuf.String())
			}
		})
	}
}

func TestFillGateReason_NilSecrets(t *testing.T) {
	r := newReconciler(crdstore.NewFake(), WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	r.Secrets = nil
	if got := r.fillGateReason(context.Background(), gatePool(), testResolveVersion, true); got != v1alpha1.CellPoolReasonArtifactSecretMissing {
		t.Errorf("fillGateReason = %q, want %q", got, v1alpha1.CellPoolReasonArtifactSecretMissing)
	}
}

func TestFillGateReason_PluginNotPinned(t *testing.T) {
	r := newReconciler(crdstore.NewFake(), WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	r.Plugins = &fakePluginPinReader{pinned: false}
	if got := r.fillGateReason(context.Background(), gatePool(), testResolveVersion, true); got != v1alpha1.CellPoolReasonArtifactPluginMissing {
		t.Errorf("fillGateReason = %q, want %q", got, v1alpha1.CellPoolReasonArtifactPluginMissing)
	}
}

func TestFillGateReason_NilPlugins(t *testing.T) {
	r := newReconciler(crdstore.NewFake(), WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	r.Plugins = nil
	if got := r.fillGateReason(context.Background(), gatePool(), testResolveVersion, true); got != v1alpha1.CellPoolReasonArtifactPluginMissing {
		t.Errorf("fillGateReason = %q, want %q", got, v1alpha1.CellPoolReasonArtifactPluginMissing)
	}
}

func TestFillGateReason_EveryTestPasses(t *testing.T) {
	r := newReconciler(crdstore.NewFake(), WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	if got := r.fillGateReason(context.Background(), gatePool(), testResolveVersion, true); got != "" {
		t.Errorf("fillGateReason = %q, want empty", got)
	}
}

// TestCellPool_FillGateRefusals drives the gate through a full pass (task
// 2.7): each row refuses the fill and writes its exact reason and message.
func TestCellPool_FillGateRefusals(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(r *WarmPoolReconciler)
		wantReason string
		wantMsg    string
	}{
		{
			name:       "record store disabled",
			mutate:     func(r *WarmPoolReconciler) { r.Config.ClaimGate = "0" },
			wantReason: v1alpha1.CellPoolReasonRecordStoreUnsupported,
			wantMsg:    "the pool needs the record store enabled (recordStore.enabled)",
		},
		{
			name:       "blob tier not s3",
			mutate:     func(r *WarmPoolReconciler) { r.Config.BlobTierS3 = false },
			wantReason: v1alpha1.CellPoolReasonBlobTierUnsupported,
			wantMsg:    "the pool needs the s3 blob tier (recordStore.blobs.mode)",
		},
		{
			name:       "artifact secret base unset",
			mutate:     func(r *WarmPoolReconciler) { r.Config.ArtifactSecretBase = "" },
			wantReason: v1alpha1.CellPoolReasonArtifactSecretMissing,
			wantMsg:    "recordStore.blobs.s3.artifactSecret is not configured",
		},
		{
			name:       "pool secret absent",
			mutate:     func(r *WarmPoolReconciler) { r.Secrets = &fakeSecretReader{} },
			wantReason: v1alpha1.CellPoolReasonArtifactSecretMissing,
			wantMsg:    fmt.Sprintf("artifact secret %q not found", testArtifactSecretBase+"-"+testProfileName),
		},
		{
			name: "pool secret read error",
			mutate: func(r *WarmPoolReconciler) {
				r.Secrets = &fakeSecretReader{errs: map[string]error{
					testArtifactSecretBase + "-" + testProfileName: errors.New("etcd unavailable"),
				}}
			},
			wantReason: v1alpha1.CellPoolReasonArtifactSecretMissing,
			wantMsg:    fmt.Sprintf("artifact secret %q not found", testArtifactSecretBase+"-"+testProfileName),
		},
		{
			name:       "nil secrets reader",
			mutate:     func(r *WarmPoolReconciler) { r.Secrets = nil },
			wantReason: v1alpha1.CellPoolReasonArtifactSecretMissing,
			wantMsg:    fmt.Sprintf("artifact secret %q not found", testArtifactSecretBase+"-"+testProfileName),
		},
		{
			name: "STS issuer secret absent",
			mutate: func(r *WarmPoolReconciler) {
				r.Config.ArtifactSecretBase = ""
				r.Config.ArtifactIssuerSecret = "varroa-artifact-issuer"
			},
			wantReason: v1alpha1.CellPoolReasonArtifactSecretMissing,
			wantMsg:    `artifact issuer secret "varroa-artifact-issuer" not found`,
		},
		{
			name:       "plugin not pinned",
			mutate:     func(r *WarmPoolReconciler) { r.Plugins = &fakePluginPinReader{pinned: false} },
			wantReason: v1alpha1.CellPoolReasonArtifactPluginMissing,
			wantMsg:    fmt.Sprintf("the plugin set for version %q does not pin artifact-manager-s3", testResolveVersion),
		},
		{
			name:       "nil plugins reader",
			mutate:     func(r *WarmPoolReconciler) { r.Plugins = nil },
			wantReason: v1alpha1.CellPoolReasonArtifactPluginMissing,
			wantMsg:    fmt.Sprintf("the plugin set for version %q does not pin artifact-manager-s3", testResolveVersion),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := crdstore.NewFake()
			crdstore.MustSeed(store, newProfile(), pipelinesTenant(), newPool(1))

			r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
			tc.mutate(r)
			if err := r.Reconcile(context.Background()); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if got := listCells(t, store); len(got) != 0 {
				t.Fatalf("a refused gate must create no cell, got %v", cellNames(got))
			}
			cond := poolReady(t, getPool(t, store, testProfileName))
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != tc.wantReason {
				t.Fatalf("PoolReady = %+v, want False/%s", cond, tc.wantReason)
			}
			if cond.Message != tc.wantMsg {
				t.Errorf("message = %q, want %q", cond.Message, tc.wantMsg)
			}
		})
	}
}

// TestCellPool_STSModeFillsWithoutPoolSecret covers STS mode: the gate
// reads the issuer Secret and never the pool's own artifact Secret.
func TestCellPool_STSModeFillsWithoutPoolSecret(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), pipelinesTenant(), newPool(1))

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	r.Config.ArtifactSecretBase = ""
	r.Config.ArtifactIssuerSecret = "varroa-artifact-issuer"
	secrets := &fakeSecretReader{data: map[string]map[string][]byte{
		"varroa-artifact-issuer": {"access-key-id": []byte("a"), "secret-access-key": []byte("b")},
	}}
	r.Secrets = secrets
	r.SecretWriter = r.Secrets.(*fakeSecretReader)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := listCells(t, store); len(got) != 1 {
		t.Fatalf("cells = %v, want one cell", cellNames(got))
	}
	if cond := poolReady(t, getPool(t, store, testProfileName)); cond == nil || cond.Reason == v1alpha1.CellPoolReasonArtifactSecretMissing {
		t.Fatalf("PoolReady = %+v, want no ArtifactSecretMissing", cond)
	}
}

// TestCellPool_RefusedFillStillRestampsFreeCells covers task 2.8: a refused
// fill still restamps the free cells that already exist.
func TestCellPool_RefusedFillStillRestampsFreeCells(t *testing.T) {
	store := crdstore.NewFake()
	free := poolCellNamed("cell-lts-2-555-0", testProfileName, nil)
	free.Spec.Version = "2.400"
	crdstore.MustSeed(store, newProfile(), pipelinesTenant(), newPool(2), free)

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	r.Config.ClaimGate = "0"
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := listCells(t, store)
	if len(got) != 1 {
		t.Fatalf("the refused fill must create no cell, got %v", cellNames(got))
	}
	if got[0].Spec.Version != testResolveVersion {
		t.Errorf("free cell version = %q, want restamped to %q", got[0].Spec.Version, testResolveVersion)
	}
}

// TestCellPool_AtTargetIgnoresPreconditions covers task 2.8/the "A pool at
// its target ignores the preconditions" scenario: a pool that wants no fill
// reports Ready even while the pool Secret is absent.
func TestCellPool_AtTargetIgnoresPreconditions(t *testing.T) {
	store := crdstore.NewFake()
	free := poolCellNamed("cell-lts-2-555-0", testProfileName, nil)
	crdstore.MustSeed(store, newProfile(), newPool(1), free)

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	r.Secrets = &fakeSecretReader{}
	r.SecretWriter = r.Secrets.(*fakeSecretReader)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	cond := poolReady(t, getPool(t, store, testProfileName))
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.CellPoolReasonReady {
		t.Fatalf("PoolReady = %+v, want True/Ready", cond)
	}
}

// TestCellPool_RefusedFillOutranksCap covers task 2.8/"A refused fill
// outranks the cap": a pool at its cap that also wants a fill reports the
// gate reason, not CellCapReached.
func TestCellPool_RefusedFillOutranksCap(t *testing.T) {
	store := crdstore.NewFake()
	pool := newPool(3)
	pool.Spec.MaxCells = 2
	crdstore.MustSeed(store, newProfile(), pool,
		poolCellNamed("cell-lts-2-555-0", testProfileName, nil),
		poolCellNamed("cell-lts-2-555-1", testProfileName, nil),
	)

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	r.Secrets = &fakeSecretReader{}
	r.SecretWriter = r.Secrets.(*fakeSecretReader)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	cond := poolReady(t, getPool(t, store, testProfileName))
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.CellPoolReasonArtifactSecretMissing {
		t.Fatalf("PoolReady = %+v, want False/ArtifactSecretMissing", cond)
	}
}

// TestCellPool_SecretCreatedAfterRefusalAdmitsFill covers task 2.8/"A pool
// Secret admits the fill": the next pass creates the cell once the pool
// Secret exists with non-empty data.
func TestCellPool_SecretCreatedAfterRefusalAdmitsFill(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), pipelinesTenant(), newPool(1))

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	fake := &fakeSecretReader{}
	r.Secrets = fake
	r.SecretWriter = r.Secrets.(*fakeSecretReader)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if got := listCells(t, store); len(got) != 0 {
		t.Fatalf("tick 1: expected no cell, got %v", cellNames(got))
	}

	fake.data = map[string]map[string][]byte{
		testArtifactSecretBase + "-" + testProfileName: {"access-key-id": []byte("a"), "secret-access-key": []byte("b")},
	}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if got := listCells(t, store); len(got) != 1 {
		t.Fatalf("tick 2: expected the fill to proceed, got %v", cellNames(got))
	}
}

// TestCellPool_WithoutArtifactSecretFillsNothing covers the warm-pool delta
// scenario "A pool without its artifact Secret fills nothing": the pool is
// created for the requested profile, and the fill creates no cell.
func TestCellPool_WithoutArtifactSecretFillsNothing(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), pipelinesTenant())

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	r.Secrets = &fakeSecretReader{}
	r.SecretWriter = r.Secrets.(*fakeSecretReader)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	pool := getPool(t, store, testProfileName)
	if got := listCells(t, store); len(got) != 0 {
		t.Fatalf("expected no cell, got %v", cellNames(got))
	}
	cond := poolReady(t, pool)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.CellPoolReasonArtifactSecretMissing {
		t.Fatalf("PoolReady = %+v, want False/ArtifactSecretMissing", cond)
	}
}

// ---------------------------------------------------------------------------
// The pool confidential store (design D1, D2, D4, tasks 2.2 to 2.6)
// ---------------------------------------------------------------------------

// poolSecretFor returns the pool store the fake holds for the test pool.
func poolSecretFor(fake *fakeSecretReader) map[string][]byte {
	return fake.data[PoolSecretName(testProfileName)]
}

// TestEnsurePoolSecrets_FirstPassMints asserts that a pool with no store
// gets all four keys and the pool label.
func TestEnsurePoolSecrets_FirstPassMints(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), newPool(0))

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	fake := r.Secrets.(*fakeSecretReader)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	data := poolSecretFor(fake)
	if len(data) != len(StoreKeys()) {
		t.Fatalf("the pool store holds %d keys, want %d", len(data), len(StoreKeys()))
	}
	for _, key := range StoreKeys() {
		if len(data[key]) == 0 {
			t.Errorf("the pool store lacks %q", key)
		}
	}
	labels := fake.labels[PoolSecretName(testProfileName)]
	if labels[LabelCellPool] != testProfileName {
		t.Errorf("the pool label is %q, want %q", labels[LabelCellPool], testProfileName)
	}
	cond := poolReady(t, getPool(t, store, testProfileName))
	if cond == nil || cond.Reason != v1alpha1.CellPoolReasonReady {
		t.Fatalf("PoolReady = %+v, want Ready", cond)
	}
}

// TestEnsurePoolSecrets_EmptyDataMints asserts that an existing store with
// empty data counts as absent.
func TestEnsurePoolSecrets_EmptyDataMints(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), newPool(0))

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	fake := r.Secrets.(*fakeSecretReader)
	fake.data[PoolSecretName(testProfileName)] = map[string][]byte{}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, key := range StoreKeys() {
		if len(poolSecretFor(fake)[key]) == 0 {
			t.Errorf("the pool store lacks %q", key)
		}
	}
}

// TestEnsurePoolSecrets_SecondPassChangesNothing asserts the mint-once rule.
func TestEnsurePoolSecrets_SecondPassChangesNothing(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), newPool(0))

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	fake := r.Secrets.(*fakeSecretReader)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	first := map[string][]byte{}
	for key, value := range poolSecretFor(fake) {
		first[key] = append([]byte(nil), value...)
	}
	writes := len(fake.merges)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if len(fake.merges) != writes {
		t.Error("the second pass wrote the store again")
	}
	for key, want := range first {
		if !bytes.Equal(poolSecretFor(fake)[key], want) {
			t.Errorf("the second pass changed %q", key)
		}
	}
}

// TestEnsurePoolSecrets_RepairsAMissingKey asserts that a repair adds only
// the absent key.
func TestEnsurePoolSecrets_RepairsAMissingKey(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), newPool(0))

	minted, err := MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	partial := map[string][]byte{}
	for key, value := range minted {
		if key == KeyInstanceIdentity {
			continue
		}
		partial[key] = value
	}
	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	fake := r.Secrets.(*fakeSecretReader)
	fake.data[PoolSecretName(testProfileName)] = partial
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	data := poolSecretFor(fake)
	if len(data[KeyInstanceIdentity]) == 0 {
		t.Fatal("the repair did not add the instance identity key")
	}
	for _, key := range []string{KeyMasterKey, KeyJnlpSecret, KeyHudsonUtilSecret} {
		if !bytes.Equal(data[key], minted[key]) {
			t.Errorf("the repair changed %q", key)
		}
	}
	cond := poolReady(t, getPool(t, store, testProfileName))
	if cond == nil || cond.Reason != v1alpha1.CellPoolReasonReady {
		t.Fatalf("PoolReady = %+v, want Ready", cond)
	}
}

// TestEnsurePoolSecrets_NoMasterKeyIsAFault asserts that a store with
// another key and no master.key gets no write and reports SecretsMissing.
func TestEnsurePoolSecrets_NoMasterKeyIsAFault(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), newPool(0))

	minted, err := MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	fake := r.Secrets.(*fakeSecretReader)
	fake.data[PoolSecretName(testProfileName)] = map[string][]byte{KeyJnlpSecret: minted[KeyJnlpSecret]}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(fake.merges) != 0 {
		t.Errorf("the pass wrote the store %v", fake.merges)
	}
	cond := poolReady(t, getPool(t, store, testProfileName))
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.CellPoolReasonSecretsMissing {
		t.Fatalf("PoolReady = %+v, want False/SecretsMissing", cond)
	}
	want := fmt.Sprintf("the pool secret %q is not usable", PoolSecretName(testProfileName))
	if cond.Message != want {
		t.Errorf("message = %q, want %q", cond.Message, want)
	}
}

// TestEnsurePoolSecrets_RefusesTheFill asserts that an unusable store stops
// a wanted fill.
func TestEnsurePoolSecrets_RefusesTheFill(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), pipelinesTenant(), newPool(1))

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	fake := r.Secrets.(*fakeSecretReader)
	fake.writeErrs = map[string]error{PoolSecretName(testProfileName): errors.New("etcd unavailable")}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if got := listCells(t, store); len(got) != 0 {
		t.Fatalf("expected no cell, got %v", cellNames(got))
	}
	cond := poolReady(t, getPool(t, store, testProfileName))
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.CellPoolReasonSecretsMissing {
		t.Fatalf("PoolReady = %+v, want False/SecretsMissing", cond)
	}
}

// TestEnsurePoolSecrets_AtTargetStillReports asserts that a pass with no
// fill still reports an unusable store.
func TestEnsurePoolSecrets_AtTargetStillReports(t *testing.T) {
	store := crdstore.NewFake()
	free := poolCellNamed("cell-lts-2-555-0", testProfileName, nil)
	crdstore.MustSeed(store, newProfile(), newPool(1), free)

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	fake := r.Secrets.(*fakeSecretReader)
	fake.errs = map[string]error{PoolSecretName(testProfileName): errors.New("etcd unavailable")}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cond := poolReady(t, getPool(t, store, testProfileName))
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.CellPoolReasonSecretsMissing {
		t.Fatalf("PoolReady = %+v, want False/SecretsMissing", cond)
	}
}

// TestEnsurePoolSecrets_OutranksTheCap asserts that an unusable store
// outranks CellCapReached on a pass that wants no fill.
func TestEnsurePoolSecrets_OutranksTheCap(t *testing.T) {
	store := crdstore.NewFake()
	pool := newPool(2)
	pool.Spec.MaxCells = 2
	crdstore.MustSeed(store, newProfile(), pool,
		poolCellNamed("cell-lts-2-555-0", testProfileName, nil),
		poolCellNamed("cell-lts-2-555-1", testProfileName, nil),
	)

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	fake := r.Secrets.(*fakeSecretReader)
	fake.errs = map[string]error{PoolSecretName(testProfileName): errors.New("etcd unavailable")}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cond := poolReady(t, getPool(t, store, testProfileName))
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.CellPoolReasonSecretsMissing {
		t.Fatalf("PoolReady = %+v, want False/SecretsMissing", cond)
	}
}

// TestEnsurePoolSecrets_UndecryptableKeyIsAFault asserts that a store whose
// key does not decrypt under its own master.key gets no write and reports
// SecretsMissing. The recovery is the same as for a store with no
// master.key: delete the Secret.
func TestEnsurePoolSecrets_UndecryptableKeyIsAFault(t *testing.T) {
	cases := []struct {
		name    string
		corrupt func(store map[string][]byte)
	}{
		{
			name: "a flipped byte in the last block",
			corrupt: func(store map[string][]byte) {
				value := store[KeyJnlpSecret]
				value[len(value)-1] ^= 0xff
			},
		},
		{
			name: "a key of another store",
			corrupt: func(store map[string][]byte) {
				other, err := MintStore()
				if err != nil {
					t.Fatalf("MintStore: %v", err)
				}
				store[KeyHudsonUtilSecret] = other[KeyHudsonUtilSecret]
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := crdstore.NewFake()
			crdstore.MustSeed(store, newProfile(), newPool(0))

			minted, err := MintStore()
			if err != nil {
				t.Fatalf("MintStore: %v", err)
			}
			tc.corrupt(minted)

			r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
			fake := r.Secrets.(*fakeSecretReader)
			fake.data[PoolSecretName(testProfileName)] = minted
			if err := r.Reconcile(context.Background()); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			if len(fake.merges) != 0 {
				t.Errorf("the pass wrote the store %v", fake.merges)
			}
			cond := poolReady(t, getPool(t, store, testProfileName))
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.CellPoolReasonSecretsMissing {
				t.Fatalf("PoolReady = %+v, want False/SecretsMissing", cond)
			}
		})
	}
}

// TestEnsurePoolSecrets_RepairVerifiesTheKeptKeys asserts that a repair of a
// store whose kept key does not decrypt writes nothing, rather than adding
// the missing key onto an unusable store.
func TestEnsurePoolSecrets_RepairVerifiesTheKeptKeys(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), newPool(0))

	minted, err := MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	partial := map[string][]byte{
		KeyMasterKey:        minted[KeyMasterKey],
		KeyJnlpSecret:       minted[KeyJnlpSecret],
		KeyHudsonUtilSecret: minted[KeyHudsonUtilSecret],
	}
	partial[KeyJnlpSecret][len(partial[KeyJnlpSecret])-1] ^= 0xff

	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	fake := r.Secrets.(*fakeSecretReader)
	fake.data[PoolSecretName(testProfileName)] = partial
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(fake.merges) != 0 {
		t.Errorf("the pass wrote the store %v", fake.merges)
	}
	cond := poolReady(t, getPool(t, store, testProfileName))
	if cond == nil || cond.Reason != v1alpha1.CellPoolReasonSecretsMissing {
		t.Fatalf("PoolReady = %+v, want False/SecretsMissing", cond)
	}
}

// TestEnsurePoolSecrets_DeleteDuringRepairMintsNextPass asserts that a delete
// during a repair write creates nothing. The pass reports SecretsMissing, and
// the next pass mints a full store.
func TestEnsurePoolSecrets_DeleteDuringRepairMintsNextPass(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), newPool(0))

	minted, err := MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	partial := map[string][]byte{}
	for key, value := range minted {
		if key == KeyInstanceIdentity {
			continue
		}
		partial[key] = value
	}
	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	fake := r.Secrets.(*fakeSecretReader)
	name := PoolSecretName(testProfileName)
	fake.data[name] = partial
	fake.deleteOnWrite = map[string]bool{name: true}

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	cond := poolReady(t, getPool(t, store, testProfileName))
	if cond == nil || cond.Reason != v1alpha1.CellPoolReasonSecretsMissing {
		t.Fatalf("tick 1 PoolReady = %+v, want False/SecretsMissing", cond)
	}
	if poolSecretFor(fake) != nil {
		t.Fatalf("tick 1 left a store behind: %v", poolSecretFor(fake))
	}
	if len(fake.mints) != 0 {
		t.Fatalf("tick 1 minted %v, want no mint", fake.mints)
	}

	fake.deleteOnWrite = nil
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	data := poolSecretFor(fake)
	for _, key := range StoreKeys() {
		if len(data[key]) == 0 {
			t.Errorf("tick 2 store lacks %q", key)
		}
	}
	if err := VerifyStore(data); err != nil {
		t.Fatalf("tick 2 store does not verify: %v", err)
	}
	if fake.labels[name][LabelCellPool] != testProfileName {
		t.Errorf("the pool label is %q, want %q", fake.labels[name][LabelCellPool], testProfileName)
	}
	cond = poolReady(t, getPool(t, store, testProfileName))
	if cond == nil || cond.Reason != v1alpha1.CellPoolReasonReady {
		t.Fatalf("tick 2 PoolReady = %+v, want Ready", cond)
	}
	if len(fake.mints) != 1 {
		t.Fatalf("mints = %v, want one mint", fake.mints)
	}
	if bytes.Equal(data[KeyMasterKey], minted[KeyMasterKey]) {
		t.Error("tick 2 reused the master.key of the partial store")
	}
}

// TestEnsurePoolSecrets_MintLosesCreateRaceRepairsNextPass asserts that a mint
// which loses the create race fails its pass and leaves the other writer's
// store unchanged. The next pass repairs that store and keeps its master.key.
func TestEnsurePoolSecrets_MintLosesCreateRaceRepairsNextPass(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), newPool(0))

	seeded, err := MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	r := newReconciler(store, WarmPoolConfig{Enabled: true, MaxTenantsPerCell: 10})
	fake := r.Secrets.(*fakeSecretReader)
	name := PoolSecretName(testProfileName)
	fake.raceSeed = map[string]map[string][]byte{name: seeded}

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	cond := poolReady(t, getPool(t, store, testProfileName))
	if cond == nil || cond.Reason != v1alpha1.CellPoolReasonSecretsMissing {
		t.Fatalf("tick 1 PoolReady = %+v, want False/SecretsMissing", cond)
	}
	if !bytes.Equal(fake.data[name][KeyMasterKey], seeded[KeyMasterKey]) {
		t.Fatal("tick 1 replaced the master.key of the other writer's store")
	}

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if !bytes.Equal(poolSecretFor(fake)[KeyMasterKey], seeded[KeyMasterKey]) {
		t.Fatal("tick 2 replaced the master.key of the other writer's store")
	}
	if err := VerifyStore(poolSecretFor(fake)); err != nil {
		t.Fatalf("tick 2 store does not verify: %v", err)
	}
	cond = poolReady(t, getPool(t, store, testProfileName))
	if cond == nil || cond.Reason != v1alpha1.CellPoolReasonReady {
		t.Fatalf("tick 2 PoolReady = %+v, want Ready", cond)
	}
}

// ---------------------------------------------------------------------------
// Departed-build hold (issue 2069)
// ---------------------------------------------------------------------------

// departedBuildCell returns a free cell of the test profile whose folder for
// each path in pending waits in the deferred deletion list, and whose
// status.cell.appliedTenants names the applied tenants.
func departedBuildCell(name string, applied, pending []string) *v1alpha1.Controller {
	c := poolCellNamed(name, testProfileName, nil)
	c.Status.Cell = &v1alpha1.CellStatus{AppliedTenants: applied, Instance: "pod-" + name}
	for _, p := range pending {
		c.Status.PendingItemDeletions = append(c.Status.PendingItemDeletions,
			v1alpha1.PendingDeletion{Path: p, Reason: "build in progress"})
	}
	return c
}

// TestCellPool_DrainKeepsACellWithADeferredDeletion covers a free cell whose
// build still runs: the scale-down keeps it, and deletes it once it is clean.
func TestCellPool_DrainKeepsACellWithADeferredDeletion(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), newPool(0),
		departedBuildCell("cell-lts-2-555-0", nil, []string{"team-a"}))

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !cellExists(t, store, "cell-lts-2-555-0") {
		t.Fatal("the drain deleted a cell whose build still runs")
	}
	if got := getPool(t, store, testProfileName).Status.DepartedBuildCells; got != 1 {
		t.Fatalf("DepartedBuildCells = %d, want 1 while the build runs", got)
	}

	crdstore.MustSeed(store, departedBuildCell("cell-lts-2-555-0", nil, nil))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if cellExists(t, store, "cell-lts-2-555-0") {
		t.Fatal("a clean free cell must be drained once its build ends")
	}
	if got := getPool(t, store, testProfileName).Status.DepartedBuildCells; got != 0 {
		t.Fatalf("DepartedBuildCells = %d, want 0 once the cell is gone", got)
	}
}

// TestCellPool_DrainKeepsACellThatStillAppliesADepartedTenant covers the
// window before the first items walk clears the deferred deletion list.
func TestCellPool_DrainKeepsACellThatStillAppliesADepartedTenant(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), newPool(0),
		departedBuildCell("cell-lts-2-555-0", []string{"team-a"}, nil))

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !cellExists(t, store, "cell-lts-2-555-0") {
		t.Fatal("the drain deleted a cell that still applies a departed tenant")
	}
}

// TestCellPool_DrainTakesAnotherFreeCellFirst covers a scale-down that finds
// one held cell and one clean cell: the clean cell goes, the held one stays.
func TestCellPool_DrainTakesAnotherFreeCellFirst(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), newPool(1),
		departedBuildCell("cell-lts-2-555-0", nil, nil),
		departedBuildCell("cell-lts-2-555-1", nil, []string{"team-a"}))

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := cellNames(listCells(t, store))
	if len(got) != 1 || got[0] != "cell-lts-2-555-1" {
		t.Fatalf("cells = %v, want [cell-lts-2-555-1]", got)
	}
	status := getPool(t, store, testProfileName).Status
	if status.WarmCells != 1 || status.DepartedBuildCells != 1 {
		t.Fatalf("WarmCells = %d, DepartedBuildCells = %d, want 1 and 1",
			status.WarmCells, status.DepartedBuildCells)
	}
}

// TestCellPool_StatusCountsDepartedBuildCells checks the count against the
// drain rule: a clean cell, a placed cell, and a cell with no instance or
// an evacuation do not count, and each held free cell does.
func TestCellPool_StatusCountsDepartedBuildCells(t *testing.T) {
	store := crdstore.NewFake()
	placed := poolCellNamed("cell-lts-2-555-5", testProfileName, []string{"alpha"})
	placed.Status.Cell = &v1alpha1.CellStatus{
		Tenants:        []string{"alpha"},
		AppliedTenants: []string{"alpha", "gone"},
		Instance:       "pod-placed",
	}
	placed.Status.PendingItemDeletions = []v1alpha1.PendingDeletion{{Path: "gone", Reason: "build in progress"}}
	noInstance := departedBuildCell("cell-lts-2-555-3", []string{"tenant-c"}, nil)
	noInstance.Status.Cell.Instance = ""
	evacuating := departedBuildCell("cell-lts-2-555-4", nil, []string{"tenant-d"})
	evacuating.Status.Cell.Evacuate = true
	crdstore.MustSeed(store, newProfile(), newPool(5),
		poolCellNamed("cell-lts-2-555-0", testProfileName, nil),
		departedBuildCell("cell-lts-2-555-1", nil, []string{"team-a"}),
		departedBuildCell("cell-lts-2-555-2", []string{"tenant-b"}, nil),
		noInstance, evacuating, placed)

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	status := getPool(t, store, testProfileName).Status
	if status.WarmCells != 5 || status.PlacedCells != 1 || status.DepartedBuildCells != 2 {
		t.Fatalf("WarmCells = %d, PlacedCells = %d, DepartedBuildCells = %d, want 5, 1, 2",
			status.WarmCells, status.PlacedCells, status.DepartedBuildCells)
	}
	if got := len(listCells(t, store)); got != 6 {
		t.Fatalf("cells = %d, want the six seeded cells and no create or delete", got)
	}
}

// TestCellPool_DepartedBuildCountFallsToZero checks that a count which drops
// to zero reaches the stored status, not only a rise.
func TestCellPool_DepartedBuildCountFallsToZero(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), newPool(1),
		departedBuildCell("cell-lts-2-555-0", nil, []string{"team-a"}))

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	status := getPool(t, store, testProfileName).Status
	if status.DepartedBuildCells != 1 || status.WarmCells != 1 {
		t.Fatalf("DepartedBuildCells = %d, WarmCells = %d, want 1 and 1",
			status.DepartedBuildCells, status.WarmCells)
	}

	crdstore.MustSeed(store, departedBuildCell("cell-lts-2-555-0", nil, nil))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := getPool(t, store, testProfileName).Status.DepartedBuildCells; got != 0 {
		t.Fatalf("DepartedBuildCells = %d, want 0 after the hold ends", got)
	}
}

// TestCellPool_HeldCellCountsTowardTheWarmTarget covers the placement race:
// a held cell is a warm cell, so the pool creates no cell beside it.
func TestCellPool_HeldCellCountsTowardTheWarmTarget(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), newPool(1),
		departedBuildCell("cell-lts-2-555-0", nil, []string{"team-a"}))

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := cellNames(listCells(t, store)); len(got) != 1 {
		t.Fatalf("cells = %v, want only the held cell", got)
	}
}

// TestCellPool_CapCountsACellWithADeferredDeletion covers maxCells: the held
// cell counts toward the cap, so the fill stops at two cells.
func TestCellPool_CapCountsACellWithADeferredDeletion(t *testing.T) {
	store := crdstore.NewFake()
	pool := newPool(3)
	pool.Spec.MaxCells = 2
	crdstore.MustSeed(store, newProfile(), pool,
		departedBuildCell("cell-lts-2-555-0", nil, nil),
		departedBuildCell("cell-lts-2-555-1", nil, []string{"team-a"}))

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := listCells(t, store); len(got) != 2 {
		t.Fatalf("cells = %v, want two", cellNames(got))
	}
	cond := poolReady(t, getPool(t, store, testProfileName))
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.CellPoolReasonCellCapReached {
		t.Fatalf("PoolReady = %+v, want False/CellCapReached", cond)
	}
}

// TestCellPool_RestampSkipsACellWithADeferredDeletion covers a template edit:
// the roll would end the held build, so only the clean cell takes the template.
func TestCellPool_RestampSkipsACellWithADeferredDeletion(t *testing.T) {
	store := crdstore.NewFake()
	pool := newPool(2)
	pool.Spec.CellTemplate = &v1alpha1.CellTemplate{
		PodOverrides: &v1alpha1.PodOverrides{NodeSelector: map[string]string{"pool": "cells"}},
	}
	crdstore.MustSeed(store, newProfile(), pool,
		departedBuildCell("cell-lts-2-555-0", nil, nil),
		departedBuildCell("cell-lts-2-555-1", nil, []string{"team-a"}))

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	clean := getCell(t, store, "cell-lts-2-555-0")
	if clean.Spec.PodOverrides == nil || clean.Spec.PodOverrides.NodeSelector["pool"] != "cells" {
		t.Errorf("the clean free cell must carry the template, got %+v", clean.Spec.PodOverrides)
	}
	held := getCell(t, store, "cell-lts-2-555-1")
	if held.Spec.PodOverrides != nil {
		t.Errorf("the held cell must stay unchanged, got %+v", held.Spec.PodOverrides)
	}

	crdstore.MustSeed(store, departedBuildCell("cell-lts-2-555-1", nil, nil))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := getCell(t, store, "cell-lts-2-555-1"); got.Spec.PodOverrides == nil {
		t.Error("the cell must take the template once its build ends")
	}
}

// TestCellPool_DrainSkipsACellThatDeferredADeletionAfterTheSnapshot covers a
// cell that gained a deferred deletion after the pass listed it as free.
func TestCellPool_DrainSkipsACellThatDeferredADeletionAfterTheSnapshot(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), newPool(0),
		departedBuildCell("cell-lts-2-555-1", nil, []string{"team-a"}))
	stale := versionedCell("cell-lts-2-555-1", "uid-1", "1", nil)

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	pool := getPool(t, store, testProfileName)
	if got := r.drainSurplus(context.Background(), pool, []*v1alpha1.Controller{stale}, 1); got != 0 {
		t.Fatalf("drainSurplus deleted %d cells, want 0", got)
	}
	if !cellExists(t, store, "cell-lts-2-555-1") {
		t.Fatal("the drain deleted a cell whose build still runs")
	}
}

// TestCellPool_DeletedPoolHoldsTheFinalizerWhileACellRunsADepartedBuild
// covers a pool delete: the held cell keeps the finalizer until its build ends.
func TestCellPool_DeletedPoolHoldsTheFinalizerWhileACellRunsADepartedBuild(t *testing.T) {
	store := crdstore.NewFake()
	deleting := newPool(0)
	now := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{v1alpha1.CellPoolFinalizerDrain}
	crdstore.MustSeed(store, newProfile(), deleting,
		departedBuildCell("cell-lts-2-555-0", nil, nil),
		departedBuildCell("cell-lts-2-555-1", nil, []string{"team-a"}))

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if cellExists(t, store, "cell-lts-2-555-0") {
		t.Error("the clean free cell must be drained")
	}
	if !cellExists(t, store, "cell-lts-2-555-1") {
		t.Fatal("the drain deleted a cell whose build still runs")
	}
	if !hasDrainFinalizer(getPool(t, store, testProfileName)) {
		t.Fatal("the finalizer must hold while a held cell survives")
	}

	crdstore.MustSeed(store, departedBuildCell("cell-lts-2-555-1", nil, nil))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if cellExists(t, store, "cell-lts-2-555-1") {
		t.Error("the cell must be drained once its build ends")
	}
	if hasDrainFinalizer(getPool(t, store, testProfileName)) {
		t.Fatal("the finalizer must be removed once the drain completes")
	}
}

// TestCellPool_DeletedPoolReportsAHeldCell covers the status of a pool delete
// while a cell holds the finalizer. Every pass writes the counts, and the
// stored PoolReady condition keeps its value.
func TestCellPool_DeletedPoolReportsAHeldCell(t *testing.T) {
	store := crdstore.NewFake()
	deleting := newPool(0)
	deleting.Generation = 3
	now := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{v1alpha1.CellPoolFinalizerDrain}
	storedAt := metav1.NewTime(time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC))
	deleting.Status = v1alpha1.CellPoolStatus{
		ObservedGeneration: 2,
		WarmCells:          2,
		PlacedCells:        1,
		Conditions: []metav1.Condition{{
			Type:               v1alpha1.CellPoolConditionPoolReady,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.CellPoolReasonReady,
			ObservedGeneration: 2,
			LastTransitionTime: storedAt,
		}},
	}
	crdstore.MustSeed(store, newProfile(), deleting,
		departedBuildCell("cell-lts-2-555-0", nil, nil),
		departedBuildCell("cell-lts-2-555-1", nil, []string{"team-b"}),
		poolCellNamed("cell-lts-2-555-2", testProfileName, []string{"team-b"}))

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	ctx := context.Background()

	// Tick 1: the clean cell is drained and the held cell survives.
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile tick 1: %v", err)
	}
	if cellExists(t, store, "cell-lts-2-555-0") {
		t.Fatal("the clean free cell must be drained")
	}
	pool := getPool(t, store, testProfileName)
	if !hasDrainFinalizer(pool) {
		t.Fatal("the finalizer must hold while a held cell survives")
	}
	st := pool.Status
	if st.WarmCells != 1 || st.DepartedBuildCells != 1 || st.PlacedCells != 1 {
		t.Errorf("counts = warm %d departed %d placed %d, want 1 1 1",
			st.WarmCells, st.DepartedBuildCells, st.PlacedCells)
	}
	if st.FreeTenantSlots != 9 || st.UnreadyCells != 2 {
		t.Errorf("FreeTenantSlots %d UnreadyCells %d, want 9 and 2", st.FreeTenantSlots, st.UnreadyCells)
	}
	if len(st.Tenants) != 1 || st.Tenants[0] != "team-b" {
		t.Errorf("Tenants = %v, want [team-b]", st.Tenants)
	}
	if st.ObservedGeneration != 3 {
		t.Errorf("ObservedGeneration = %d, want 3", st.ObservedGeneration)
	}
	cond := findPoolReady(t, pool)
	if cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.CellPoolReasonReady {
		t.Errorf("PoolReady = %+v, want True/Ready", cond)
	}
	if cond.ObservedGeneration != 2 || !cond.LastTransitionTime.Time.Equal(storedAt.Time) {
		t.Errorf("PoolReady was rewritten during the delete: %+v", cond)
	}

	// Tick 2: the hold continues and the counts stay the same.
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile tick 2: %v", err)
	}
	pool = getPool(t, store, testProfileName)
	if !hasDrainFinalizer(pool) {
		t.Fatal("the finalizer must still hold on tick 2")
	}
	if pool.Status.WarmCells != 1 || pool.Status.DepartedBuildCells != 1 {
		t.Errorf("tick 2 counts = warm %d departed %d, want 1 1",
			pool.Status.WarmCells, pool.Status.DepartedBuildCells)
	}

	// Tick 3: the build ends, the cell drains, and the final pass writes the
	// status before the finalizer goes.
	crdstore.MustSeed(store, departedBuildCell("cell-lts-2-555-1", nil, nil))
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile tick 3: %v", err)
	}
	if cellExists(t, store, "cell-lts-2-555-1") {
		t.Fatal("the cell must be drained once its build ends")
	}
	pool = getPool(t, store, testProfileName)
	if hasDrainFinalizer(pool) {
		t.Fatal("the finalizer must be removed once the drain completes")
	}
	if pool.Status.WarmCells != 0 || pool.Status.DepartedBuildCells != 0 || pool.Status.PlacedCells != 1 {
		t.Errorf("final counts = warm %d departed %d placed %d, want 0 0 1",
			pool.Status.WarmCells, pool.Status.DepartedBuildCells, pool.Status.PlacedCells)
	}
}

// TestCellPool_DeletedPoolStatusWriteFailureStillReleasesTheFinalizer covers
// a refused status write: the drain still completes and removes the finalizer.
func TestCellPool_DeletedPoolStatusWriteFailureStillReleasesTheFinalizer(t *testing.T) {
	store := crdstore.NewFake()
	deleting := newPool(0)
	now := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{v1alpha1.CellPoolFinalizerDrain}
	crdstore.MustSeed(store, newProfile(), deleting,
		poolCellNamed("cell-lts-2-555-0", testProfileName, nil))

	gvr, err := crdstore.GVRFor[v1alpha1.CellPool]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	store.FailAlways("patchstatus", gvr, errors.New("status write refused"))

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if cellExists(t, store, "cell-lts-2-555-0") {
		t.Error("the clean free cell must be drained")
	}
	if hasDrainFinalizer(getPool(t, store, testProfileName)) {
		t.Fatal("a refused status write must not hold the finalizer")
	}
}

// TestCellPool_DeletedPoolStatusIsWrittenBeforeTheFinalizerRelease covers a
// refused finalizer removal: the status the same pass wrote still reads the
// counts after the drain. The fake cannot observe write order.
func TestCellPool_DeletedPoolStatusIsWrittenBeforeTheFinalizerRelease(t *testing.T) {
	store := crdstore.NewFake()
	deleting := newPool(0)
	now := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{v1alpha1.CellPoolFinalizerDrain}
	crdstore.MustSeed(store, newProfile(), deleting,
		departedBuildCell("cell-lts-2-555-0", nil, nil),
		poolCellNamed("cell-lts-2-555-1", testProfileName, []string{"team-a"}))

	gvr, err := crdstore.GVRFor[v1alpha1.CellPool]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	store.FailAlways("patchmeta", gvr, errors.New("finalizer write refused"))

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	pool := getPool(t, store, testProfileName)
	if pool.Status.WarmCells != 0 || pool.Status.PlacedCells != 1 {
		t.Errorf("status = warm %d placed %d, want 0 1 after the drain",
			pool.Status.WarmCells, pool.Status.PlacedCells)
	}
}

// findPoolReady returns the PoolReady condition of pool, or fails the test.
func findPoolReady(t *testing.T, pool *v1alpha1.CellPool) metav1.Condition {
	t.Helper()
	for _, c := range pool.Status.Conditions {
		if c.Type == v1alpha1.CellPoolConditionPoolReady {
			return c
		}
	}
	t.Fatal("PoolReady condition missing")
	return metav1.Condition{}
}

// hasDrainFinalizer reports whether the pool still holds the drain finalizer.
func hasDrainFinalizer(pool *v1alpha1.CellPool) bool {
	for _, f := range pool.Finalizers {
		if f == v1alpha1.CellPoolFinalizerDrain {
			return true
		}
	}
	return false
}

// TestCellPool_DrainTakesAnEvacuatingCell covers a cell the roll gate gave up
// on: its builds are already lost, so the pool drains it.
func TestCellPool_DrainTakesAnEvacuatingCell(t *testing.T) {
	store := crdstore.NewFake()
	cell := departedBuildCell("cell-lts-2-555-0", []string{"team-a"}, []string{"team-a"})
	cell.Status.Cell.Evacuate = true
	crdstore.MustSeed(store, newProfile(), newPool(0), cell)

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if cellExists(t, store, "cell-lts-2-555-0") {
		t.Fatal("an evacuating cell must be drained")
	}
}

// TestCellHoldsDepartedBuild covers the hold predicate's table.
func TestCellHoldsDepartedBuild(t *testing.T) {
	evacuate := func(c *v1alpha1.Controller) *v1alpha1.Controller {
		if c.Status.Cell == nil {
			c.Status.Cell = &v1alpha1.CellStatus{}
		}
		c.Status.Cell.Evacuate = true
		return c
	}
	tests := []struct {
		name string
		cell *v1alpha1.Controller
		want bool
	}{
		{"nil status", poolCellNamed("c", testProfileName, nil), false},
		{"pending only", departedBuildCell("c", nil, []string{"team-a"}), true},
		{"applied only", departedBuildCell("c", []string{"team-a"}, nil), true},
		{"applied without a live pod", func() *v1alpha1.Controller {
			c := departedBuildCell("c", []string{"team-a"}, nil)
			c.Status.Cell.Instance = ""
			return c
		}(), false},
		{"pending without a live pod", func() *v1alpha1.Controller {
			c := departedBuildCell("c", nil, []string{"team-a"})
			c.Status.Cell.Instance = ""
			return c
		}(), false},
		{"empty cell status", &v1alpha1.Controller{Status: v1alpha1.ControllerStatus{Cell: &v1alpha1.CellStatus{}}}, false},
		{"evacuate plus applied", evacuate(departedBuildCell("c", []string{"team-a"}, nil)), false},
		{"evacuate plus pending", evacuate(departedBuildCell("c", nil, []string{"team-a"})), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cellHoldsDepartedBuild(tt.cell); got != tt.want {
				t.Fatalf("cellHoldsDepartedBuild = %v, want %v", got, tt.want)
			}
		})
	}
}
