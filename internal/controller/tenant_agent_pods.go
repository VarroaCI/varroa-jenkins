package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

// The kubernetes plugin stamps both keys on every agent pod it builds,
// Pending pods included. The label value derives from the cloud's
// jenkinsUrl, which names the slot's agent Service, so it stays the same
// across a cell roll, a slot move, and a promotion. The annotation holds
// the epoch milliseconds of the last refresh by the JVM that holds the pod.
const (
	agentPodControllerLabel   = "kubernetes.jenkins.io/controller"
	agentPodLastRefreshAnnot  = "kubernetes.jenkins.io/last-refresh"
	agentPodDefaultLabelKey   = "jenkins"
	agentPodDefaultLabelValue = "slave"
)

// tenantAgentRunningReapAfter is the refresh age past which the operator
// deletes a Running tenant agent pod. It is five minutes more than
// tenantAgentGCTimeoutSeconds, so the plugin's own garbage collection
// acts first while a live JVM holds the slot's cloud. A Running pod may
// hold a node that a resume adopts later, so it must outlive
// TenantAgentAdoptBound.
const tenantAgentRunningReapAfter = tenantAgentGCTimeoutSeconds*time.Second + 5*time.Minute

// tenantAgentPendingReapAfter is the refresh age past which the operator
// deletes a tenant agent pod that is not Running. The plugin accepts a
// task on an agent only after every container of its pod is ready, so no
// node body ran on such a pod and no resume adopts it. A live JVM
// refreshes its pods once a minute, so three minutes is three missed
// refreshes. The rule also acts while the slot's new cell pod is still
// Pending, when no JVM holds the cloud.
const tenantAgentPendingReapAfter = 3 * time.Minute

// tenantAgentReapInterval is the least time between two steady-state pod
// lists for one tenant. A teardown tick ignores it, because the finalizer
// release needs a fresh count.
const tenantAgentReapInterval = time.Minute

// tenantAgentPodReaper is implemented by ResourceClient backends that can
// list and delete tenant agent pods. It is checked with a type assertion,
// the same as tenantServiceWriter, so the ResourceClient interface and its
// fakes outside this package do not grow.
type tenantAgentPodReaper interface {
	ListTenantAgentPods(ctx context.Context, namespace string) ([]corev1.Pod, error)
	DeletePodWithUID(ctx context.Context, namespace, name string, uid types.UID) error
}

// orphanTenantAgentPods answers the pods of one tenant that the operator
// must delete. A pod is a candidate only when it runs as the tenant agent
// ServiceAccount, carries the plugin's controller label, and carries a
// last-refresh annotation that parses. A candidate goes when its last
// refresh is older than its reap age (agentPodReapAfter), or at once while
// the tenant tears down. A pod with no annotation or an unparseable one is
// never deleted: the operator cannot tell how old it is, and another
// controller may own it.
func orphanTenantAgentPods(pods []corev1.Pod, tenant string, now time.Time, tearingDown bool) []corev1.Pod {
	sa := tenantAgentServiceAccountName(tenant)
	var out []corev1.Pod
	for _, pod := range pods {
		if pod.Spec.ServiceAccountName != sa {
			continue
		}
		if _, ok := pod.Labels[agentPodControllerLabel]; !ok {
			continue
		}
		refreshed, ok := agentPodLastRefresh(pod)
		if !ok {
			continue
		}
		if tearingDown || now.Sub(refreshed) > agentPodReapAfter(pod) {
			out = append(out, pod)
		}
	}
	return out
}

// agentPodReapAfter answers the refresh age past which a tenant agent pod
// goes. A Running pod, or one whose phase the kubelet cannot report, may
// hold a node that a resume adopts later, so it gets the long age. Every
// other phase gets the short age.
func agentPodReapAfter(pod corev1.Pod) time.Duration {
	switch pod.Status.Phase {
	case corev1.PodRunning, corev1.PodUnknown:
		return tenantAgentRunningReapAfter
	default:
		return tenantAgentPendingReapAfter
	}
}

// agentPodLastRefresh parses the plugin's last-refresh annotation.
func agentPodLastRefresh(pod corev1.Pod) (time.Time, bool) {
	raw, ok := pod.Annotations[agentPodLastRefreshAnnot]
	if !ok {
		return time.Time{}, false
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
}

// reapTenantAgentPods deletes the tenant agent pods in agentNamespace that
// no live JVM refreshes, or every tenant agent pod while the tenant tears
// down. It answers the count of tenant agent pods that still exist after
// the pass, which counts a pod that this pass deleted and that still
// terminates, and leaves out a pod whose delete answered NotFound. A backend that is not a tenantAgentPodReaper answers zero.
// Each delete carries the listed pod's UID as a precondition, so a pod
// that the plugin created again under the same name is never deleted.
func (r *TenantReconciler) reapTenantAgentPods(ctx context.Context, tenant, agentNamespace string, tearingDown bool) (int, error) {
	if agentNamespace == "" {
		return 0, nil
	}
	reaper, ok := r.client.(tenantAgentPodReaper)
	if !ok {
		return 0, nil
	}
	pods, err := reaper.ListTenantAgentPods(ctx, agentNamespace)
	if err != nil {
		return 0, fmt.Errorf("list the tenant agent pods in namespace %q: %w", agentNamespace, err)
	}
	now := time.Now()
	reason := "stale"
	if tearingDown {
		reason = "teardown"
	}
	orphans := orphanTenantAgentPods(pods, tenant, now, tearingDown)
	gone := 0
	var first error
	for _, pod := range orphans {
		if pod.DeletionTimestamp != nil {
			continue
		}
		if err := reaper.DeletePodWithUID(ctx, agentNamespace, pod.Name, pod.UID); err != nil {
			// A NotFound pod already left the API list, so it no longer
			// counts. A Conflict names a new pod under the listed name,
			// so it still counts: teardown must not release over it.
			if apierrors.IsNotFound(err) {
				gone++
				continue
			}
			if apierrors.IsConflict(err) {
				continue
			}
			if first == nil {
				first = fmt.Errorf("delete the tenant agent pod %q in namespace %q: %w", pod.Name, agentNamespace, err)
			}
			continue
		}
		r.logger.Info("deleted a tenant agent pod", "pod", pod.Name, "namespace", agentNamespace,
			"tenant", tenant, "reason", reason)
	}
	remaining := len(orphanTenantAgentPods(pods, tenant, now, true)) - gone
	return remaining, first
}

// reapTenantAgentPodsDue reports whether the steady-state reaper may list
// the tenant's pods on this tick, and records the list time when it may.
func (r *TenantReconciler) reapTenantAgentPodsDue(tenant string, now time.Time) bool {
	r.reapMu.Lock()
	defer r.reapMu.Unlock()
	if r.lastReap == nil {
		r.lastReap = map[string]time.Time{}
	}
	if last, ok := r.lastReap[tenant]; ok && now.Sub(last) < tenantAgentReapInterval {
		return false
	}
	r.lastReap[tenant] = now
	return true
}

// forgetTenantAgentReap drops the tenant's rate-limit entry after teardown,
// so a Tenant name that is reused later starts with no stale entry.
func (r *TenantReconciler) forgetTenantAgentReap(tenant string) {
	r.reapMu.Lock()
	defer r.reapMu.Unlock()
	delete(r.lastReap, tenant)
}

// ReleaseRunAgentPods deletes the tenant agent pods of one ended run. The
// recorder names the pods by their Jenkins node names, which equal the
// pod names. A named pod goes only when it runs as the tenant's agent
// ServiceAccount and carries the plugin's controller label. Every other
// pod stays, so a name that a different tenant or controller holds is
// never deleted.
func (r *Reconciler) ReleaseRunAgentPods(ctx context.Context, tenant, namespace string, names []string) {
	reaper, ok := r.client.(tenantAgentPodReaper)
	if !ok {
		return
	}
	deleted, err := releaseNamedAgentPods(ctx, reaper, tenant, namespace, names)
	if err != nil {
		r.Logger.Warn("release run agent pods failed", "tenant", tenant, "namespace", namespace, "error", err)
	}
	for range deleted {
		r.Logger.Info("released a run agent pod", "tenant", tenant, "namespace", namespace, "reason", "run-ended")
	}
}

// releaseNamedAgentPods deletes the pods of names that pass the tenant
// test, each with its listed UID as a precondition. It answers the count
// of pods it deleted. NotFound and Conflict are not errors: a pod that
// left the API list, or a pod that a later create replaced under the
// same name, needs no delete.
func releaseNamedAgentPods(ctx context.Context, reaper tenantAgentPodReaper, tenant, namespace string,
	names []string) (int, error) {
	if namespace == "" || len(names) == 0 {
		return 0, nil
	}
	want := make(map[string]struct{}, len(names))
	for _, name := range names {
		want[name] = struct{}{}
	}
	pods, err := reaper.ListTenantAgentPods(ctx, namespace)
	if err != nil {
		return 0, fmt.Errorf("list the tenant agent pods in namespace %q: %w", namespace, err)
	}
	sa := tenantAgentServiceAccountName(tenant)
	deleted := 0
	var first error
	for _, pod := range pods {
		if _, named := want[pod.Name]; !named {
			continue
		}
		if pod.Spec.ServiceAccountName != sa {
			continue
		}
		if _, ok := pod.Labels[agentPodControllerLabel]; !ok {
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		if err := reaper.DeletePodWithUID(ctx, namespace, pod.Name, pod.UID); err != nil {
			if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
				continue
			}
			if first == nil {
				first = fmt.Errorf("delete the run agent pod %q in namespace %q: %w", pod.Name, namespace, err)
			}
			continue
		}
		deleted++
	}
	return deleted, first
}

// ReleaseInstanceAgentPods deletes the idle tenant agent pods of a cell
// instance that the operator confirmed gone. The recorder names the nodes
// that a resume can still adopt in keep, and those pods stay. Any operator
// replica can run this. Each delete carries the listed UID, so a repeat or
// a parallel action is harmless.
func (r *Reconciler) ReleaseInstanceAgentPods(ctx context.Context, tenant, namespace string, keep []string, refreshedBefore time.Time) {
	reaper, ok := r.client.(tenantAgentPodReaper)
	if !ok {
		return
	}
	deleted, err := releaseInstanceAgentPods(ctx, reaper, tenant, namespace, keep, refreshedBefore, time.Now())
	if err != nil {
		r.Logger.Warn("release instance agent pods failed", "tenant", tenant, "namespace", namespace, "error", err)
	}
	for range deleted {
		r.Logger.Info("released an instance agent pod", "tenant", tenant, "namespace", namespace, "reason", "instance-gone")
	}
}

// releaseInstanceAgentPods deletes the pods that staleInstanceAgentPods
// selects, each with its listed UID as a precondition. It answers the count
// of pods it deleted. NotFound and Conflict are not errors. A later error
// does not stop the pass: the first other error is returned after the rest
// of the pods were tried.
func releaseInstanceAgentPods(ctx context.Context, reaper tenantAgentPodReaper, tenant, namespace string,
	keep []string, refreshedBefore, now time.Time) (int, error) {
	if namespace == "" {
		return 0, nil
	}
	pods, err := reaper.ListTenantAgentPods(ctx, namespace)
	if err != nil {
		return 0, fmt.Errorf("list the tenant agent pods in namespace %q: %w", namespace, err)
	}
	deleted := 0
	var first error
	for _, pod := range staleInstanceAgentPods(pods, tenant, keep, refreshedBefore, now) {
		if err := reaper.DeletePodWithUID(ctx, namespace, pod.Name, pod.UID); err != nil {
			if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
				continue
			}
			if first == nil {
				first = fmt.Errorf("delete the instance agent pod %q in namespace %q: %w", pod.Name, namespace, err)
			}
			continue
		}
		deleted++
	}
	return deleted, first
}

// staleInstanceAgentPods answers the pods of tenant that a confirmed gone
// cell instance left idle. A pod qualifies when it runs as the tenant agent
// ServiceAccount, carries the plugin's controller label, is not being
// deleted, is not named in keep, and has a parseable last refresh that is
// at or before refreshedBefore and more than tenantAgentPendingReapAfter
// old at now. The pod phase does not matter. The three-minute floor reuses
// the three-missed-refresh bound, because a live sibling slot's pod can show
// a refresh just before the confirmation. A pod with no annotation, or one
// that does not parse, always stays.
func staleInstanceAgentPods(pods []corev1.Pod, tenant string, keep []string, refreshedBefore, now time.Time) []corev1.Pod {
	kept := make(map[string]struct{}, len(keep))
	for _, name := range keep {
		kept[name] = struct{}{}
	}
	sa := tenantAgentServiceAccountName(tenant)
	var out []corev1.Pod
	for _, pod := range pods {
		if pod.Spec.ServiceAccountName != sa {
			continue
		}
		if _, ok := pod.Labels[agentPodControllerLabel]; !ok {
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		if _, ok := kept[pod.Name]; ok {
			continue
		}
		refreshed, ok := agentPodLastRefresh(pod)
		if !ok {
			continue
		}
		if refreshed.After(refreshedBefore) {
			continue
		}
		if now.Sub(refreshed) <= tenantAgentPendingReapAfter {
			continue
		}
		out = append(out, pod)
	}
	return out
}
