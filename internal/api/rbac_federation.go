package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

const defaultRbacFederationResync = 60 * time.Second

type rbacFederationStore interface {
	List() []interface{}
	GetByKey(key string) (interface{}, bool, error)
}

// RBACFederationReconciler projects core VarroaRole bindings into JenkinsRole/
// JenkinsRoleBinding CRs on every non-core member cluster, converging each cluster
// on informer events and a periodic resync (leader-gated by the caller).
type RBACFederationReconciler struct {
	roles            rbacFederationStore
	bindings         rbacFederationStore
	coreJenkinsRoles rbacFederationStore
	brood            ConfigBrood
	kv               *bus.KV
	coreCluster      string
	logger           *slog.Logger
	clusterLister    func() ([]bus.ClusterInfo, error)
	signal           chan struct{}
	debounceInterval time.Duration
	resyncInterval   time.Duration
	warningsMu       sync.Mutex
	lastWarnings     map[string]struct{}
}

// NewRBACFederationReconciler builds a reconciler over the core's VarroaRole,
// VarroaRoleBinding, and JenkinsRole listers plus the config brood and cluster
// membership KV.
func NewRBACFederationReconciler(roles, bindings, coreJenkinsRoles rbacFederationStore, brood ConfigBrood, kv *bus.KV, coreCluster string, logger *slog.Logger) *RBACFederationReconciler {
	r := &RBACFederationReconciler{
		roles:            roles,
		bindings:         bindings,
		coreJenkinsRoles: coreJenkinsRoles,
		brood:            brood,
		kv:               kv,
		coreCluster:      coreCluster,
		logger:           logger.With("component", "rbac_federation"),
		signal:           make(chan struct{}, 1),
		debounceInterval: 250 * time.Millisecond,
		resyncInterval:   defaultRbacFederationResync,
		lastWarnings:     make(map[string]struct{}),
	}
	r.clusterLister = func() ([]bus.ClusterInfo, error) { return bus.ListClusters(kv) }
	return r
}

// Enqueue signals a debounced reconcile (non-blocking, coalesced).
func (r *RBACFederationReconciler) Enqueue() {
	select {
	case r.signal <- struct{}{}:
	default:
	}
}

// Run drives the reconcile loop until ctx is cancelled: a debounced enqueue
// signal or the resync ticker each trigger a full reconcile.
func (r *RBACFederationReconciler) Run(ctx context.Context) {
	r.Enqueue()
	ticker := time.NewTicker(r.resyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.signal:
			timer := time.NewTimer(r.debounceInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			r.reconcileAndLog(ctx)
		case <-ticker.C:
			r.reconcileAndLog(ctx)
		}
	}
}

// Reconcile runs a single convergence pass and returns any per-cluster warnings
// (exported for tests and manual triggers).
func (r *RBACFederationReconciler) Reconcile(ctx context.Context) []string {
	return r.reconcile(ctx)
}

func (r *RBACFederationReconciler) reconcileAndLog(ctx context.Context) {
	warnings := r.reconcile(ctx)
	r.warningsMu.Lock()
	previous := r.lastWarnings
	current := make(map[string]struct{}, len(warnings))
	for _, warning := range warnings {
		current[warning] = struct{}{}
	}
	r.lastWarnings = current
	r.warningsMu.Unlock()
	for _, warning := range warnings {
		if _, alreadyLogged := previous[warning]; alreadyLogged {
			continue
		}
		r.logger.Warn("rbac federation warning", "warning", warning)
	}
}

func (r *RBACFederationReconciler) reconcile(ctx context.Context) []string {
	var warnings []string
	desiredRoles, desiredBindings, projectorWarnings := r.desired()
	warnings = append(warnings, projectorWarnings...)

	clusters, err := r.clusterLister()
	if err != nil {
		return append(warnings, fmt.Sprintf("list clusters: %v", err))
	}
	for _, cluster := range clusters {
		if cluster.Name == "" || cluster.Name == r.coreCluster {
			continue
		}
		if err := r.reconcileCluster(ctx, cluster.Name, desiredRoles, desiredBindings, &warnings); err != nil {
			warnings = append(warnings, fmt.Sprintf("cluster %s: %v", cluster.Name, err))
			continue
		}
	}
	sort.Strings(warnings)
	return warnings
}

func (r *RBACFederationReconciler) desired() ([]*v1alpha1.JenkinsRole, []*v1alpha1.JenkinsRoleBinding, []string) {
	var bindings []*v1alpha1.VarroaRoleBinding
	for _, obj := range r.bindings.List() {
		if binding, ok := obj.(*v1alpha1.VarroaRoleBinding); ok {
			bindings = append(bindings, binding)
		}
	}
	return rbac.DesiredFederatedCRs(bindings, func(name string) (*v1alpha1.VarroaRole, bool) {
		obj, ok, err := r.roles.GetByKey(name)
		if err != nil || !ok {
			return nil, false
		}
		role, ok := obj.(*v1alpha1.VarroaRole)
		return role, ok
	}, func(name string) (*v1alpha1.JenkinsRole, bool) {
		obj, ok, err := r.coreJenkinsRoles.GetByKey(name)
		if err != nil || !ok {
			return nil, false
		}
		role, ok := obj.(*v1alpha1.JenkinsRole)
		return role, ok
	})
}

func (r *RBACFederationReconciler) reconcileCluster(ctx context.Context, cluster string, desiredRoles []*v1alpha1.JenkinsRole, desiredBindings []*v1alpha1.JenkinsRoleBinding, warnings *[]string) error {
	actualRoles, err := r.listRoles(ctx, cluster)
	if err != nil {
		return fmt.Errorf("list JenkinsRoles: %w", err)
	}
	actualBindings, err := r.listBindings(ctx, cluster)
	if err != nil {
		return fmt.Errorf("list JenkinsRoleBindings: %w", err)
	}

	for _, desired := range desiredRoles {
		actual, exists := actualRoles[desired.Name]
		if exists && !isFederated(actual.Labels) {
			if builtinRoleMatches(actual) {
				continue
			}
			if actual.Labels != nil && actual.Labels[v1alpha1.LabelBuiltin] == "true" {
				if _, ok := builtinRoleDefinition(actual.Name); ok {
					*warnings = append(*warnings, fmt.Sprintf("cluster %s: builtin spec drift JenkinsRole collision %s", cluster, desired.Name))
				} else {
					*warnings = append(*warnings, fmt.Sprintf("cluster %s: builtin-labeled JenkinsRole collision %s", cluster, desired.Name))
				}
			} else {
				*warnings = append(*warnings, fmt.Sprintf("cluster %s: skip unlabeled JenkinsRole collision %s", cluster, desired.Name))
			}
			continue
		}
		if !exists {
			if err := r.createRole(ctx, cluster, desired); err != nil && !r.isConvergentCreate(ctx, cluster, desired.Name, true, err, warnings) {
				return err
			}
			continue
		}
		if roleDrifted(actual, desired) {
			updated := desired.DeepCopy()
			updated.ResourceVersion = actual.ResourceVersion
			if err := r.updateRole(ctx, cluster, updated); err != nil && !isConflict(err) {
				return err
			}
		}
	}
	for name, actual := range actualRoles {
		if !isFederated(actual.Labels) || roleDesired(name, desiredRoles) {
			continue
		}
		if err := r.brood.DeleteJenkinsRole(ctx, cluster, name); err != nil && !isNotFound(err) {
			return err
		}
	}

	for _, desired := range desiredBindings {
		actual, exists := actualBindings[desired.Name]
		if exists && !isFederated(actual.Labels) {
			*warnings = append(*warnings, fmt.Sprintf("cluster %s: skip unlabeled JenkinsRoleBinding collision %s", cluster, desired.Name))
			continue
		}
		if !exists {
			if err := r.createBinding(ctx, cluster, desired); err != nil && !r.isConvergentCreate(ctx, cluster, desired.Name, false, err, warnings) {
				return err
			}
			continue
		}
		if bindingDrifted(actual, desired) {
			updated := desired.DeepCopy()
			updated.ResourceVersion = actual.ResourceVersion
			if err := r.updateBinding(ctx, cluster, updated); err != nil && !isConflict(err) {
				return err
			}
		}
	}
	for name, actual := range actualBindings {
		if !isFederated(actual.Labels) || bindingDesired(name, desiredBindings) {
			continue
		}
		if err := r.brood.DeleteJenkinsRoleBinding(ctx, cluster, name); err != nil && !isNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *RBACFederationReconciler) listRoles(ctx context.Context, cluster string) (map[string]*v1alpha1.JenkinsRole, error) {
	raw, err := r.brood.ListJenkinsRoles(ctx, cluster)
	if err != nil {
		return nil, err
	}
	roles := make(map[string]*v1alpha1.JenkinsRole, len(raw))
	for _, item := range raw {
		var role v1alpha1.JenkinsRole
		if err := json.Unmarshal(item, &role); err != nil {
			return nil, err
		}
		roles[role.Name] = &role
	}
	return roles, nil
}

func (r *RBACFederationReconciler) listBindings(ctx context.Context, cluster string) (map[string]*v1alpha1.JenkinsRoleBinding, error) {
	raw, err := r.brood.ListJenkinsRoleBindings(ctx, cluster)
	if err != nil {
		return nil, err
	}
	bindings := make(map[string]*v1alpha1.JenkinsRoleBinding, len(raw))
	for _, item := range raw {
		var binding v1alpha1.JenkinsRoleBinding
		if err := json.Unmarshal(item, &binding); err != nil {
			return nil, err
		}
		bindings[binding.Name] = &binding
	}
	return bindings, nil
}

func (r *RBACFederationReconciler) createRole(ctx context.Context, cluster string, role *v1alpha1.JenkinsRole) error {
	raw, err := json.Marshal(role)
	if err != nil {
		return err
	}
	_, err = r.brood.CreateJenkinsRole(ctx, cluster, role.Name, raw)
	return err
}

func (r *RBACFederationReconciler) updateRole(ctx context.Context, cluster string, role *v1alpha1.JenkinsRole) error {
	raw, err := json.Marshal(role)
	if err != nil {
		return err
	}
	_, err = r.brood.UpdateJenkinsRole(ctx, cluster, role.Name, raw)
	return err
}

func (r *RBACFederationReconciler) createBinding(ctx context.Context, cluster string, binding *v1alpha1.JenkinsRoleBinding) error {
	raw, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	_, err = r.brood.CreateJenkinsRoleBinding(ctx, cluster, binding.Name, raw)
	return err
}

func (r *RBACFederationReconciler) updateBinding(ctx context.Context, cluster string, binding *v1alpha1.JenkinsRoleBinding) error {
	raw, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	_, err = r.brood.UpdateJenkinsRoleBinding(ctx, cluster, binding.Name, raw)
	return err
}

func (r *RBACFederationReconciler) isConvergentCreate(ctx context.Context, cluster, name string, role bool, err error, warnings *[]string) bool {
	if !isConflict(err) {
		return false
	}
	var labels map[string]string
	var actualRole *v1alpha1.JenkinsRole
	var getErr error
	if role {
		var raw json.RawMessage
		raw, getErr = r.brood.GetJenkinsRole(ctx, cluster, name)
		if getErr == nil {
			var obj v1alpha1.JenkinsRole
			getErr = json.Unmarshal(raw, &obj)
			labels = obj.Labels
			actualRole = &obj
		}
	} else {
		var raw json.RawMessage
		raw, getErr = r.brood.GetJenkinsRoleBinding(ctx, cluster, name)
		if getErr == nil {
			var obj v1alpha1.JenkinsRoleBinding
			getErr = json.Unmarshal(raw, &obj)
			labels = obj.Labels
		}
	}
	if getErr != nil {
		return isNotFound(getErr)
	}
	if isFederated(labels) {
		return true
	}
	// Mirror the collision handling of the list-based path so a create that
	// loses the race against the agent operator's builtin reconciler stays
	// silent (or warns identically) rather than always reporting "unlabeled".
	if role && actualRole != nil {
		if builtinRoleMatches(actualRole) {
			return true
		}
		if labels[v1alpha1.LabelBuiltin] == "true" {
			if _, ok := builtinRoleDefinition(actualRole.Name); ok {
				*warnings = append(*warnings, fmt.Sprintf("cluster %s: builtin spec drift JenkinsRole collision %s", cluster, name))
			} else {
				*warnings = append(*warnings, fmt.Sprintf("cluster %s: builtin-labeled JenkinsRole collision %s", cluster, name))
			}
			return true
		}
	}
	kind := "JenkinsRoleBinding"
	if role {
		kind = "JenkinsRole"
	}
	*warnings = append(*warnings, fmt.Sprintf("cluster %s: skip unlabeled %s collision %s", cluster, kind, name))
	return true
}

func isFederated(labels map[string]string) bool {
	return labels != nil && labels[rbac.LabelFederatedFrom] != ""
}

func builtinRoleDefinition(name string) (*v1alpha1.JenkinsRole, bool) {
	for _, role := range rbac.BuiltinJenkinsRoles() {
		if role.Name == name {
			return role, true
		}
	}
	return nil, false
}

func builtinRoleMatches(actual *v1alpha1.JenkinsRole) bool {
	if actual.Labels == nil || actual.Labels[v1alpha1.LabelBuiltin] != "true" {
		return false
	}
	want, ok := builtinRoleDefinition(actual.Name)
	return ok && reflect.DeepEqual(actual.Spec, want.Spec)
}

func roleDrifted(actual, desired *v1alpha1.JenkinsRole) bool {
	return !reflect.DeepEqual(actual.Labels, desired.Labels) || !reflect.DeepEqual(actual.Spec, desired.Spec)
}

func bindingDrifted(actual, desired *v1alpha1.JenkinsRoleBinding) bool {
	return !reflect.DeepEqual(actual.Labels, desired.Labels) || !reflect.DeepEqual(actual.Spec, desired.Spec)
}

func roleDesired(name string, desired []*v1alpha1.JenkinsRole) bool {
	for _, role := range desired {
		if role.Name == name {
			return true
		}
	}
	return false
}

func bindingDesired(name string, desired []*v1alpha1.JenkinsRoleBinding) bool {
	for _, binding := range desired {
		if binding.Name == name {
			return true
		}
	}
	return false
}

func isConflict(err error) bool {
	var fe *BroodError
	return errors.As(err, &fe) && fe.Code == bus.CodeConflict
}

func isNotFound(err error) bool {
	var fe *BroodError
	return errors.As(err, &fe) && fe.Code == bus.CodeNotFound
}
