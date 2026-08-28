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
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// guardStub is a stateful ResourceClient for the MCP tests.
// It embeds the stateless stubClient and overrides only the methods the
// controller read/validation paths exercise.
type guardStub struct {
	*stubClient
	*crdstore.Fake
	controllers map[string]*v1alpha1.Controller
	profiles    []*v1alpha1.JenkinsVersionProfile
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

// TestMCPControllerMutationsRouteThroughBroodWithVarroaUI (5.7): the MCP
// local direct-apply fallback is gone, so create/update leave no spec owner
// other than varroa-ui by construction — the only write path is the bus, and
// update passes fieldManager "varroa-ui" to Brood.Update.
func TestMCPControllerMutationsRouteThroughBroodWithVarroaUI(t *testing.T) {
	brood := &recordingBrood{}
	handler := NewHandler(broodHandler(brood).deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "update_controller",
		"arguments": map[string]interface{}{"namespace": "team-a", "name": "ci", "powerState": "Running"},
	}, guardClaims)
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("update via brood failed: %q", toolText(tr))
	}
	if !brood.updateCalled {
		t.Fatal("update_controller must route through Brood.Update")
	}
	if brood.updateActor != "varroa-ui" {
		t.Errorf("Brood.Update field manager = %q, want varroa-ui", brood.updateActor)
	}
}

func toolText(tr toolResult) string {
	var b strings.Builder
	for _, c := range tr.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}
