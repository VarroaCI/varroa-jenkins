package cells

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func memoryRequest(q string) *corev1.ResourceRequirements {
	return &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(q)},
	}
}

func seedTemplate() *v1alpha1.CellTemplate {
	return &v1alpha1.CellTemplate{
		Resources:     memoryRequest("3Gi"),
		MiteResources: memoryRequest("32Mi"),
		PodOverrides:  &v1alpha1.PodOverrides{JvmOpts: "-XX:MaxRAMPercentage=50"},
	}
}

func getCell(t *testing.T, store crdstore.Backend, name string) *v1alpha1.Controller {
	t.Helper()
	c, err := crdstore.Get[v1alpha1.Controller](context.Background(), store, name, testOperatorNamespace)
	if err != nil {
		t.Fatalf("get cell %s: %v", name, err)
	}
	return c
}

// An auto-created pool carries the configured template, and the cells the
// same pass creates carry its three fields.
func TestCellPool_AutoCreatedPoolCarriesTheTemplate(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), pipelinesTenant())

	cfg := WarmPoolConfig{Enabled: true, PerProfile: 1, MaxTenantsPerCell: 10, CellTemplate: seedTemplate()}
	r := newReconciler(store, cfg)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	pool := getPool(t, store, testProfileName)
	if !equality.Semantic.DeepEqual(pool.Spec.CellTemplate, seedTemplate()) {
		t.Fatalf("cellTemplate = %+v, want %+v", pool.Spec.CellTemplate, seedTemplate())
	}
	if pool.Spec.CellTemplate == r.Config.CellTemplate {
		t.Error("the seeded pool must hold a copy of the configured template, not the same value")
	}

	cells := listCells(t, store)
	if len(cells) != 1 {
		t.Fatalf("cells = %v, want one", cellNames(cells))
	}
	cell := cells[0]
	if !equality.Semantic.DeepEqual(cell.Spec.Resources, memoryRequest("3Gi")) {
		t.Errorf("resources = %+v, want 3Gi", cell.Spec.Resources)
	}
	if cell.Spec.MiteSpec == nil || !equality.Semantic.DeepEqual(cell.Spec.MiteSpec.Resources, memoryRequest("32Mi")) {
		t.Errorf("miteSpec = %+v, want resources 32Mi", cell.Spec.MiteSpec)
	}
	if cell.Spec.PodOverrides == nil || cell.Spec.PodOverrides.JvmOpts != "-XX:MaxRAMPercentage=50" {
		t.Errorf("podOverrides = %+v, want the template jvmOpts", cell.Spec.PodOverrides)
	}
}

// A nil configured template seeds a pool with no template.
func TestCellPool_NilConfiguredTemplateSeedsNone(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), pipelinesTenant())

	r := newReconciler(store, WarmPoolConfig{Enabled: true, PerProfile: 1, MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if tmpl := getPool(t, store, testProfileName).Spec.CellTemplate; tmpl != nil {
		t.Errorf("cellTemplate = %+v, want nil", tmpl)
	}
}

// A pool that exists keeps its own spec: the configured template never
// reaches it.
func TestCellPool_ExistingPoolKeepsItsTemplate(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, newProfile(), pipelinesTenant(), newPool(1))

	r := newReconciler(store, WarmPoolConfig{Enabled: true, PerProfile: 1, MaxTenantsPerCell: 10, CellTemplate: seedTemplate()})
	for i := 0; i < 2; i++ {
		if err := r.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	if tmpl := getPool(t, store, testProfileName).Spec.CellTemplate; tmpl != nil {
		t.Errorf("cellTemplate = %+v, want the existing pool's nil template", tmpl)
	}
	for _, c := range listCells(t, store) {
		if c.Spec.Resources != nil || c.Spec.MiteSpec != nil {
			t.Errorf("cell %s carries %+v / %+v, want no template fields", c.Name, c.Spec.Resources, c.Spec.MiteSpec)
		}
	}
}

// A template edit sets the mite resources of a free cell, keeps its mite
// image, and leaves a placed cell unchanged.
func TestCellPool_TemplateEditRestampsMiteResourcesOfFreeCells(t *testing.T) {
	store := crdstore.NewFake()
	pool := newPool(1)
	pool.Spec.CellTemplate = &v1alpha1.CellTemplate{MiteResources: memoryRequest("64Mi")}
	free := poolCellNamed("cell-lts-2-555-0", testProfileName, nil)
	free.Spec.MiteSpec = &v1alpha1.MiteSpec{Image: "mite:pinned", ImagePullPolicy: corev1.PullAlways}
	placed := poolCellNamed("cell-lts-2-555-1", testProfileName, []string{"team-a"})
	crdstore.MustSeed(store, newProfile(), pool, free, placed)

	r := newReconciler(store, WarmPoolConfig{MaxTenantsPerCell: 10})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := getCell(t, store, "cell-lts-2-555-0")
	if got.Spec.MiteSpec == nil {
		t.Fatal("the free cell lost its miteSpec")
	}
	if !equality.Semantic.DeepEqual(got.Spec.MiteSpec.Resources, memoryRequest("64Mi")) {
		t.Errorf("mite resources = %+v, want 64Mi", got.Spec.MiteSpec.Resources)
	}
	if got.Spec.MiteSpec.Image != "mite:pinned" || got.Spec.MiteSpec.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("miteSpec = %+v, want the image and pull policy kept", got.Spec.MiteSpec)
	}
	if p := getCell(t, store, "cell-lts-2-555-1"); p.Spec.MiteSpec != nil {
		t.Errorf("the placed cell must stay unchanged, got %+v", p.Spec.MiteSpec)
	}
}

func TestApplyCellTemplate_MiteSpec(t *testing.T) {
	cases := []struct {
		name string
		in   *v1alpha1.MiteSpec
		tmpl *v1alpha1.CellTemplate
		want *v1alpha1.MiteSpec
	}{
		{
			name: "nil template clears resources and removes an empty miteSpec",
			in:   &v1alpha1.MiteSpec{Resources: memoryRequest("32Mi")},
			tmpl: nil,
			want: nil,
		},
		{
			name: "nil template keeps the image",
			in:   &v1alpha1.MiteSpec{Resources: memoryRequest("32Mi"), Image: "mite:pinned"},
			tmpl: nil,
			want: &v1alpha1.MiteSpec{Image: "mite:pinned"},
		},
		{
			name: "template without miteResources clears resources",
			in:   &v1alpha1.MiteSpec{Resources: memoryRequest("32Mi")},
			tmpl: &v1alpha1.CellTemplate{Resources: memoryRequest("3Gi")},
			want: nil,
		},
		{
			name: "miteResources allocates a miteSpec",
			in:   nil,
			tmpl: &v1alpha1.CellTemplate{MiteResources: memoryRequest("32Mi")},
			want: &v1alpha1.MiteSpec{Resources: memoryRequest("32Mi")},
		},
		{
			name: "nil template with no miteSpec stays nil",
			in:   nil,
			tmpl: nil,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &v1alpha1.ControllerSpec{MiteSpec: tc.in}
			applyCellTemplate(spec, tc.tmpl)
			if !equality.Semantic.DeepEqual(spec.MiteSpec, tc.want) {
				t.Errorf("miteSpec = %+v, want %+v", spec.MiteSpec, tc.want)
			}
		})
	}
}

// The stamp copies the template, so a later edit of the cell spec never
// reaches the pool's template.
func TestApplyCellTemplate_CopiesMiteResources(t *testing.T) {
	tmpl := &v1alpha1.CellTemplate{MiteResources: memoryRequest("32Mi")}
	spec := &v1alpha1.ControllerSpec{}
	applyCellTemplate(spec, tmpl)
	spec.MiteSpec.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("1Gi")
	if !equality.Semantic.DeepEqual(tmpl.MiteResources, memoryRequest("32Mi")) {
		t.Errorf("template changed to %+v", tmpl.MiteResources)
	}
}

func TestEqualCellShape_MiteResources(t *testing.T) {
	a := &v1alpha1.ControllerSpec{MiteSpec: &v1alpha1.MiteSpec{Resources: memoryRequest("32Mi")}}
	b := &v1alpha1.ControllerSpec{MiteSpec: &v1alpha1.MiteSpec{Resources: memoryRequest("64Mi")}}
	if equalCellShape(a, b) {
		t.Error("a mite resources change must count as a shape change")
	}
	same := &v1alpha1.ControllerSpec{MiteSpec: &v1alpha1.MiteSpec{Resources: memoryRequest("32Mi")}}
	if !equalCellShape(a, same) {
		t.Error("equal mite resources must count as the same shape")
	}
	if equalCellShape(&v1alpha1.ControllerSpec{}, a) {
		t.Error("a nil miteSpec and set mite resources must differ")
	}
	image := &v1alpha1.ControllerSpec{MiteSpec: &v1alpha1.MiteSpec{Image: "mite:pinned"}}
	if !equalCellShape(&v1alpha1.ControllerSpec{}, image) {
		t.Error("the mite image is not a template field, so it must not count")
	}
}
