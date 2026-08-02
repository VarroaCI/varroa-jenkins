package controller

import (
	"context"
	"fmt"
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// buildDesiredRoleBindings maps cr.Spec.RBACSpec into cluster-scoped JenkinsRoleBindings.
// builtins must be VarroaRole CRDs labeled varroa.dev/builtin="true".
// Returns the desired bindings and a list of unknown role names.
func buildDesiredRoleBindings(cr *v1alpha1.Controller, builtins []*v1alpha1.VarroaRole) ([]*v1alpha1.JenkinsRoleBinding, []string) {
	if cr.Spec.RBACSpec == nil || len(cr.Spec.RBACSpec.Groups) == 0 {
		return nil, nil
	}

	// Build a lookup from builtin role CRD name → JenkinsRoleRef.
	builtinByRole := make(map[string]string)
	for _, vr := range builtins {
		if vr.Labels != nil && vr.Labels[v1alpha1.LabelBuiltin] == "true" {
			builtinByRole[vr.Name] = vr.Spec.JenkinsRoleRef
		}
	}

	// Group rbacSpec.groups by role.
	roleGroups := make(map[string][]v1alpha1.RBACGroupBinding)
	for _, g := range cr.Spec.RBACSpec.Groups {
		roleGroups[g.Role] = append(roleGroups[g.Role], g)
	}

	var bindings []*v1alpha1.JenkinsRoleBinding
	var unknown []string

	for role, groups := range roleGroups {
		roleRef, ok := builtinByRole[role]
		if !ok {
			unknown = append(unknown, role)
			continue
		}

		subjects := make([]v1alpha1.SubjectRef, 0, len(groups))
		for _, g := range groups {
			subjects = append(subjects, v1alpha1.SubjectRef{
				Kind: "Group",
				Name: g.Name,
			})
		}

		bindings = append(bindings, &v1alpha1.JenkinsRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("ctrl-%s-%s-%s", cr.Namespace, cr.Name, role),
				Labels: map[string]string{
					v1alpha1.LabelManagedBy:           v1alpha1.ManagedByOperator,
					v1alpha1.LabelControllerNamespace: cr.Namespace,
					v1alpha1.LabelControllerName:      cr.Name,
				},
			},
			Spec: v1alpha1.JenkinsRoleBindingSpec{
				Subjects: subjects,
				RoleRef:  roleRef,
				ControllerScope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces:         []string{cr.Namespace},
					ControllerSelector: &metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.LabelControllerName: cr.Name}},
				},
			},
		})
	}

	return bindings, unknown
}

// syncRBACBindings converges the JenkinsRoleBindings owned by this controller.
func (r *Reconciler) syncRBACBindings(ctx context.Context, cr *v1alpha1.Controller) error {
	// Ensure the controller carries the LabelControllerName label for scope matching.
	if cr.Labels == nil {
		cr.Labels = make(map[string]string)
	}
	if cr.Labels[v1alpha1.LabelControllerName] != cr.Name {
		cr.Labels[v1alpha1.LabelControllerName] = cr.Name
		if err := crdstore.Apply[v1alpha1.Controller](ctx, r.store, cr); err != nil {
			return fmt.Errorf("set controller label: %w", err)
		}
	}

	// List built-in VarroaRole CRDs.
	allRoles, err := crdstore.List[v1alpha1.VarroaRole](ctx, r.store, "", "")
	if err != nil {
		return fmt.Errorf("list VarroaRoles: %w", err)
	}

	// Compute desired bindings.
	desired, unknown := buildDesiredRoleBindings(cr, allRoles)
	for _, u := range unknown {
		r.Logger.Warn("unknown rbac role, skipping", "controller", cr.Namespace+"/"+cr.Name, "role", u)
	}

	// List all JenkinsRoleBindings and filter by ownership labels in Go.
	allBindings, err := crdstore.List[v1alpha1.JenkinsRoleBinding](ctx, r.store, "", "")
	if err != nil {
		return fmt.Errorf("list JenkinsRoleBindings: %w", err)
	}

	var actual []*v1alpha1.JenkinsRoleBinding
	for _, b := range allBindings {
		if b.Labels != nil &&
			b.Labels[v1alpha1.LabelManagedBy] == v1alpha1.ManagedByOperator &&
			b.Labels[v1alpha1.LabelControllerNamespace] == cr.Namespace &&
			b.Labels[v1alpha1.LabelControllerName] == cr.Name {
			actual = append(actual, b)
		}
	}

	// Create or update desired bindings.
	desiredByName := make(map[string]*v1alpha1.JenkinsRoleBinding)
	for _, d := range desired {
		desiredByName[d.Name] = d
	}

	for _, d := range desired {
		var exists bool
		for _, a := range actual {
			if a.Name == d.Name {
				exists = true
				if !reflect.DeepEqual(a.Spec, d.Spec) {
					if err := crdstore.Apply[v1alpha1.JenkinsRoleBinding](ctx, r.store, d); err != nil {
						return fmt.Errorf("update JenkinsRoleBinding %s: %w", d.Name, err)
					}
				}
				break
			}
		}
		if !exists {
			if err := crdstore.Apply[v1alpha1.JenkinsRoleBinding](ctx, r.store, d); err != nil {
				return fmt.Errorf("create JenkinsRoleBinding %s: %w", d.Name, err)
			}
		}
	}

	// Delete actual bindings not in desired.
	for _, a := range actual {
		if _, ok := desiredByName[a.Name]; !ok {
			if err := crdstore.Delete[v1alpha1.JenkinsRoleBinding](ctx, r.store, a.Name, ""); err != nil {
				r.Logger.Warn("failed to delete JenkinsRoleBinding", "binding", a.Name, "error", err)
			}
		}
	}

	return nil
}

// deleteControllerBindings removes all JenkinsRoleBindings owned by this controller.
func (r *Reconciler) deleteControllerBindings(ctx context.Context, cr *v1alpha1.Controller) error {
	allBindings, err := crdstore.List[v1alpha1.JenkinsRoleBinding](ctx, r.store, "", "")
	if err != nil {
		return fmt.Errorf("list JenkinsRoleBindings: %w", err)
	}

	for _, b := range allBindings {
		if b.Labels != nil &&
			b.Labels[v1alpha1.LabelManagedBy] == v1alpha1.ManagedByOperator &&
			b.Labels[v1alpha1.LabelControllerNamespace] == cr.Namespace &&
			b.Labels[v1alpha1.LabelControllerName] == cr.Name {
			if err := crdstore.Delete[v1alpha1.JenkinsRoleBinding](ctx, r.store, b.Name, ""); err != nil {
				r.Logger.Warn("failed to delete JenkinsRoleBinding", "binding", b.Name, "error", err)
			}
		}
	}

	return nil
}
