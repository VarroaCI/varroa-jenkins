package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// rbacSpecRoleRefs maps each role that spec.rbacSpec accepts to the JenkinsRole
// of its built-in VarroaRole. The set comes from builtinRoles and never from the
// varroa.dev/builtin label. The CRD enum on RBACGroupBinding.Role must list the
// same roles.
func rbacSpecRoleRefs() map[string]string {
	refs := make(map[string]string)
	for _, vr := range builtinRoles() {
		refs[vr.Name] = vr.Spec.JenkinsRoleRef
	}
	return refs
}

// IsRBACSpecRole reports whether spec.rbacSpec accepts role.
func IsRBACSpecRole(role string) bool {
	_, ok := rbacSpecRoleRefs()[role]
	return ok
}

// rbacBindingName is the name of the JenkinsRoleBinding that the operator
// writes for one rbacSpec role of the controller namespace/name. A slash occurs
// in neither part, so the hash gives each controller its own names. The name
// has at most 31 characters for any input.
func rbacBindingName(namespace, name, role string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	return "ctrl-" + hex.EncodeToString(sum[:])[:16] + "-" + role
}

// ownsRBACBinding reports whether b is an rbacSpec binding of the controller
// namespace/name. The labels alone are not proof, because a copy of a generated
// binding keeps them. The name must also be the rbacBindingName of an rbacSpec
// role. The operator never writes the earlier dash-joined name, so a binding at
// that name is not owned, even when its labels name this controller.
func ownsRBACBinding(b *v1alpha1.JenkinsRoleBinding, namespace, name string) bool {
	if b.Labels[v1alpha1.LabelManagedBy] != v1alpha1.ManagedByOperator ||
		b.Labels[v1alpha1.LabelControllerNamespace] != namespace ||
		b.Labels[v1alpha1.LabelControllerName] != name {
		return false
	}
	for role := range rbacSpecRoleRefs() {
		if b.Name == rbacBindingName(namespace, name, role) {
			return true
		}
	}
	return false
}

// RBACSpecBindingOwner returns the controller whose spec.rbacSpec writes b.
// It reads the controller from the labels of b. It returns ok only when
// ownsRBACBinding accepts b for that controller.
func RBACSpecBindingOwner(b *v1alpha1.JenkinsRoleBinding) (namespace, name string, ok bool) {
	if b == nil {
		return "", "", false
	}
	namespace = b.Labels[v1alpha1.LabelControllerNamespace]
	name = b.Labels[v1alpha1.LabelControllerName]
	if namespace == "" || name == "" || !ownsRBACBinding(b, namespace, name) {
		return "", "", false
	}
	return namespace, name, true
}

// ownedRBACBindings lists the JenkinsRoleBindings that ownsRBACBinding accepts for cr.
func (r *Reconciler) ownedRBACBindings(ctx context.Context, cr *v1alpha1.Controller) ([]*v1alpha1.JenkinsRoleBinding, error) {
	all, err := crdstore.List[v1alpha1.JenkinsRoleBinding](ctx, r.store, "", "")
	if err != nil {
		return nil, fmt.Errorf("list JenkinsRoleBindings: %w", err)
	}
	var owned []*v1alpha1.JenkinsRoleBinding
	for _, b := range all {
		if ownsRBACBinding(b, cr.Namespace, cr.Name) {
			owned = append(owned, b)
		}
	}
	return owned, nil
}

// deleteOwnedRBACBinding deletes b only while it keeps the uid and the
// resourceVersion of the list read. A conflict means that another writer
// changed or replaced b after that read, so the binding stays.
func (r *Reconciler) deleteOwnedRBACBinding(ctx context.Context, b *v1alpha1.JenkinsRoleBinding) {
	err := crdstore.DeleteIfUnchanged[v1alpha1.JenkinsRoleBinding](ctx, r.store, b.Name, "", string(b.UID), b.ResourceVersion)
	switch {
	case err == nil:
	case apierrors.IsConflict(err):
		r.Logger.Info("JenkinsRoleBinding changed after the list, delete skipped", "binding", b.Name)
	default:
		r.Logger.Warn("failed to delete JenkinsRoleBinding", "binding", b.Name, "error", err)
	}
}

// buildDesiredRoleBindings maps cr.Spec.RBACSpec into cluster-scoped JenkinsRoleBindings.
// It returns the desired bindings and the role names that rbacSpecRoleRefs does not hold.
func buildDesiredRoleBindings(cr *v1alpha1.Controller) ([]*v1alpha1.JenkinsRoleBinding, []string) {
	if cr.Spec.RBACSpec == nil || len(cr.Spec.RBACSpec.Groups) == 0 {
		return nil, nil
	}

	refs := rbacSpecRoleRefs()

	// Group rbacSpec.groups by role.
	roleGroups := make(map[string][]v1alpha1.RBACGroupBinding)
	for _, g := range cr.Spec.RBACSpec.Groups {
		roleGroups[g.Role] = append(roleGroups[g.Role], g)
	}

	var bindings []*v1alpha1.JenkinsRoleBinding
	var unknown []string

	for role, groups := range roleGroups {
		roleRef, ok := refs[role]
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
				Name: rbacBindingName(cr.Namespace, cr.Name, role),
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
		// Labels-only merge patch, not a full-spec Apply: the operator must
		// claim no spec ownership, and a whole-object write would transfer
		// ownership of every spec field present in the object. patchTickMeta
		// issues {"metadata":{"labels":{...}}} under the operator's own field
		// manager, never "varroa-ui".
		if err := r.patchTickMeta(ctx, cr, map[string]any{"labels": map[string]any{v1alpha1.LabelControllerName: cr.Name}}); err != nil {
			return fmt.Errorf("set controller label: %w", err)
		}
	}

	// Compute desired bindings.
	desired, unknown := buildDesiredRoleBindings(cr)
	for _, u := range unknown {
		r.Logger.Warn("unknown rbac role, skipping", "controller", cr.Namespace+"/"+cr.Name, "role", u)
	}

	actual, err := r.ownedRBACBindings(ctx, cr)
	if err != nil {
		return err
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

	// Delete owned bindings not in desired.
	for _, a := range actual {
		if _, ok := desiredByName[a.Name]; !ok {
			r.deleteOwnedRBACBinding(ctx, a)
		}
	}

	return nil
}

// deleteControllerBindings removes the JenkinsRoleBindings that this controller owns.
func (r *Reconciler) deleteControllerBindings(ctx context.Context, cr *v1alpha1.Controller) error {
	owned, err := r.ownedRBACBindings(ctx, cr)
	if err != nil {
		return err
	}
	for _, b := range owned {
		r.deleteOwnedRBACBinding(ctx, b)
	}
	return nil
}
