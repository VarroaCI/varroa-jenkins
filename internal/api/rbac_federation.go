package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
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
	store            crdstore.Backend
	now              func() time.Time
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
// membership KV. The store receives the VarroaRoleBinding status writes. A nil
// store turns the status writes off.
func NewRBACFederationReconciler(roles, bindings, coreJenkinsRoles rbacFederationStore, brood ConfigBrood, store crdstore.Backend, kv *bus.KV, coreCluster string, logger *slog.Logger) *RBACFederationReconciler {
	r := &RBACFederationReconciler{
		roles:            roles,
		bindings:         bindings,
		coreJenkinsRoles: coreJenkinsRoles,
		brood:            brood,
		store:            store,
		now:              time.Now,
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
	// One snapshot feeds both the projection and the status write, so a
	// status never pairs an outcome with a later spec generation.
	sources := r.sourceBindings()
	varroaRoles := r.sourceRoles()
	desiredRoles, desiredBindings, projectorWarnings, outcomes := r.desired(sources, varroaRoles)
	warnings = append(warnings, projectorWarnings...)

	clusters, err := r.clusterLister()
	if err != nil {
		return append(warnings, fmt.Sprintf("list clusters: %v", err))
	}
	owners := newStaleOwnerIndex(sources, outcomes)
	roleOwners := newStaleRoleOwnerIndex(varroaRoles)
	results := make(map[string]*hiveResult)
	for _, cluster := range clusters {
		if cluster.Name == "" || cluster.Name == r.coreCluster {
			continue
		}
		result := newHiveResult()
		results[cluster.Name] = result
		if err := r.reconcileCluster(ctx, cluster.Name, desiredRoles, desiredBindings, owners, roleOwners, &warnings, result); err != nil {
			warnings = append(warnings, fmt.Sprintf("cluster %s: %v", cluster.Name, err))
			result.listFault = &federationFault{reason: reasonHiveUnavailable, message: err.Error()}
			continue
		}
	}
	r.writeStatuses(ctx, sources, outcomes, results, &warnings)
	r.writeRoleStatuses(ctx, varroaRoles, results, &warnings)
	sort.Strings(warnings)
	return warnings
}

// sourceBindings snapshots the core bindings the informer holds. An informer
// update replaces the stored pointer, so the snapshot objects stay fixed.
func (r *RBACFederationReconciler) sourceBindings() []*v1alpha1.VarroaRoleBinding {
	var bindings []*v1alpha1.VarroaRoleBinding
	for _, obj := range r.bindings.List() {
		if binding, ok := obj.(*v1alpha1.VarroaRoleBinding); ok {
			bindings = append(bindings, binding)
		}
	}
	return bindings
}

// sourceRoles snapshots the core VarroaRoles the informer holds, keyed by
// name. The projection, the stale-role owner index, and the role status write
// read this one snapshot, so a role that the informer adds or removes during
// the pass does not give the three steps different answers.
func (r *RBACFederationReconciler) sourceRoles() map[string]*v1alpha1.VarroaRole {
	roles := make(map[string]*v1alpha1.VarroaRole)
	for _, obj := range r.roles.List() {
		if role, ok := obj.(*v1alpha1.VarroaRole); ok && role.Name != "" {
			roles[role.Name] = role
		}
	}
	return roles
}

func (r *RBACFederationReconciler) desired(bindings []*v1alpha1.VarroaRoleBinding, varroaRoles map[string]*v1alpha1.VarroaRole) ([]*v1alpha1.JenkinsRole, []*v1alpha1.JenkinsRoleBinding, []string, map[string]rbac.BindingOutcome) {
	return rbac.DesiredFederatedCRs(bindings, func(name string) (*v1alpha1.VarroaRole, bool) {
		role, ok := varroaRoles[name]
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

// reconcileCluster converges one hive. Only a list failure stops the pass for
// the cluster. A failed write on one object becomes a warning, so the writes
// for every other object and the garbage-collect loops still run. The
// warning text must stay the same across passes: reconcileAndLog uses it to
// log a repeated failure once. Each fault on a desired child is also recorded
// in result, keyed by the child name, for the binding status write. A failed
// delete of a stale binding child is recorded under each owner that owners
// finds for it. A failed delete of a stale role child is recorded under each
// VarroaRole that roleOwners finds for it.
func (r *RBACFederationReconciler) reconcileCluster(ctx context.Context, cluster string, desiredRoles []*v1alpha1.JenkinsRole, desiredBindings []*v1alpha1.JenkinsRoleBinding, owners staleOwnerIndex, roleOwners staleRoleOwnerIndex, warnings *[]string, result *hiveResult) error {
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
			result.roleFault(desired.Name, reasonJenkinsRoleCollision, roleCollisionMessage(actual))
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
			if err := r.createRole(ctx, cluster, desired); err != nil && !r.isConvergentCreate(ctx, cluster, desired.Name, true, err, warnings, result) {
				objectWarning(warnings, cluster, "create JenkinsRole", desired.Name, err)
				result.roleFault(desired.Name, reasonJenkinsRoleWriteFailed, err.Error())
			}
			continue
		}
		if roleDrifted(actual, desired) {
			updated := desired.DeepCopy()
			updated.ResourceVersion = actual.ResourceVersion
			if err := r.updateRole(ctx, cluster, updated); err != nil && !isConflict(err) {
				objectWarning(warnings, cluster, "update JenkinsRole", desired.Name, err)
				result.roleFault(desired.Name, reasonJenkinsRoleWriteFailed, err.Error())
			}
		}
	}
	for name, actual := range actualRoles {
		if !isFederated(actual.Labels) || roleDesired(name, desiredRoles) {
			continue
		}
		if err := r.brood.DeleteJenkinsRole(ctx, cluster, name); err != nil && !isNotFound(err) {
			staleOwners := roleOwners.ownersOf(actual)
			if len(staleOwners) == 0 {
				objectWarning(warnings, cluster, "delete orphaned JenkinsRole", name, err)
				continue
			}
			objectWarning(warnings, cluster, "delete JenkinsRole", name, err)
			for _, owner := range staleOwners {
				result.staleRoleFault(owner, name, err.Error())
			}
		}
	}

	for _, desired := range desiredBindings {
		actual, exists := actualBindings[desired.Name]
		if exists && !isFederated(actual.Labels) {
			*warnings = append(*warnings, fmt.Sprintf("cluster %s: skip unlabeled JenkinsRoleBinding collision %s", cluster, desired.Name))
			result.bindingFault(desired.Name, reasonJenkinsRoleBindingCollision, bindingCollisionMessage(desired.Name))
			continue
		}
		if !exists {
			if err := r.createBinding(ctx, cluster, desired); err != nil && !r.isConvergentCreate(ctx, cluster, desired.Name, false, err, warnings, result) {
				objectWarning(warnings, cluster, "create JenkinsRoleBinding", desired.Name, err)
				result.bindingFault(desired.Name, reasonJenkinsRoleBindingWriteFailed, err.Error())
			}
			continue
		}
		if bindingDrifted(actual, desired) {
			updated := desired.DeepCopy()
			updated.ResourceVersion = actual.ResourceVersion
			if err := r.updateBinding(ctx, cluster, updated); err != nil && !isConflict(err) {
				objectWarning(warnings, cluster, "update JenkinsRoleBinding", desired.Name, err)
				result.bindingFault(desired.Name, reasonJenkinsRoleBindingWriteFailed, err.Error())
			}
		}
	}
	for name, actual := range actualBindings {
		if !isFederated(actual.Labels) || bindingDesired(name, desiredBindings) {
			continue
		}
		if err := r.brood.DeleteJenkinsRoleBinding(ctx, cluster, name); err != nil && !isNotFound(err) {
			staleOwners := owners.ownersOf(actual)
			if len(staleOwners) == 0 {
				objectWarning(warnings, cluster, "delete orphaned JenkinsRoleBinding", name, err)
				continue
			}
			objectWarning(warnings, cluster, "delete JenkinsRoleBinding", name, err)
			for _, owner := range staleOwners {
				result.staleFault(owner, name, err.Error())
			}
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

func (r *RBACFederationReconciler) isConvergentCreate(ctx context.Context, cluster, name string, role bool, err error, warnings *[]string, result *hiveResult) bool {
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
		result.roleFault(name, reasonJenkinsRoleCollision, roleCollisionMessage(actualRole))
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
		result.roleFault(name, reasonJenkinsRoleCollision, fmt.Sprintf("JenkinsRole %s exists and is not federated", name))
	} else {
		result.bindingFault(name, reasonJenkinsRoleBindingCollision, bindingCollisionMessage(name))
	}
	*warnings = append(*warnings, fmt.Sprintf("cluster %s: skip unlabeled %s collision %s", cluster, kind, name))
	return true
}

func objectWarning(warnings *[]string, cluster, action, name string, err error) {
	*warnings = append(*warnings, fmt.Sprintf("cluster %s: %s %s: %v", cluster, action, name, err))
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

// roleDrifted compares only the source annotation key, so an annotation that
// another writer adds does not cause an update on each pass.
func roleDrifted(actual, desired *v1alpha1.JenkinsRole) bool {
	return !reflect.DeepEqual(actual.Labels, desired.Labels) ||
		actual.Annotations[rbac.AnnotationFederatedRoles] != desired.Annotations[rbac.AnnotationFederatedRoles] ||
		!reflect.DeepEqual(actual.Spec, desired.Spec)
}

// bindingDrifted compares only the owner annotation key, so an annotation that
// another writer adds does not cause an update on each pass.
func bindingDrifted(actual, desired *v1alpha1.JenkinsRoleBinding) bool {
	return !reflect.DeepEqual(actual.Labels, desired.Labels) ||
		actual.Annotations[rbac.AnnotationFederatedBindings] != desired.Annotations[rbac.AnnotationFederatedBindings] ||
		!reflect.DeepEqual(actual.Spec, desired.Spec)
}

// staleOwnerIndex finds the core bindings that own a stale federated
// JenkinsRoleBinding. It holds only the bindings of the pass snapshot, so a
// deleted core binding owns nothing. The role-label fallback holds only
// projected bindings: a skipped binding with no annotation link has no
// evidence that it ever owned a child.
type staleOwnerIndex struct {
	names     map[string]bool
	byRoleRef map[string][]string
}

func newStaleOwnerIndex(sources []*v1alpha1.VarroaRoleBinding, outcomes map[string]rbac.BindingOutcome) staleOwnerIndex {
	idx := staleOwnerIndex{names: map[string]bool{}, byRoleRef: map[string][]string{}}
	for _, binding := range sources {
		if binding == nil || binding.Name == "" {
			continue
		}
		idx.names[binding.Name] = true
		if outcomes[binding.Name].SkipReason != "" {
			continue
		}
		idx.byRoleRef[binding.Spec.RoleRef] = append(idx.byRoleRef[binding.Spec.RoleRef], binding.Name)
	}
	return idx
}

// ownersOf reads the owner annotation. A child with no annotation names no
// owner, so every projected core binding on the role of its label owns it: a report on
// a binding that never owned the child is better than a missed revocation.
func (idx staleOwnerIndex) ownersOf(child *v1alpha1.JenkinsRoleBinding) []string {
	listed, annotated := child.Annotations[rbac.AnnotationFederatedBindings]
	if !annotated {
		return idx.byRoleRef[child.Labels[rbac.LabelFederatedFrom]]
	}
	var owners []string
	for _, name := range strings.Split(listed, ",") {
		if idx.names[name] {
			owners = append(owners, name)
		}
	}
	return owners
}

// staleRoleOwnerIndex finds the core VarroaRoles that own a stale federated
// JenkinsRole. It holds only the VarroaRoles of the pass snapshot, so a
// deleted VarroaRole owns nothing and its stale copy gets only a warning.
type staleRoleOwnerIndex struct {
	names map[string]bool
}

func newStaleRoleOwnerIndex(varroaRoles map[string]*v1alpha1.VarroaRole) staleRoleOwnerIndex {
	idx := staleRoleOwnerIndex{names: make(map[string]bool, len(varroaRoles))}
	for name := range varroaRoles {
		idx.names[name] = true
	}
	return idx
}

// ownersOf reads the source annotation. A child with no annotation falls back
// to the VarroaRole of its label.
func (idx staleRoleOwnerIndex) ownersOf(child *v1alpha1.JenkinsRole) []string {
	listed, annotated := child.Annotations[rbac.AnnotationFederatedRoles]
	if !annotated {
		if label := child.Labels[rbac.LabelFederatedFrom]; idx.names[label] {
			return []string{label}
		}
		return nil
	}
	var owners []string
	for _, name := range strings.Split(listed, ",") {
		if idx.names[name] {
			owners = append(owners, name)
		}
	}
	return owners
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
