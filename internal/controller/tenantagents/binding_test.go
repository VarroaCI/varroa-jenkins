package tenantagents

import (
	"context"
	"log/slog"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

const releaseNS = "varroa-system"

func tenantAgent(ns, name, tenant string) *v1alpha1.TenantAgent {
	return &v1alpha1.TenantAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.TenantAgentSpec{TenantRef: v1alpha1.TenantAgentTenantRef{Name: tenant}},
	}
}

func newReconciler(f *crdstore.Fake) *Reconciler {
	return &Reconciler{Backend: f, ReleaseNamespace: releaseNS, Logger: slog.Default()}
}

func bindings(t *testing.T, f *crdstore.Fake) []*v1alpha1.VarroaRoleBinding {
	t.Helper()
	list, err := crdstore.List[v1alpha1.VarroaRoleBinding](context.Background(), f, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// TestTickAppliesBinding checks the exact shape of the binding of one accepted
// agent.
func TestTickAppliesBinding(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f,
		&v1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}, Spec: v1alpha1.TenantSpec{Namespaces: []string{"ns-a"}}},
		tenantAgent("ns-a", "agent-a", "team-a"),
	)
	if err := newReconciler(f).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := bindings(t, f)
	if len(got) != 1 {
		t.Fatalf("bindings = %d, want 1", len(got))
	}
	b := got[0]
	if want := v1alpha1.TenantAgentBindingName("ns-a", "agent-a"); b.Name != want {
		t.Errorf("name = %q, want %q", b.Name, want)
	}
	wantLabels := map[string]string{
		v1alpha1.LabelTenantAgent:          "true",
		v1alpha1.LabelTenantAgentNamespace: "ns-a",
		v1alpha1.LabelTenantAgentName:      "agent-a",
	}
	for k, v := range wantLabels {
		if b.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, b.Labels[k], v)
		}
	}
	if len(b.Spec.Subjects) != 1 || b.Spec.Subjects[0].Kind != "User" || b.Spec.Subjects[0].Name != "agent:ns-a/team-a/agent-a" {
		t.Errorf("subjects = %+v", b.Spec.Subjects)
	}
	if o, ok := v1alpha1.VarroaRoleBindingOwner(b); !ok || o != (v1alpha1.BindingOwner{Kind: v1alpha1.BindingOwnerTenantAgent, Namespace: "ns-a", Name: "agent-a"}) {
		t.Errorf("owner = %+v, %v; want the TenantAgent ns-a/agent-a", o, ok)
	}
	if b.Spec.RoleRef != "developer" {
		t.Errorf("roleRef = %q, want developer", b.Spec.RoleRef)
	}
	if b.Spec.Scope == nil || len(b.Spec.Scope.Tenants) != 1 || b.Spec.Scope.Tenants[0].Name != "team-a" {
		t.Errorf("scope = %+v", b.Spec.Scope)
	}
}

// TestTickDeletesOnLoss removes the binding when its agent object goes away.
func TestTickDeletesOnLoss(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f,
		&v1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}, Spec: v1alpha1.TenantSpec{Namespaces: []string{"ns-a"}}},
		tenantAgent("ns-a", "agent-a", "team-a"),
	)
	r := newReconciler(f)
	ctx := context.Background()
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := crdstore.Delete[v1alpha1.TenantAgent](ctx, f, "agent-a", "ns-a"); err != nil {
		t.Fatal(err)
	}
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(bindings(t, f)); n != 0 {
		t.Fatalf("bindings after the agent is gone = %d, want 0", n)
	}
}

// TestTickIgnoresUnlabelled leaves bindings that the operator did not write.
func TestTickIgnoresUnlabelled(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "hand-written"},
		Spec:       v1alpha1.VarroaRoleBindingSpec{RoleRef: "viewer"},
	})
	if err := newReconciler(f).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(bindings(t, f)); n != 1 {
		t.Fatalf("bindings = %d, want the unlabelled one only", n)
	}
}

// TestTickSkipsForeignNamespace writes no binding for an agent that the
// release namespace and the Tenant do not own.
func TestTickSkipsForeignNamespace(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f,
		&v1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}, Spec: v1alpha1.TenantSpec{Namespaces: []string{"ns-a"}}},
		tenantAgent("ns-x", "agent-x", "team-a"),
	)
	if err := newReconciler(f).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(bindings(t, f)); n != 0 {
		t.Fatalf("bindings = %d, want 0", n)
	}
}

// agentLabels returns the labels that DesiredBinding writes for ns/name.
func agentLabels(ns, name string) map[string]string {
	return map[string]string{
		v1alpha1.LabelTenantAgent:          "true",
		v1alpha1.LabelTenantAgentNamespace: ns,
		v1alpha1.LabelTenantAgentName:      name,
	}
}

// TestTickPrunesOnlyOwnedBindings deletes a labelled binding only when
// v1alpha1.VarroaRoleBindingOwner names a TenantAgent that is not accepted.
func TestTickPrunesOnlyOwnedBindings(t *testing.T) {
	cases := []struct {
		name    string
		binding *v1alpha1.VarroaRoleBinding
		agent   bool
		kept    bool
	}{
		{
			name: "stale owned binding",
			binding: &v1alpha1.VarroaRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.TenantAgentBindingName("ns-old", "gone"), Labels: agentLabels("ns-old", "gone")},
			},
			kept: false,
		},
		{
			name: "copy of a stale binding under another name",
			binding: &v1alpha1.VarroaRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "gone-copy", Labels: agentLabels("ns-old", "gone")},
			},
			kept: true,
		},
		{
			name: "copy of a live binding under another name",
			binding: &v1alpha1.VarroaRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "agent-a-copy", Labels: agentLabels("ns-a", "agent-a")},
			},
			agent: true,
			kept:  true,
		},
		{
			name: "derived name with the labels of another agent",
			binding: &v1alpha1.VarroaRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.TenantAgentBindingName("ns-old", "gone"), Labels: agentLabels("ns-old", "other")},
			},
			kept: true,
		},
		{
			name: "label without the owner labels",
			binding: &v1alpha1.VarroaRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "bare-label", Labels: map[string]string{v1alpha1.LabelTenantAgent: "true"}},
			},
			kept: true,
		},
		{
			name: "brood schedule binding with the tenant-agent label",
			binding: &v1alpha1.VarroaRoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:   v1alpha1.BroodScheduleBindingName("ns-b", "nightly"),
					Labels: map[string]string{v1alpha1.LabelTenantAgent: "true"},
					Annotations: map[string]string{
						v1alpha1.AnnotationBroodScheduleNamespace: "ns-b",
						v1alpha1.AnnotationBroodScheduleName:      "nightly",
					},
				},
				Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "operator"},
			},
			kept: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := crdstore.NewFake()
			crdstore.MustSeed(f,
				&v1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "tenant-p"}, Spec: v1alpha1.TenantSpec{Namespaces: []string{"ns-a"}}},
				tc.binding.DeepCopy(),
			)
			if tc.agent {
				crdstore.MustSeed(f, tenantAgent("ns-a", "agent-a", "tenant-p"))
			}
			if err := newReconciler(f).Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			names := map[string]bool{}
			for _, b := range bindings(t, f) {
				names[b.Name] = true
			}
			if names[tc.binding.Name] != tc.kept {
				t.Errorf("binding %s kept = %v, want %v", tc.binding.Name, names[tc.binding.Name], tc.kept)
			}
			if live := v1alpha1.TenantAgentBindingName("ns-a", "agent-a"); tc.agent && !names[live] {
				t.Errorf("binding %s of the accepted agent is missing", live)
			}
		})
	}
}
