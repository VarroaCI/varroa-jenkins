package controller

import (
	"context"
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/tenancy"
)

// TeamReconciler reconciles Team CRDs by composing them into owned Group +
// VarroaRoleBinding objects.
type TeamReconciler struct {
	client            ResourceClient
	store             crdstore.Backend
	operatorNamespace string
	managedSet        tenancy.ManagedSet
	logger            *slog.Logger
}

// NewTeamReconciler creates a new TeamReconciler. The managed set defaults to
// cluster-wide; in scoped-RBAC mode the caller must inject the real set via
// SetManagedSet or Unmanaged namespaces are never detected (C1).
func NewTeamReconciler(client ResourceClient, store crdstore.Backend, operatorNamespace string, logger *slog.Logger) *TeamReconciler {
	return &TeamReconciler{
		client:            client,
		store:             store,
		operatorNamespace: operatorNamespace,
		managedSet:        tenancy.NewManagedSet("", operatorNamespace),
		logger:            logger,
	}
}

// SetManagedSet injects the managedNamespaces set (the same one the controller
// reconciler's tenancy gate uses) so Team namespace states honor scoped RBAC.
func (r *TeamReconciler) SetManagedSet(set tenancy.ManagedSet) {
	r.managedSet = set
}

// Reconcile reconciles a single Team by name.
func (r *TeamReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	team, err := crdstore.Get[v1alpha1.Team](ctx, r.store, req.Name, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.logger.Info("Team not found, skipping", "team", req.Name)
			return reconcile.Result{}, nil
		}
		r.logger.Error("failed to get Team", "team", req.Name, "error", err)
		return reconcile.Result{}, err
	}

	status := &v1alpha1.TeamStatus{}
	team.Status.DeepCopyInto(status)
	status.ObservedGeneration = team.Generation

	role := defaultRoleRef(team.Spec.RoleRef)

	// --- roleRef guardrail ---
	if role == "admin" {
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.TeamReasonInvalidRoleRef,
			Message: "roleRef 'admin' not permitted on a Team",
		})
		if err := r.patchStatus(ctx, team.Name, status); err != nil {
			r.logger.Error("failed to patch status", "team", team.Name, "error", err)
		}
		return reconcile.Result{}, nil
	}

	if _, err := crdstore.Get[v1alpha1.VarroaRole](ctx, r.store, role, ""); apierrors.IsNotFound(err) {
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.TeamReasonInvalidRoleRef,
			Message: fmt.Sprintf("roleRef %q does not exist", role),
		})
		if err := r.patchStatus(ctx, team.Name, status); err != nil {
			r.logger.Error("failed to patch status", "team", team.Name, "error", err)
		}
		return reconcile.Result{}, nil
	} else if err != nil {
		r.logger.Error("failed to get VarroaRole", "role", role, "error", err)
		return reconcile.Result{}, err
	}

	labels := teamChildLabels(team)
	ownerRef := teamOwnerRef(team)

	// --- owned Group (only if members) ---
	groupRef := ""
	if len(team.Spec.Members) > 0 {
		wantName := "team-" + team.Name
		if !r.ensureOwned(ctx, "Group", wantName, team) {
			r.setCondition(status, v1alpha1.TeamCondition{
				Type:    v1alpha1.TeamConditionRBACReady,
				Status:  metav1.ConditionFalse,
				Reason:  v1alpha1.TeamReasonGroupApplyFailed,
				Message: fmt.Sprintf("Group %q exists without Team ownership", wantName),
			})
			r.setCondition(status, v1alpha1.TeamCondition{
				Type:    v1alpha1.TeamConditionReady,
				Status:  metav1.ConditionFalse,
				Reason:  v1alpha1.TeamReasonChildApplyFailed,
				Message: fmt.Sprintf("failed to apply Group %q", wantName),
			})
			if err := r.patchStatus(ctx, team.Name, status); err != nil {
				r.logger.Error("failed to patch status", "team", team.Name, "error", err)
			}
			return reconcile.Result{}, nil
		}
		want := &v1alpha1.Group{
			ObjectMeta: metav1.ObjectMeta{
				Name:            wantName,
				Labels:          labels,
				OwnerReferences: []metav1.OwnerReference{ownerRef},
			},
			Spec: v1alpha1.GroupSpec{
				DisplayName: team.Spec.DisplayName,
				Members:     team.Spec.Members,
			},
		}
		if err := crdstore.Apply[v1alpha1.Group](ctx, r.store, want); err != nil {
			r.logger.Error("failed to apply Group", "group", wantName, "error", err)
			r.setCondition(status, v1alpha1.TeamCondition{
				Type:    v1alpha1.TeamConditionRBACReady,
				Status:  metav1.ConditionFalse,
				Reason:  v1alpha1.TeamReasonGroupApplyFailed,
				Message: fmt.Sprintf("failed to apply Group %q: %v", wantName, err),
			})
			r.setCondition(status, v1alpha1.TeamCondition{
				Type:    v1alpha1.TeamConditionReady,
				Status:  metav1.ConditionFalse,
				Reason:  v1alpha1.TeamReasonChildApplyFailed,
				Message: fmt.Sprintf("failed to apply Group %q", wantName),
			})
			if err := r.patchStatus(ctx, team.Name, status); err != nil {
				r.logger.Error("failed to patch status", "team", team.Name, "error", err)
			}
			return reconcile.Result{}, nil
		}
		groupRef = wantName
	} else {
		// No members: delete a previously-owned Group if one exists.
		delName := "team-" + team.Name
		if r.ensureOwned(ctx, "Group", delName, team) {
			if err := crdstore.Delete[v1alpha1.Group](ctx, r.store, delName, ""); err != nil {
				r.logger.Warn("failed to delete previously-owned Group", "group", delName, "error", err)
			}
		}
	}

	// --- owned binding ---
	subjects := make([]v1alpha1.SubjectRef, 0)
	if groupRef != "" {
		subjects = append(subjects, v1alpha1.SubjectRef{Kind: "Group", Name: groupRef})
	}
	subjects = append(subjects, team.Spec.Subjects...)

	bindingName := "team-" + team.Name
	if !r.ensureOwned(ctx, "VarroaRoleBinding", bindingName, team) {
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionRBACReady,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.TeamReasonBindingApplyFailed,
			Message: fmt.Sprintf("VarroaRoleBinding %q exists without Team ownership", bindingName),
		})
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.TeamReasonChildApplyFailed,
			Message: fmt.Sprintf("failed to apply VarroaRoleBinding %q", bindingName),
		})
		if err := r.patchStatus(ctx, team.Name, status); err != nil {
			r.logger.Error("failed to patch status", "team", team.Name, "error", err)
		}
		return reconcile.Result{}, nil
	}

	wantB := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            bindingName,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: subjects,
			RoleRef:  role,
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Namespaces: team.Spec.Namespaces,
			},
		},
	}

	if err := crdstore.Apply[v1alpha1.VarroaRoleBinding](ctx, r.store, wantB); err != nil {
		r.logger.Error("failed to apply VarroaRoleBinding", "binding", bindingName, "error", err)
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionRBACReady,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.TeamReasonBindingApplyFailed,
			Message: fmt.Sprintf("failed to apply VarroaRoleBinding %q: %v", bindingName, err),
		})
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.TeamReasonChildApplyFailed,
			Message: fmt.Sprintf("failed to apply VarroaRoleBinding %q", bindingName),
		})
		if err := r.patchStatus(ctx, team.Name, status); err != nil {
			r.logger.Error("failed to patch status", "team", team.Name, "error", err)
		}
		return reconcile.Result{}, nil
	}

	status.GroupRef = groupRef
	status.BindingRef = bindingName

	r.setCondition(status, v1alpha1.TeamCondition{
		Type:    v1alpha1.TeamConditionRBACReady,
		Status:  metav1.ConditionTrue,
		Reason:  v1alpha1.TeamReasonMaterialized,
		Message: "Group and VarroaRoleBinding applied",
	})

	// --- namespaces ---
	nsOK := true
	status.NamespaceStates = nil
	for _, ns := range team.Spec.Namespaces {
		st, err := r.reconcileNamespace(ctx, ns, team.Spec.ProvisionNamespaces, labels)
		nsState := v1alpha1.TeamNamespaceState{Name: ns, State: st}
		if err != nil || st == v1alpha1.TeamNamespaceStateMissing || st == v1alpha1.TeamNamespaceStateUnmanaged {
			nsOK = false
		}
		status.NamespaceStates = append(status.NamespaceStates, nsState)
	}

	if nsOK {
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionNamespacesReady,
			Status:  metav1.ConditionTrue,
			Reason:  v1alpha1.TeamReasonNamespacesSatisfied,
			Message: "All namespaces are satisfied",
		})
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionReady,
			Status:  metav1.ConditionTrue,
			Reason:  v1alpha1.TeamReasonReconciled,
			Message: "Team reconciled successfully",
		})
	} else {
		// Determine the specific reason for the NamespacesReady condition.
		nsReason := v1alpha1.TeamReasonNamespaceMissing
		for _, nsSt := range status.NamespaceStates {
			if nsSt.State == v1alpha1.TeamNamespaceStateUnmanaged {
				nsReason = v1alpha1.TeamReasonNamespaceUnmanaged
				break
			}
		}
		// If any had an error, use EnsureFailed
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionNamespacesReady,
			Status:  metav1.ConditionFalse,
			Reason:  nsReason,
			Message: "One or more namespaces are not satisfied",
		})
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.TeamReasonNamespaceUnsatisfied,
			Message: "One or more namespaces are not satisfied",
		})
	}

	if err := r.patchStatus(ctx, team.Name, status); err != nil {
		r.logger.Error("failed to patch status", "team", team.Name, "error", err)
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// setCondition upserts a condition on the team status.
func (r *TeamReconciler) setCondition(status *v1alpha1.TeamStatus, cond v1alpha1.TeamCondition) {
	for i, existing := range status.Conditions {
		if existing.Type == cond.Type {
			if existing.Status == cond.Status && existing.Reason == cond.Reason && existing.Message == cond.Message {
				cond.LastTransitionTime = existing.LastTransitionTime
			}
			status.Conditions[i] = cond
			return
		}
	}
	status.Conditions = append(status.Conditions, cond)
}

// patchStatus patches the status subresource of a Team.
func (r *TeamReconciler) patchStatus(ctx context.Context, name string, status *v1alpha1.TeamStatus) error {
	return crdstore.PatchStatus[v1alpha1.Team](ctx, r.store, name, "", status)
}

// teamOwnerRef returns an OwnerReference for a Team.
func teamOwnerRef(t *v1alpha1.Team) metav1.OwnerReference {
	controller := true
	apiVersion := t.APIVersion
	kind := t.Kind
	if apiVersion == "" {
		apiVersion = v1alpha1.SchemeGroupVersion.String()
	}
	if kind == "" {
		kind = "Team"
	}
	return metav1.OwnerReference{
		APIVersion:         apiVersion,
		Kind:               kind,
		Name:               t.Name,
		UID:                t.UID,
		BlockOwnerDeletion: &controller,
		Controller:         &controller,
	}
}

// defaultRoleRef returns the default role ref when empty.
func defaultRoleRef(s string) string {
	if s == "" {
		return "developer"
	}
	return s
}

// teamChildLabels returns the labels to apply to owned child resources.
func teamChildLabels(t *v1alpha1.Team) map[string]string {
	return map[string]string{
		v1alpha1.LabelManagedBy: "team",
		v1alpha1.LabelTeamName:  t.Name,
	}
}

// ensureOwned checks whether a resource with the given name is safe to manage
// as a child of the Team. It returns true if the resource does not exist
// (free to create), or if it already has a Team ownership marker.
// Returns false if the resource exists without ownership (hand-authored collision).
func (r *TeamReconciler) ensureOwned(ctx context.Context, kind, name string, team *v1alpha1.Team) bool {
	var existing metav1.Object
	var err error

	switch kind {
	case "Group":
		var g *v1alpha1.Group
		g, err = crdstore.Get[v1alpha1.Group](ctx, r.store, name, "")
		if err == nil {
			existing = g
		}
	case "VarroaRoleBinding":
		var b *v1alpha1.VarroaRoleBinding
		b, err = crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, r.store, name, "")
		if err == nil {
			existing = b
		}
	default:
		return false
	}

	if apierrors.IsNotFound(err) {
		return true // free to create
	}
	if err != nil {
		return false // treat as not safe
	}

	// Check owner reference or label-based ownership.
	for _, or := range existing.GetOwnerReferences() {
		if or.UID == team.UID {
			return true
		}
	}

	lbls := existing.GetLabels()
	if lbls[v1alpha1.LabelManagedBy] == "team" && lbls[v1alpha1.LabelTeamName] == team.Name {
		return true
	}

	return false // hand-authored collision
}

// reconcileNamespace reconciles a single namespace for a Team using the
// tenancy helper.
func (r *TeamReconciler) reconcileNamespace(ctx context.Context, ns string, provision bool, labels map[string]string) (string, error) {
	managedSet := r.managedSet

	if !provision {
		// Precheck path: classify only, never creates.
		state, err := tenancy.Classify(ctx, r.client.(tenancy.NamespaceReader), managedSet, ns)
		if err != nil {
			return "", err
		}
		switch state {
		case tenancy.NamespaceReady:
			return v1alpha1.TeamNamespaceStateManaged, nil
		case tenancy.NamespaceMissing:
			return v1alpha1.TeamNamespaceStateMissing, nil
		case tenancy.NamespaceUnmanaged:
			return v1alpha1.TeamNamespaceStateUnmanaged, nil
		}
		return "", fmt.Errorf("unexpected namespace state: %s", state)
	}

	// Provisioning path: create or merge-patch labels.
	classifier := tenancy.NewClassifier(r.client.(tenancy.NamespaceClient), managedSet)
	result, err := classifier.Ensure(ctx, ns, labels)
	if err != nil {
		return "", err
	}
	// Ensure's postcondition is always Ready or Unmanaged, never Missing (tenancy.go
	// EnsureResult doc) — sync whenever the namespace is usable, covering both the
	// just-Created and the already-Managed-but-still-missing-the-secret cases. Must run
	// before the Created early-return below, or first-creation never syncs.
	if result.State == tenancy.NamespaceReady {
		r.syncImagePullSecrets(ctx, ns)
	}
	if result.Created {
		return v1alpha1.TeamNamespaceStateCreated, nil
	}
	switch result.State {
	case tenancy.NamespaceReady:
		return v1alpha1.TeamNamespaceStateManaged, nil
	case tenancy.NamespaceUnmanaged:
		return v1alpha1.TeamNamespaceStateUnmanaged, nil
	}
	return "", fmt.Errorf("unexpected ensure state: %s", result.State)
}

// Ensure TeamReconciler implements reconcile.Reconciler.
var _ reconcile.Reconciler = (*TeamReconciler)(nil)

// syncImagePullSecrets copies each imagePullSecret from
// ProvisioningDefaults.Spec.ImagePullSecrets into the given tenant namespace.
// Runs on every Team's every 30s tick (self-healing, idempotently converged).
// Failures are warn-logged only and never flip TeamConditionReady to False.
func (r *TeamReconciler) syncImagePullSecrets(ctx context.Context, ns string) {
	defaults, err := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, r.store, provisioningDefaultsName, "")
	if err != nil || defaults == nil {
		return
	}
	for _, name := range defaults.Spec.ImagePullSecrets {
		if err := r.client.CopyImagePullSecret(ctx, r.operatorNamespace, ns, name); err != nil {
			r.logger.Warn("failed to sync image pull secret into tenant namespace",
				"secret", name, "namespace", ns, "error", err)
		}
	}
}
