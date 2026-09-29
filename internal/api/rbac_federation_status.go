package api

import (
	"context"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// Reason codes of a hive-side federation outcome. The projector owns the
// skip reason codes (rbac.Skip*).
const (
	reasonFederated                     = "Federated"
	reasonHiveUnavailable               = "HiveUnavailable"
	reasonJenkinsRoleCollision          = "JenkinsRoleCollision"
	reasonJenkinsRoleWriteFailed        = "JenkinsRoleWriteFailed"
	reasonJenkinsRoleBindingCollision   = "JenkinsRoleBindingCollision"
	reasonJenkinsRoleBindingWriteFailed = "JenkinsRoleBindingWriteFailed"
	reasonStaleBindingDeleteFailed      = "StaleJenkinsRoleBindingDeleteFailed"
)

type federationFault struct {
	reason  string
	message string
}

// hiveResult holds the faults of one pass on one hive. A list failure stops
// the pass for the hive, so listFault applies to every binding. The other
// faults key on the desired child name. The first fault for a name wins, so
// the result does not depend on the order of later writes. staleFaults keys
// on the source binding name, because a stale child is no longer desired.
type hiveResult struct {
	listFault     *federationFault
	roleFaults    map[string]federationFault
	bindingFaults map[string]federationFault
	staleFaults   map[string]staleFault
}

// staleFault is a failed delete of a stale binding child.
type staleFault struct {
	child string
	err   string
}

func newHiveResult() *hiveResult {
	return &hiveResult{roleFaults: map[string]federationFault{}, bindingFaults: map[string]federationFault{}, staleFaults: map[string]staleFault{}}
}

// staleFault keeps the lexically first child for each owner. The pass visits
// the stale children in map order, and the message must stay the same across
// passes.
func (h *hiveResult) staleFault(owner, child, err string) {
	if old, ok := h.staleFaults[owner]; ok && old.child <= child {
		return
	}
	h.staleFaults[owner] = staleFault{child: child, err: err}
}

func (h *hiveResult) roleFault(name, reason, message string) {
	if _, ok := h.roleFaults[name]; !ok {
		h.roleFaults[name] = federationFault{reason: reason, message: message}
	}
}

func (h *hiveResult) bindingFault(name, reason, message string) {
	if _, ok := h.bindingFaults[name]; !ok {
		h.bindingFaults[name] = federationFault{reason: reason, message: message}
	}
}

func roleCollisionMessage(actual *v1alpha1.JenkinsRole) string {
	if actual.Labels != nil && actual.Labels[v1alpha1.LabelBuiltin] == "true" {
		return fmt.Sprintf("JenkinsRole %s is a builtin role with a different spec", actual.Name)
	}
	return fmt.Sprintf("JenkinsRole %s exists and is not federated", actual.Name)
}

func bindingCollisionMessage(name string) string {
	return fmt.Sprintf("JenkinsRoleBinding %s exists and is not federated", name)
}

// federationEntry gives the outcome of one binding on one hive. A projector skip
// applies to every hive, but a stale fault on the hive comes first: the stale
// child still grants its subjects there. A list fault comes before a role
// fault, a role fault before a binding fault, and a binding fault before a
// stale fault.
func federationEntry(cluster, binding string, outcome rbac.BindingOutcome, result *hiveResult) v1alpha1.BindingFederationStatus {
	entry := v1alpha1.BindingFederationStatus{Cluster: cluster}
	fault := func(f federationFault) v1alpha1.BindingFederationStatus {
		entry.State, entry.Reason, entry.Message = v1alpha1.BindingFailed, f.reason, f.message
		return entry
	}
	stale, hasStale := result.staleFaults[binding]
	if outcome.SkipReason != "" {
		if hasStale {
			return fault(federationFault{reason: reasonStaleBindingDeleteFailed, message: fmt.Sprintf("delete of earlier copy %s failed: %s", stale.child, stale.err)})
		}
		entry.State, entry.Reason, entry.Message = v1alpha1.BindingSkipped, outcome.SkipReason, outcome.SkipMessage
		return entry
	}
	if result.listFault != nil {
		return fault(*result.listFault)
	}
	if f, ok := result.roleFaults[outcome.RoleName]; ok {
		return fault(f)
	}
	if f, ok := result.bindingFaults[outcome.BindingName]; ok {
		return fault(f)
	}
	if hasStale {
		return fault(federationFault{reason: reasonStaleBindingDeleteFailed, message: fmt.Sprintf("JenkinsRoleBinding %s applied; delete of earlier copy %s failed: %s", outcome.BindingName, stale.child, stale.err)})
	}
	entry.State, entry.Reason, entry.Message = v1alpha1.BindingFederated, reasonFederated, "JenkinsRoleBinding "+outcome.BindingName
	return entry
}

// writeStatuses patches the status of every binding in the snapshot the
// projection read.
// It writes only a status that differs from the stored one: every status
// write is an informer update that enqueues another pass, so an unconditional
// write never settles.
func (r *RBACFederationReconciler) writeStatuses(ctx context.Context, sources []*v1alpha1.VarroaRoleBinding, outcomes map[string]rbac.BindingOutcome, results map[string]*hiveResult, warnings *[]string) {
	if r.store == nil {
		return
	}
	clusters := make([]string, 0, len(results))
	for cluster := range results {
		clusters = append(clusters, cluster)
	}
	sort.Strings(clusters)
	now := metav1.NewTime(r.now().UTC().Truncate(time.Second))

	for _, binding := range sources {
		if binding == nil || binding.Name == "" {
			continue
		}
		outcome, ok := outcomes[binding.Name]
		if !ok {
			continue
		}
		build := func(stored v1alpha1.VarroaRoleBindingStatus) v1alpha1.VarroaRoleBindingStatus {
			return buildBindingStatus(binding.Name, binding.Generation, clusters, outcome, results, stored, now)
		}
		next := build(binding.Status)
		if statusEqual(binding.Status, next) {
			continue
		}
		// The informer copy can lag an earlier write of this pass loop. Read
		// the stored status before a write, so a lagged copy neither resets
		// lastTransitionTime nor repeats an unchanged status.
		live, err := crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, r.store, binding.Name, "")
		if err != nil {
			if !apierrors.IsNotFound(err) {
				*warnings = append(*warnings, fmt.Sprintf("read status varroarolebinding %s: %v", binding.Name, err))
			}
			continue
		}
		next = build(live.Status)
		if statusEqual(live.Status, next) {
			continue
		}
		if err := crdstore.PatchStatus[v1alpha1.VarroaRoleBinding](ctx, r.store, binding.Name, "", next); err != nil && !apierrors.IsNotFound(err) {
			*warnings = append(*warnings, fmt.Sprintf("status varroarolebinding %s: %v", binding.Name, err))
		}
	}
}

// buildBindingStatus builds the status of one binding for the visited hives.
// An entry keeps the stored lastTransitionTime while its state, reason, and
// message stay the same.
func buildBindingStatus(name string, generation int64, clusters []string, outcome rbac.BindingOutcome, results map[string]*hiveResult, stored v1alpha1.VarroaRoleBindingStatus, now metav1.Time) v1alpha1.VarroaRoleBindingStatus {
	previous := make(map[string]v1alpha1.BindingFederationStatus, len(stored.Federation))
	for _, e := range stored.Federation {
		previous[e.Cluster] = e
	}
	next := v1alpha1.VarroaRoleBindingStatus{ObservedGeneration: generation}
	for _, cluster := range clusters {
		entry := federationEntry(cluster, name, outcome, results[cluster])
		entry.LastTransitionTime = now
		if old, ok := previous[cluster]; ok && old.State == entry.State && old.Reason == entry.Reason && old.Message == entry.Message {
			entry.LastTransitionTime = old.LastTransitionTime
		}
		next.Federation = append(next.Federation, entry)
	}
	return next
}

// statusEqual compares by cluster, so the stored entry order does not matter.
func statusEqual(a, b v1alpha1.VarroaRoleBindingStatus) bool {
	if a.ObservedGeneration != b.ObservedGeneration || len(a.Federation) != len(b.Federation) {
		return false
	}
	byCluster := make(map[string]v1alpha1.BindingFederationStatus, len(b.Federation))
	for _, e := range b.Federation {
		byCluster[e.Cluster] = e
	}
	for _, x := range a.Federation {
		y, ok := byCluster[x.Cluster]
		if !ok || x.State != y.State || x.Reason != y.Reason || x.Message != y.Message || !x.LastTransitionTime.Equal(&y.LastTransitionTime) {
			return false
		}
	}
	return true
}
