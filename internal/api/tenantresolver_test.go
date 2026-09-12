package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

var resolverClaims = &auth.Claims{Subject: "u"}

// pipelineBinding grants pipelinesRoles()'s "dev" role (pipelines
// read+write) on each named tenant, to subject "u".
func pipelineBinding(name string, tenants ...string) *v1alpha1.VarroaRoleBinding {
	scope := make([]v1alpha1.TenantScope, len(tenants))
	for i, tn := range tenants {
		scope[i] = v1alpha1.TenantScope{Name: tn}
	}
	return &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "dev",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: scope},
		},
	}
}

// resolverAuthorizer builds an Authorizer granting pipelines read+write on
// the named placed-Team tenants (rbac.TenantTeam kind).
func resolverAuthorizer(tenants ...string) *Authorizer {
	bindings := []*v1alpha1.VarroaRoleBinding{pipelineBinding("b", tenants...)}
	return NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
}

// namespaceAuthorizer builds an Authorizer granting pipelines read+write on
// every synthetic controller tenant (rbac.TenantController kind) in the
// named namespaces. A controller tenant's grant path is the binding
// scope's namespace allowlist, not its tenants list.
func namespaceAuthorizer(namespaces ...string) *Authorizer {
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "dev",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: namespaces},
		},
	}
	return NewAuthorizer(testResolver(pipelinesRoles(), []*v1alpha1.VarroaRoleBinding{binding}), false)
}

// placedTeamObject builds the unstructured Team object design D1 step 1
// reads: spec.pipelines present, and status.placement naming the cell.
func placedTeamObject(name, cluster, cellNamespace, cell string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "Team",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"namespaces": []any{cellNamespace},
			"pipelines":  map[string]any{},
		},
		"status": map[string]any{
			"placement": map[string]any{
				"cluster":       cluster,
				"cellNamespace": cellNamespace,
				"cell":          cell,
			},
		},
	}}
}

// placedTeamObjectWithBundle extends placedTeamObject with
// spec.pipelines.composedBundleRef, for a placed Team whose own tenant
// bundle, not the cell's, delivers its pipeline templates (design D13).
func placedTeamObjectWithBundle(name, cluster, cellNamespace, cell, bundleName, bundleNamespace string) *unstructured.Unstructured {
	obj := placedTeamObject(name, cluster, cellNamespace, cell)
	_ = unstructured.SetNestedMap(obj.Object, map[string]any{
		"name":      bundleName,
		"namespace": bundleNamespace,
	}, "spec", "pipelines", "composedBundleRef")
	return obj
}

// unplacedTeamObject builds a Team object carrying neither spec.pipelines
// nor status.placement.cell, so step 2 fans out over spec.namespaces.
func unplacedTeamObject(name string, namespaces ...string) *unstructured.Unstructured {
	nsIface := make([]any, len(namespaces))
	for i, ns := range namespaces {
		nsIface[i] = ns
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "Team",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"namespaces": nsIface},
	}}
}

// seedTeam creates a Team object directly through the unstructured
// Backend, bypassing the typed Seed helper, because spec.pipelines and
// status.placement are not yet fields of v1alpha1.Team.
func seedTeam(t *testing.T, store *crdstore.Fake, obj *unstructured.Unstructured) {
	t.Helper()
	gvr, err := crdstore.GVRFor[v1alpha1.Team]()
	if err != nil {
		t.Fatalf("GVRFor[Team]: %v", err)
	}
	if _, err := store.CreateObject(context.Background(), gvr, "", obj); err != nil {
		t.Fatalf("seed team %q: %v", obj.GetName(), err)
	}
}

func clusterController(cluster, namespace, name string) ClusterController {
	return ClusterController{Cluster: cluster, CR: &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}}
}

// fakeResolverBrood returns a fixed controller set from ListAll,
// regardless of the namespace and cluster filter arguments — the resolver
// filters client-side.
type fakeResolverBrood struct {
	Brood
	controllers []ClusterController
}

func (f fakeResolverBrood) ListAll(context.Context, string, string) ([]ClusterController, []ClusterFanoutStatus, error) {
	return f.controllers, nil, nil
}

// failListAllBrood fails the test if ListAll is called. Step 1 (a placed
// Team) SHALL run no brood-index lookup.
type failListAllBrood struct {
	Brood
	t *testing.T
}

func (f failListAllBrood) ListAll(context.Context, string, string) ([]ClusterController, []ClusterFanoutStatus, error) {
	f.t.Helper()
	f.t.Fatal("ListAll must not be called resolving a placed Team")
	return nil, nil, nil
}

// --------------------------------------------------------------------------
// Step 1: a placed Team resolves to one tenant, with no brood-index lookup.
// --------------------------------------------------------------------------

func TestTenantResolver_Step1_PlacedTeamResolvesToOneTenant(t *testing.T) {
	store := crdstore.NewFake()
	seedTeam(t, store, placedTeamObject("payments", "core", "varroa-cells", "cell-a"))

	resolver := NewTeamTenantResolver(store, failListAllBrood{t: t}, resolverAuthorizer("payments"))

	refs, err := resolver.Resolve(context.Background(), resolverClaims, "payments")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1", len(refs))
	}
	want := TenantRef{Cluster: "core", Namespace: "varroa-cells", Controller: "cell-a", Tenant: "payments", Team: "payments", Folder: "payments"}
	if refs[0] != want {
		t.Errorf("got %+v, want %+v", refs[0], want)
	}
}

// --------------------------------------------------------------------------
// Step 2: a Team with no placement fans out over its namespaces' controllers.
// This also covers "a Team object that carries neither placement path".
// --------------------------------------------------------------------------

func TestTenantResolver_Step2_UnplacedTeamFansOutOverNamespaces(t *testing.T) {
	store := crdstore.NewFake()
	seedTeam(t, store, unplacedTeamObject("payments", "team-pay"))

	brood := fakeResolverBrood{controllers: []ClusterController{
		clusterController("core", "team-pay", "svc-a"),
		clusterController("core", "team-pay", "svc-b"),
		clusterController("core", "other-ns", "svc-c"), // outside spec.namespaces
	}}
	resolver := NewTeamTenantResolver(store, brood, namespaceAuthorizer("team-pay"))

	refs, err := resolver.Resolve(context.Background(), resolverClaims, "payments")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want 2: %+v", len(refs), refs)
	}
	tenants := map[string]bool{}
	for _, r := range refs {
		tenants[r.Tenant] = true
		if r.Folder != "" {
			t.Errorf("ref %+v: want empty Folder", r)
		}
	}
	if !tenants["team-pay/svc-a"] || !tenants["team-pay/svc-b"] {
		t.Errorf("got tenants %v, want team-pay/svc-a and team-pay/svc-b", tenants)
	}
}

// --------------------------------------------------------------------------
// Step 3: a controller with no Team is its own tenant.
// --------------------------------------------------------------------------

func TestTenantResolver_Step3_ControllerWithNoTeamIsItsOwnTenant(t *testing.T) {
	store := crdstore.NewFake()
	brood := fakeResolverBrood{controllers: []ClusterController{
		clusterController("core", "default", "ci"),
	}}
	resolver := NewTeamTenantResolver(store, brood, namespaceAuthorizer("default"))

	refs, err := resolver.Resolve(context.Background(), resolverClaims, "ci")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1", len(refs))
	}
	want := TenantRef{Cluster: "core", Namespace: "default", Controller: "ci", Tenant: "default/ci", Team: "ci", Folder: ""}
	if refs[0] != want {
		t.Errorf("got %+v, want %+v", refs[0], want)
	}
}

// --------------------------------------------------------------------------
// Step 4: two controllers of one name on two clusters is ambiguous, 409.
// --------------------------------------------------------------------------

func TestTenantResolver_Step4_AmbiguousControllerNameIs409(t *testing.T) {
	store := crdstore.NewFake()
	brood := fakeResolverBrood{controllers: []ClusterController{
		clusterController("core", "default", "ci"),
		clusterController("hive-b", "default", "ci"),
	}}
	resolver := NewTeamTenantResolver(store, brood, namespaceAuthorizer("default"))

	_, err := resolver.Resolve(context.Background(), resolverClaims, "ci")
	var serr *ServiceError
	if !errors.As(err, &serr) {
		t.Fatalf("Resolve error = %v, want *ServiceError", err)
	}
	if serr.Status != http.StatusConflict {
		t.Errorf("Status = %d, want %d", serr.Status, http.StatusConflict)
	}
	if !strings.Contains(serr.Message, "core") || !strings.Contains(serr.Message, "hive-b") {
		t.Errorf("message %q does not name both clusters", serr.Message)
	}
}

// --------------------------------------------------------------------------
// Step 5: the merged 404, for an absent team and for an unreadable one.
// --------------------------------------------------------------------------

func TestTenantResolver_Step5_AbsentTeamIs404(t *testing.T) {
	store := crdstore.NewFake()
	brood := fakeResolverBrood{}
	resolver := NewTeamTenantResolver(store, brood, namespaceAuthorizer())

	_, err := resolver.Resolve(context.Background(), resolverClaims, "ghost")
	assertNotFound(t, err)
}

func TestTenantResolver_Step5_UnreadableTeamIs404(t *testing.T) {
	store := crdstore.NewFake()
	brood := fakeResolverBrood{controllers: []ClusterController{
		clusterController("core", "default", "ci"),
	}}
	// No tenant read grant: the candidate resolves, but the read check
	// admits nothing, and this must read exactly like an absent team.
	resolver := NewTeamTenantResolver(store, brood, namespaceAuthorizer())

	_, err := resolver.Resolve(context.Background(), resolverClaims, "ci")
	assertNotFound(t, err)
}

func TestTenantResolver_Step5_UnreadableTeamObjectIs404(t *testing.T) {
	store := crdstore.NewFake()
	gvr, err := crdstore.GVRFor[v1alpha1.Team]()
	if err != nil {
		t.Fatalf("GVRFor[Team]: %v", err)
	}
	// A non-NotFound GetObject error must not fall through to the
	// synthetic-controller path: it merges into the same 404 an absent
	// team gets, so a caller cannot tell a Team it may not read from one
	// that does not exist.
	store.FailNext("get", gvr, errors.New("forbidden"))
	brood := fakeResolverBrood{controllers: []ClusterController{
		clusterController("core", "default", "payments"),
	}}
	resolver := NewTeamTenantResolver(store, brood, namespaceAuthorizer("default"))

	_, rerr := resolver.Resolve(context.Background(), resolverClaims, "payments")
	assertNotFound(t, rerr)
}

// --------------------------------------------------------------------------
// The 20-tenant ceiling.
// --------------------------------------------------------------------------

func TestTenantResolver_TeamOverTheCeilingIs422(t *testing.T) {
	store := crdstore.NewFake()
	seedTeam(t, store, unplacedTeamObject("big", "team-big"))

	controllers := make([]ClusterController, 21)
	for i := range controllers {
		name := "svc" + string(rune('a'+i))
		controllers[i] = clusterController("core", "team-big", name)
	}
	brood := fakeResolverBrood{controllers: controllers}
	resolver := NewTeamTenantResolver(store, brood, namespaceAuthorizer("team-big"))

	_, err := resolver.Resolve(context.Background(), resolverClaims, "big")
	var serr *ServiceError
	if !errors.As(err, &serr) {
		t.Fatalf("Resolve error = %v, want *ServiceError", err)
	}
	if serr.Status != http.StatusUnprocessableEntity {
		t.Errorf("Status = %d, want %d", serr.Status, http.StatusUnprocessableEntity)
	}
	if !strings.Contains(serr.Message, "21") {
		t.Errorf("message %q does not name the count", serr.Message)
	}
}

// --------------------------------------------------------------------------
// ResolveRun's team-segment rule.
// --------------------------------------------------------------------------

func TestTenantResolver_ResolveRun_ControllerTenantYieldsControllerName(t *testing.T) {
	resolver := NewTeamTenantResolver(crdstore.NewFake(), fakeResolverBrood{}, resolverAuthorizer())
	run := recordstore.Run{Cluster: "core", Namespace: "team-pay", Cell: "ci", Tenant: "team-pay/ci"}

	ref, err := resolver.ResolveRun(context.Background(), resolverClaims, run)
	if err != nil {
		t.Fatalf("ResolveRun: %v", err)
	}
	want := TenantRef{Cluster: "core", Namespace: "team-pay", Controller: "ci", Tenant: "team-pay/ci", Team: "ci", Folder: ""}
	if ref != want {
		t.Errorf("got %+v, want %+v", ref, want)
	}
}

func TestTenantResolver_ResolveRun_PlacedTenantYieldsTeamName(t *testing.T) {
	resolver := NewTeamTenantResolver(crdstore.NewFake(), fakeResolverBrood{}, resolverAuthorizer())
	run := recordstore.Run{Cluster: "core", Namespace: "varroa-cells", Cell: "cell-a", Tenant: "payments"}

	ref, err := resolver.ResolveRun(context.Background(), resolverClaims, run)
	if err != nil {
		t.Fatalf("ResolveRun: %v", err)
	}
	want := TenantRef{Cluster: "core", Namespace: "varroa-cells", Controller: "cell-a", Tenant: "payments", Team: "payments", Folder: "payments"}
	if ref != want {
		t.Errorf("got %+v, want %+v", ref, want)
	}
}

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

func assertNotFound(t *testing.T, err error) {
	t.Helper()
	var serr *ServiceError
	if !errors.As(err, &serr) {
		t.Fatalf("error = %v, want *ServiceError", err)
	}
	if serr.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want %d", serr.Status, http.StatusNotFound)
	}
}
