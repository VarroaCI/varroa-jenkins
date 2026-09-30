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
func orphanTenantAgentPods(pods []corev1.Pod, team string, now time.Time, tearingDown bool) []corev1.Pod {
	sa := tenantAgentServiceAccountName(team)
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
func (r *TeamReconciler) reapTenantAgentPods(ctx context.Context, team, agentNamespace string, tearingDown bool) (int, error) {
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
	orphans := orphanTenantAgentPods(pods, team, now, tearingDown)
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
			"team", team, "reason", reason)
	}
	remaining := len(orphanTenantAgentPods(pods, team, now, true)) - gone
	return remaining, first
}

// reapTenantAgentPodsDue reports whether the steady-state reaper may list
// the tenant's pods on this tick, and records the list time when it may.
func (r *TeamReconciler) reapTenantAgentPodsDue(team string, now time.Time) bool {
	r.reapMu.Lock()
	defer r.reapMu.Unlock()
	if r.lastReap == nil {
		r.lastReap = map[string]time.Time{}
	}
	if last, ok := r.lastReap[team]; ok && now.Sub(last) < tenantAgentReapInterval {
		return false
	}
	r.lastReap[team] = now
	return true
}

// forgetTenantAgentReap drops the tenant's rate-limit entry after teardown,
// so a Team name that is reused later starts with no stale entry.
func (r *TeamReconciler) forgetTenantAgentReap(team string) {
	r.reapMu.Lock()
	defer r.reapMu.Unlock()
	delete(r.lastReap, team)
}
