package teamagents

import (
	"context"
	"log/slog"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

const releaseNS = "varroa-system"

func teamAgent(ns, name, team string) *v1alpha1.TeamAgent {
	return &v1alpha1.TeamAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.TeamAgentSpec{TeamRef: v1alpha1.TeamAgentTeamRef{Name: team}},
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
		&v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}, Spec: v1alpha1.TeamSpec{Namespaces: []string{"ns-a"}}},
		teamAgent("ns-a", "agent-a", "team-a"),
	)
	if err := newReconciler(f).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := bindings(t, f)
	if len(got) != 1 {
		t.Fatalf("bindings = %d, want 1", len(got))
	}
	b := got[0]
	if want := BindingName("ns-a", "agent-a"); b.Name != want {
		t.Errorf("name = %q, want %q", b.Name, want)
	}
	wantLabels := map[string]string{
		v1alpha1.LabelTeamAgent:          "true",
		v1alpha1.LabelTeamAgentNamespace: "ns-a",
		v1alpha1.LabelTeamAgentName:      "agent-a",
	}
	for k, v := range wantLabels {
		if b.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, b.Labels[k], v)
		}
	}
	if len(b.Spec.Subjects) != 1 || b.Spec.Subjects[0].Kind != "User" || b.Spec.Subjects[0].Name != "agent:ns-a/team-a/agent-a" {
		t.Errorf("subjects = %+v", b.Spec.Subjects)
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
		&v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}, Spec: v1alpha1.TeamSpec{Namespaces: []string{"ns-a"}}},
		teamAgent("ns-a", "agent-a", "team-a"),
	)
	r := newReconciler(f)
	ctx := context.Background()
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := crdstore.Delete[v1alpha1.TeamAgent](ctx, f, "agent-a", "ns-a"); err != nil {
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
// release namespace and the Team do not own.
func TestTickSkipsForeignNamespace(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f,
		&v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}, Spec: v1alpha1.TeamSpec{Namespaces: []string{"ns-a"}}},
		teamAgent("ns-x", "agent-x", "team-a"),
	)
	if err := newReconciler(f).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(bindings(t, f)); n != 0 {
		t.Fatalf("bindings = %d, want 0", n)
	}
}
