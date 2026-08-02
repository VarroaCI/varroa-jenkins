package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/jenkinsver"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// guardStub is a stateful ResourceClient for the MCP preflight-parity tests.
// It embeds the stateless stubClient and overrides only the methods the
// create/update preflight path exercises.
type guardStub struct {
	*stubClient
	*crdstore.Fake
	controllers map[string]*v1alpha1.Controller
	profiles    []*v1alpha1.JenkinsVersionProfile
	applied     []string
	namespaces  map[string]bool
}

func newGuardStub() *guardStub {
	return &guardStub{
		stubClient:  &stubClient{},
		Fake:        crdstore.NewFake(),
		controllers: map[string]*v1alpha1.Controller{},
		namespaces:  map[string]bool{},
	}
}

func (g *guardStub) GetControllerCRD(_ context.Context, name, _ string) (*v1alpha1.Controller, error) {
	cr, ok := g.controllers[name]
	if !ok {
		return nil, fmt.Errorf("controllers %q not found", name)
	}
	return cr, nil
}

func (g *guardStub) ApplyControllerCRD(_ context.Context, cr *v1alpha1.Controller) error {
	g.applied = append(g.applied, cr.Name)
	g.controllers[cr.Name] = cr
	return nil
}

func (g *guardStub) TransitionPowerState(_ context.Context, name, namespace, from, to string) (bool, error) {
	return false, nil
}

func (g *guardStub) ListControllerCRDs(_ context.Context, _ string) ([]*v1alpha1.Controller, error) {
	out := make([]*v1alpha1.Controller, 0, len(g.controllers))
	for _, cr := range g.controllers {
		out = append(out, cr)
	}
	return out, nil
}

func (g *guardStub) GetComposedBundleCRD(_ context.Context, _, _ string) (*v1alpha1.ComposedBundle, error) {
	return &v1alpha1.ComposedBundle{Status: v1alpha1.ComposedBundleStatus{
		Phase: v1alpha1.ComposedBundleReady, ResolvedHash: "x",
	}}, nil
}

func (g *guardStub) ListJenkinsVersionProfileCRDs(_ context.Context) ([]*v1alpha1.JenkinsVersionProfile, error) {
	return g.profiles, nil
}

func (g *guardStub) GetJenkinsVersionProfileCRD(_ context.Context, name string) (*v1alpha1.JenkinsVersionProfile, error) {
	for _, p := range g.profiles {
		if p.Name == name {
			return p, nil
		}
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "jenkinsversionprofiles"}, name)
}

func (g *guardStub) CreateJenkinsVersionProfileCRD(_ context.Context, cr *v1alpha1.JenkinsVersionProfile) error {
	g.profiles = append(g.profiles, cr.DeepCopy())
	return nil
}

func (g *guardStub) UpdateJenkinsVersionProfileCRD(_ context.Context, cr *v1alpha1.JenkinsVersionProfile) error {
	for i, p := range g.profiles {
		if p.Name == cr.Name {
			g.profiles[i] = cr.DeepCopy()
			return nil
		}
	}
	g.profiles = append(g.profiles, cr.DeepCopy())
	return nil
}

func (g *guardStub) DeleteJenkinsVersionProfileCRD(_ context.Context, name string) error {
	for i, p := range g.profiles {
		if p.Name == name {
			g.profiles = append(g.profiles[:i], g.profiles[i+1:]...)
			return nil
		}
	}
	return nil
}

func (g *guardStub) GetNamespace(_ context.Context, name string) (*corev1.Namespace, error) {
	if g.namespaces[name] {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, name)
}

func guardAuthorizer() *api.Authorizer {
	roles := []*v1alpha1.VarroaRole{{
		ObjectMeta: metav1.ObjectMeta{Name: "admin"},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}}},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{{
		ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
		Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "admins"}}, RoleRef: "admin"},
	}}
	return api.NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false)
}

var guardClaims = &auth.Claims{Subject: "admin", Groups: []string{"admins"}}

func guardVersions(t *testing.T) (unsafe, safe string) {
	t.Helper()
	segs, ok := jenkinsver.Core(pluginlock.Baseline())
	if !ok || len(segs) < 2 {
		t.Fatalf("unexpected baseline %q", pluginlock.Baseline())
	}
	return fmt.Sprintf("%d.%d", segs[0], segs[1]-1), fmt.Sprintf("%d.%d.1", segs[0], segs[1]+9)
}

// Task 9.3(a): update_controller to core<B0 with no profile → tool error citing
// pluginCoreCompat, CRD NOT applied.
func TestMCPUpdateController_BlocksUnsafeVersionChange(t *testing.T) {
	unsafe, safe := guardVersions(t)
	stub := newGuardStub()
	stub.namespaces["team-a"] = true
	stub.controllers["ci"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: safe},
	}
	crdstore.MustSeed(stub.Fake, stub.controllers["ci"])
	handler := NewHandler(&api.Dependencies{Client: stub, Store: stub, Authorizer: guardAuthorizer()})

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "update_controller",
		"arguments": map[string]interface{}{"namespace": "team-a", "name": "ci", "version": unsafe},
	}, guardClaims)

	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatalf("expected tool error, got success: %v", tr.Content)
	}
	if !strings.Contains(toolText(tr), "pluginCoreCompat") {
		t.Errorf("expected pluginCoreCompat in error, got %q", toolText(tr))
	}
	// Verify no controller was applied (ci still has version=safe)
	updated, err := crdstore.Get[v1alpha1.Controller](context.Background(), stub.Fake, "ci", "team-a")
	if err != nil {
		t.Fatalf("expected ci to still exist: %v", err)
	}
	if updated.Spec.Version != safe {
		t.Errorf("expected ci version=%q, got %q", safe, updated.Spec.Version)
	}
}

// Task 9.3(b): update_controller leaving a grandfathered unsafe version unchanged
// (mutating only composedBundleRef) applies.
func TestMCPUpdateController_AllowsUnchangedGrandfathered(t *testing.T) {
	unsafe, _ := guardVersions(t)
	stub := newGuardStub()
	stub.namespaces["team-a"] = true
	stub.namespaces["varroa-system"] = true
	stub.controllers["ci"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: unsafe},
	}
	crdstore.MustSeed(stub.Fake, stub.controllers["ci"])
	handler := NewHandler(&api.Dependencies{Client: stub, Store: stub, Authorizer: guardAuthorizer()})

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "update_controller",
		"arguments": map[string]interface{}{"namespace": "team-a", "name": "ci", "composedBundleRef": "b"},
	}, guardClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("expected success for unchanged grandfathered version, got error: %q", toolText(tr))
	}
	// Check the controller was applied to the store
	if _, err := crdstore.Get[v1alpha1.Controller](context.Background(), stub.Fake, "ci", "team-a"); err != nil {
		t.Errorf("expected ci to be applied, err=%v", err)
	}
}

// Task 9.3(c): create_controller with a failing draft → tool error, not applied.
func TestMCPCreateController_BlocksUnsafeVersion(t *testing.T) {
	unsafe, _ := guardVersions(t)
	stub := newGuardStub()
	stub.namespaces["team-a"] = true
	stub.namespaces["varroa-system"] = true
	handler := NewHandler(&api.Dependencies{Client: stub, Store: stub, Authorizer: guardAuthorizer()})

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "create_controller",
		"arguments": map[string]interface{}{"namespace": "team-a", "name": "newci", "version": unsafe},
	}, guardClaims)

	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatalf("expected tool error, got success: %v", tr.Content)
	}
	if !strings.Contains(toolText(tr), "pluginCoreCompat") {
		t.Errorf("expected pluginCoreCompat in error, got %q", toolText(tr))
	}
	// Verify no controller was created in the store
	if _, err := crdstore.Get[v1alpha1.Controller](context.Background(), stub.Fake, "newci", "team-a"); err == nil {
		t.Error("CRD must not be applied when blocked")
	}
}

// Task 9.3(d): passing drafts still apply.
func TestMCPCreateController_AllowsSafeVersion(t *testing.T) {
	_, safe := guardVersions(t)
	stub := newGuardStub()
	stub.namespaces["team-a"] = true
	stub.namespaces["varroa-system"] = true
	handler := NewHandler(&api.Dependencies{Client: stub, Store: stub, Authorizer: guardAuthorizer()})

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "create_controller",
		"arguments": map[string]interface{}{"namespace": "team-a", "name": "okci", "version": safe},
	}, guardClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("expected success for safe version, got error: %q", toolText(tr))
	}
	// Verify the controller was created in the store
	if _, err := crdstore.Get[v1alpha1.Controller](context.Background(), stub.Fake, "okci", "team-a"); err != nil {
		t.Errorf("expected okci to be created in store, err=%v", err)
	}
}

func toolText(tr toolResult) string {
	var b strings.Builder
	for _, c := range tr.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

func (g *guardStub) CreateComposedBundleCRD(_ context.Context, _ *v1alpha1.ComposedBundle) error {
	return nil
}
func (g *guardStub) UpdateComposedBundleCRD(_ context.Context, _ *v1alpha1.ComposedBundle) error {
	return nil
}
func (g *guardStub) CreateCatalogSourceCRD(_ context.Context, _ *v1alpha1.CatalogSource) error {
	return nil
}
func (g *guardStub) UpdateCatalogSourceCRD(_ context.Context, _ *v1alpha1.CatalogSource) error {
	return nil
}
func (g *guardStub) CreateJenkinsRoleCRD(_ context.Context, _ *v1alpha1.JenkinsRole) error {
	return nil
}
func (g *guardStub) UpdateJenkinsRoleCRD(_ context.Context, _ *v1alpha1.JenkinsRole) error {
	return nil
}
func (g *guardStub) CreateJenkinsRoleBindingCRD(_ context.Context, _ *v1alpha1.JenkinsRoleBinding) error {
	return nil
}
func (g *guardStub) UpdateJenkinsRoleBindingCRD(_ context.Context, _ *v1alpha1.JenkinsRoleBinding) error {
	return nil
}
