// Package teamagents keeps one VarroaRoleBinding per accepted TeamAgent. The
// binding grants the agent's principal the developer role on its Team, and
// nothing else. The operator is the only writer of these bindings.
package teamagents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/accept"
)

// developerRole is the built-in role that a Team agent holds on its Team.
const developerRole = "developer"

// Reconciler applies and prunes the agent bindings.
type Reconciler struct {
	Backend          crdstore.Backend
	ReleaseNamespace string
	Logger           *slog.Logger
}

// BindingName returns the name of the binding of one TeamAgent. The hash keeps
// the name short and valid for any namespace and name.
func BindingName(ns, name string) string {
	sum := sha256.Sum256([]byte(ns + "/" + name))
	return "teamagent-" + hex.EncodeToString(sum[:])[:16]
}

// DesiredBinding returns the binding that one accepted TeamAgent needs.
func DesiredBinding(ta *v1alpha1.TeamAgent) *v1alpha1.VarroaRoleBinding {
	return &v1alpha1.VarroaRoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: "VarroaRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name: BindingName(ta.Namespace, ta.Name),
			Labels: map[string]string{
				v1alpha1.LabelTeamAgent:          "true",
				v1alpha1.LabelTeamAgentNamespace: ta.Namespace,
				v1alpha1.LabelTeamAgentName:      ta.Name,
			},
		},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: accept.Principal(ta)}},
			RoleRef:  developerRole,
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Tenants: []v1alpha1.TenantScope{{Name: ta.Spec.TeamRef.Name}},
			},
		},
	}
}

// Tick applies one binding per accepted object and deletes each labelled
// binding that no accepted object needs.
func (r *Reconciler) Tick(ctx context.Context) error {
	in, err := r.input(ctx)
	if err != nil {
		return err
	}
	verdicts := accept.Decide(in)
	want := map[string]bool{}
	for i := range in.TeamAgents {
		ta := &in.TeamAgents[i]
		v := verdicts[types.NamespacedName{Namespace: ta.Namespace, Name: ta.Name}]
		if !v.Accepted {
			continue
		}
		b := DesiredBinding(ta)
		if err := crdstore.Apply(ctx, r.Backend, b); err != nil {
			return fmt.Errorf("apply binding for %s/%s: %w", ta.Namespace, ta.Name, err)
		}
		want[b.Name] = true
	}
	bindings, err := crdstore.List[v1alpha1.VarroaRoleBinding](ctx, r.Backend, "", v1alpha1.LabelTeamAgent+"=true")
	if err != nil {
		return err
	}
	for _, b := range bindings {
		if want[b.Name] {
			continue
		}
		if err := crdstore.Delete[v1alpha1.VarroaRoleBinding](ctx, r.Backend, b.Name, ""); err != nil {
			return fmt.Errorf("delete stale binding %s: %w", b.Name, err)
		}
		if r.Logger != nil {
			r.Logger.Info("team agent binding removed", "binding", b.Name)
		}
	}
	return nil
}

// input reads the TeamAgent and Team objects that accept.Decide needs.
func (r *Reconciler) input(ctx context.Context) (accept.Input, error) {
	agents, err := crdstore.List[v1alpha1.TeamAgent](ctx, r.Backend, "", "")
	if err != nil {
		return accept.Input{}, err
	}
	teams, err := crdstore.List[v1alpha1.Team](ctx, r.Backend, "", "")
	if err != nil {
		return accept.Input{}, err
	}
	in := accept.Input{ReleaseNamespace: r.ReleaseNamespace, Teams: map[string]*v1alpha1.Team{}}
	for _, ta := range agents {
		in.TeamAgents = append(in.TeamAgents, *ta)
	}
	for _, t := range teams {
		in.Teams[t.Name] = t
	}
	return in, nil
}
