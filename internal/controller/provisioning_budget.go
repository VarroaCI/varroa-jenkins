package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// The provisioning budget counts time spent on image pulls for the
// current pod as zero, up to maxImagePullCredit. A pull needs no timeout
// of its own. The hard cap bounds that credit, so a pull that never ends
// still fails provisioning.

// imagePull names a container whose image kubelet pulls now.
type imagePull struct{ Container, Image string }

// provisioningLimits holds the two time limits of one provisioning run.
type provisioningLimits struct {
	Base time.Duration // budget for the time outside image pulls
	Hard time.Duration // limit for the whole run, image pulls included
}

// provisioningLimitsFrom is the one reader of provisioningTimeoutSec. A nil
// defaults or a value of zero or less gives provisioningTimeout. The hard
// limit is the base plus maxImagePullCredit, so it is always above the base.
func provisioningLimitsFrom(defaults *v1alpha1.ProvisioningDefaults) provisioningLimits {
	base := provisioningTimeout
	if defaults != nil && defaults.Spec.ProvisioningTimeoutSec > 0 {
		base = time.Duration(defaults.Spec.ProvisioningTimeoutSec) * time.Second
	}
	return provisioningLimits{Base: base, Hard: base + maxImagePullCredit}
}

// currentProvisioningLimits reads ProvisioningDefaults now. A failed read
// gives the default limits, the same rule as every optional defaults field.
func (r *Reconciler) currentProvisioningLimits(ctx context.Context) provisioningLimits {
	d, err := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, r.store, provisioningDefaultsName, "")
	if err != nil {
		return provisioningLimitsFrom(nil)
	}
	return provisioningLimitsFrom(d)
}

// provisioningBudget is the provisioning timeout verdict of one tick.
type provisioningBudget struct {
	Elapsed  time.Duration // since status.provisioningStartedAt
	PullWait time.Duration // part of Elapsed spent on image pulls of the current pod
	Pull     *imagePull    // the pull that runs now, or nil
	Limits   provisioningLimits
}

// exceeded reports whether provisioning has run out of time.
func (b provisioningBudget) exceeded() bool {
	return b.Elapsed > b.Limits.Hard || b.Elapsed-b.PullWait > b.Limits.Base
}

// failureMessage describes the verdict for status.conditions.
func (b provisioningBudget) failureMessage() string {
	if b.Elapsed > b.Limits.Hard {
		if b.Pull != nil {
			return fmt.Sprintf("provisioning exceeded %v; container %s still pulls image %s",
				b.Limits.Hard, b.Pull.Container, b.Pull.Image)
		}
		return fmt.Sprintf("provisioning exceeded %v", b.Limits.Hard)
	}
	if b.PullWait > 0 {
		return fmt.Sprintf("provisioning exceeded %v, not counting %v of image pulls",
			b.Limits.Base, b.PullWait.Round(time.Second))
	}
	return fmt.Sprintf("provisioning exceeded %v", b.Limits.Base)
}

// podPullEventLister lists the Pulling events of one pod. Only the
// clientset implements it. A test double may omit it, and the budget then
// earns no pull credit.
type podPullEventLister interface {
	ListPodPullEvents(ctx context.Context, namespace string, podUID types.UID) ([]corev1.Event, error)
}

var _ podPullEventLister = (*ClientsetClient)(nil)

// provisioningBudgetFor computes the budget of one tick. It reads events only
// when the answer can change the verdict: a container waits to be created, or
// the base budget has passed.
func (r *Reconciler) provisioningBudgetFor(ctx context.Context, cr *v1alpha1.Controller, pod *corev1.Pod, limits provisioningLimits, now time.Time) provisioningBudget {
	if cr.Status.ProvisioningStartedAt == nil {
		return provisioningBudget{Limits: limits}
	}
	since := cr.Status.ProvisioningStartedAt.Time
	b := provisioningBudget{Elapsed: now.Sub(since), Limits: limits}
	if pod == nil {
		return b
	}
	if b.Elapsed <= limits.Base && nextStartWaiter(pod) == "" {
		return b
	}
	lister, ok := r.client.(podPullEventLister)
	if !ok {
		return b
	}
	events, err := lister.ListPodPullEvents(ctx, pod.Namespace, pod.UID)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Debug("list pod pull events", "controller", cr.Namespace+"/"+cr.Name, "error", err)
		}
		return b
	}
	b.PullWait, b.Pull = imagePullWait(pod, pullStartsByContainer(events), since, now)
	return b
}

// isCreateWait reports whether a container waits for kubelet to create it.
// The status does not change while the first init container pulls its
// image, so only a Pulling event separates a pull from a volume or sandbox
// wait.
func isCreateWait(s corev1.ContainerStatus) bool {
	if s.State.Waiting == nil {
		return false
	}
	return s.State.Waiting.Reason == "PodInitializing" || s.State.Waiting.Reason == "ContainerCreating"
}

// nextStartWaiter names the first container that waits to be created, or
// returns "" when no container waits.
func nextStartWaiter(pod *corev1.Pod) string {
	statuses := make(map[string]corev1.ContainerStatus, len(pod.Status.InitContainerStatuses))
	for _, s := range pod.Status.InitContainerStatuses {
		statuses[s.Name] = s
	}
	for _, c := range pod.Spec.InitContainers {
		s, ok := statuses[c.Name]
		if !ok {
			return ""
		}
		switch {
		case isCreateWait(s):
			return c.Name
		case s.State.Terminated != nil && s.State.Terminated.ExitCode == 0:
			continue
		case s.State.Running != nil && c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways:
			continue
		default:
			return ""
		}
	}
	for _, s := range pod.Status.ContainerStatuses {
		if isCreateWait(s) {
			return s.Name
		}
	}
	return ""
}

// pullStartsByContainer keeps the latest Pulling event time of each container.
func pullStartsByContainer(events []corev1.Event) map[string]time.Time {
	starts := make(map[string]time.Time)
	for _, ev := range events {
		if ev.Reason != "Pulling" {
			continue
		}
		name, ok := containerNameFromFieldPath(ev.InvolvedObject.FieldPath)
		if !ok {
			continue
		}
		t := ev.LastTimestamp.Time
		if t.IsZero() {
			t = ev.EventTime.Time
		}
		if t.IsZero() {
			t = ev.FirstTimestamp.Time
		}
		if t.IsZero() {
			continue
		}
		if t.After(starts[name]) {
			starts[name] = t
		}
	}
	return starts
}

// containerNameFromFieldPath returns the container name in an event field
// path such as spec.initContainers{plugins-init}.
func containerNameFromFieldPath(fp string) (string, bool) {
	for _, prefix := range []string{"spec.initContainers{", "spec.containers{"} {
		if rest, ok := strings.CutPrefix(fp, prefix); ok {
			name, ok := strings.CutSuffix(rest, "}")
			return name, ok && name != ""
		}
	}
	return "", false
}

// imagePullWait sums the image-pull windows of the pod that overlap the
// interval [since, now]. It also names the pull that runs now, if one does.
// A window runs from the container's Pulling event to the container start,
// or to now while the container still waits to be created. Kubelet starts
// one container at a time, so the windows do not overlap.
func imagePullWait(pod *corev1.Pod, pullStarts map[string]time.Time, since, now time.Time) (time.Duration, *imagePull) {
	images := make(map[string]string)
	for _, c := range pod.Spec.InitContainers {
		images[c.Name] = c.Image
	}
	for _, c := range pod.Spec.Containers {
		images[c.Name] = c.Image
	}
	statuses := make([]corev1.ContainerStatus, 0, len(pod.Status.InitContainerStatuses)+len(pod.Status.ContainerStatuses))
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.ContainerStatuses...)

	var total time.Duration
	var current *imagePull
	for _, s := range statuses {
		start, ok := pullStarts[s.Name]
		if !ok {
			continue
		}
		// A pull that began before a restart ended when the first run started.
		// LastTerminationState keeps that start only while one run has ended.
		// With more restarts, no start of an earlier run is known, so no credit.
		if s.RestartCount > 0 && !pullAfterLastExit(s, start) {
			if last := s.LastTerminationState.Terminated; s.RestartCount == 1 && last != nil {
				total += windowOverlap(start, last.StartedAt.Time, since, now)
			}
			continue
		}
		switch {
		case s.State.Running != nil:
			total += windowOverlap(start, s.State.Running.StartedAt.Time, since, now)
		case s.State.Terminated != nil:
			total += windowOverlap(start, s.State.Terminated.StartedAt.Time, since, now)
		case isCreateWait(s):
			total += windowOverlap(start, now, since, now)
			if current == nil {
				image := images[s.Name]
				if image == "" {
					image = s.Image
				}
				current = &imagePull{Container: s.Name, Image: image}
			}
		}
	}
	return total, current
}

// pullAfterLastExit reports whether a pull that began at start belongs to the
// current run of a restarted container. It belongs when the previous run ended
// before the pull began.
func pullAfterLastExit(s corev1.ContainerStatus, start time.Time) bool {
	last := s.LastTerminationState.Terminated
	return last != nil && start.After(last.FinishedAt.Time)
}

// windowOverlap returns the length of [start, end] inside [lo, hi].
func windowOverlap(start, end, lo, hi time.Time) time.Duration {
	if start.Before(lo) {
		start = lo
	}
	if end.After(hi) {
		end = hi
	}
	if !end.After(start) {
		return 0
	}
	return end.Sub(start)
}

// setImagePullingCondition records the pull that runs now, or removes the
// condition when none runs.
func setImagePullingCondition(cr *v1alpha1.Controller, pull *imagePull) {
	if pull == nil {
		cr.Status.Conditions = removeConditionByType(cr.Status.Conditions, v1alpha1.ConditionImagePulling)
		return
	}
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionImagePulling,
		Status:  metav1.ConditionTrue,
		Reason:  v1alpha1.ReasonImagePullInProgress,
		Message: fmt.Sprintf("pulling image %s for container %s", pull.Image, pull.Container),
	})
}
