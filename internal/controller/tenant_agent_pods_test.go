package controller

import (
	"context"
	"log/slog"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// agentPod builds one agent pod in payments-ns. A zero refreshed leaves
// the last-refresh annotation out.
func agentPod(name, sa string, controllerLabel bool, refreshed time.Time) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "payments-ns",
			UID:         types.UID("uid-" + name),
			Labels:      map[string]string{agentPodDefaultLabelKey: agentPodDefaultLabelValue},
			Annotations: map[string]string{},
		},
		Spec: corev1.PodSpec{ServiceAccountName: sa},
	}
	if controllerLabel {
		pod.Labels[agentPodControllerLabel] = "varroa-agents-payments-0-abc"
	}
	if !refreshed.IsZero() {
		pod.Annotations[agentPodLastRefreshAnnot] = strconv.FormatInt(refreshed.UnixMilli(), 10)
	}
	return pod
}

// phased sets the pod phase on an agent pod.
func phased(pod corev1.Pod, phase corev1.PodPhase) corev1.Pod {
	pod.Status.Phase = phase
	return pod
}

func TestOrphanTenantAgentPods(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	const sa = "varroa-agent-payments"
	unparseable := agentPod("unparseable", sa, true, time.Time{})
	unparseable.Annotations[agentPodLastRefreshAnnot] = "yesterday"
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	cases := []struct {
		name        string
		pod         corev1.Pod
		tearingDown bool
		want        bool
	}{
		{"refreshed 60s ago is kept", agentPod("fresh", sa, true, ago(60*time.Second)), false, false},
		{"stale pod of another ServiceAccount is kept", agentPod("other-sa", "varroa-agent-search", true, ago(time.Hour)), false, false},
		{"stale pod with no controller label is kept", agentPod("no-label", sa, false, ago(time.Hour)), false, false},
		{"pod with no annotation is kept", agentPod("no-annotation", sa, true, time.Time{}), false, false},
		{"pod with an unparseable annotation is kept", unparseable, false, false},
		{"pending pod refreshed 120s ago is kept", phased(agentPod("pending-120", sa, true, ago(120*time.Second)), corev1.PodPending), false, false},
		{"pending pod past 180s is deleted", phased(agentPod("pending-181", sa, true, ago(181*time.Second)), corev1.PodPending), false, true},
		{"failed pod past 180s is deleted", phased(agentPod("failed-181", sa, true, ago(181*time.Second)), corev1.PodFailed), false, true},
		{"succeeded pod past 180s is deleted", phased(agentPod("succeeded-181", sa, true, ago(181*time.Second)), corev1.PodSucceeded), false, true},
		{"running pod inside the resume window is kept", phased(agentPod("running-20m", sa, true, ago(20*time.Minute)), corev1.PodRunning), false, false},
		{"running pod at the plugin timeout is kept", phased(agentPod("running-gc", sa, true, ago(tenantAgentGCTimeoutSeconds*time.Second)), corev1.PodRunning), false, false},
		{"running pod past the running age is deleted", phased(agentPod("running-old", sa, true, ago(tenantAgentRunningReapAfter+time.Second)), corev1.PodRunning), false, true},
		{"unknown-phase pod gets the running age", phased(agentPod("unknown-20m", sa, true, ago(20*time.Minute)), corev1.PodUnknown), false, false},
		{"teardown deletes a fresh tenant pod", agentPod("fresh-teardown", sa, true, ago(10*time.Second)), true, true},
		{"teardown deletes a fresh running tenant pod", phased(agentPod("running-teardown", sa, true, ago(10*time.Second)), corev1.PodRunning), true, true},
		{"teardown keeps another ServiceAccount's pod", agentPod("other-teardown", "varroa-agent-search", true, now), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := orphanTenantAgentPods([]corev1.Pod{tc.pod}, "payments", now, tc.tearingDown)
			if (len(got) == 1) != tc.want {
				t.Errorf("orphanTenantAgentPods = %d pods, want deleted=%v", len(got), tc.want)
			}
		})
	}
}

// TestReapTenantAgentPods_DeletesAStalePendingPodAndKeepsAnAdoptableRunningPod
// models a cell roll: the old instance left one Pending pod for a node
// block that waited for an agent, and one Running pod whose node body a
// resume adopts later. Both stopped their refresh ten minutes ago.
func TestReapTenantAgentPods_DeletesAStalePendingPodAndKeepsAnAdoptableRunningPod(t *testing.T) {
	now := time.Now()
	pending := phased(agentPod("old-pending", "varroa-agent-payments", true, now.Add(-10*time.Minute)), corev1.PodPending)
	running := phased(agentPod("old-running", "varroa-agent-payments", true, now.Add(-10*time.Minute)), corev1.PodRunning)
	fc := fake.NewSimpleClientset(&pending, &running)

	rec := NewTenantReconciler(&ClientsetClient{clientset: fc}, crdstore.NewFake(), tenantTestNS, slog.Default())
	if _, err := rec.reapTenantAgentPods(context.Background(), "payments", "payments-ns", false); err != nil {
		t.Fatalf("reapTenantAgentPods: %v", err)
	}

	left, err := fc.CoreV1().Pods("payments-ns").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	names := map[string]bool{}
	for _, p := range left.Items {
		names[p.Name] = true
	}
	if names["old-pending"] || !names["old-running"] {
		t.Errorf("pods left = %v, want old-running only", names)
	}
}

func TestReapTenantAgentPods_DeletesOnlyTheStalePodWithAUIDPrecondition(t *testing.T) {
	now := time.Now()
	stale := agentPod("stale", "varroa-agent-payments", true, now.Add(-time.Hour))
	fresh := agentPod("fresh", "varroa-agent-payments", true, now.Add(-30*time.Second))
	foreign := agentPod("foreign", "jenkins-agent", true, now.Add(-time.Hour))
	fc := fake.NewSimpleClientset(&stale, &fresh, &foreign)

	var preconditionUIDs []types.UID
	fc.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		del := action.(k8stesting.DeleteActionImpl)
		if p := del.GetDeleteOptions().Preconditions; p != nil && p.UID != nil {
			preconditionUIDs = append(preconditionUIDs, *p.UID)
		} else {
			preconditionUIDs = append(preconditionUIDs, "")
		}
		return false, nil, nil
	})

	rec := NewTenantReconciler(&ClientsetClient{clientset: fc}, crdstore.NewFake(), tenantTestNS, slog.Default())
	if _, err := rec.reapTenantAgentPods(context.Background(), "payments", "payments-ns", false); err != nil {
		t.Fatalf("reapTenantAgentPods: %v", err)
	}

	left, err := fc.CoreV1().Pods("payments-ns").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	names := map[string]bool{}
	for _, p := range left.Items {
		names[p.Name] = true
	}
	if names["stale"] || !names["fresh"] || !names["foreign"] {
		t.Errorf("pods left = %v, want fresh and foreign only", names)
	}
	if len(preconditionUIDs) != 1 || preconditionUIDs[0] != stale.UID {
		t.Errorf("delete preconditions = %v, want exactly [%s]", preconditionUIDs, stale.UID)
	}
}

func TestReapTenantAgentPods_ConflictIsNotAnError(t *testing.T) {
	stale := agentPod("stale", "varroa-agent-payments", true, time.Now().Add(-time.Hour))
	fc := fake.NewSimpleClientset(&stale)
	fc.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "stale", nil)
	})
	rec := NewTenantReconciler(&ClientsetClient{clientset: fc}, crdstore.NewFake(), tenantTestNS, slog.Default())
	if _, err := rec.reapTenantAgentPods(context.Background(), "payments", "payments-ns", false); err != nil {
		t.Errorf("a UID conflict answered %v, want no error", err)
	}
}

func TestReapTenantAgentPods_TeardownCount(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"a deleted pod still terminates", nil, 1},
		{"a NotFound pod no longer counts", apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "p"), 0},
		{"a Conflict pod still counts", apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "p", nil), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := agentPod("p", "varroa-agent-payments", true, time.Now())
			fc := fake.NewSimpleClientset(&pod)
			if tc.err != nil {
				fc.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tc.err
				})
			}
			rec := NewTenantReconciler(&ClientsetClient{clientset: fc}, crdstore.NewFake(), tenantTestNS, slog.Default())
			remaining, err := rec.reapTenantAgentPods(context.Background(), "payments", "payments-ns", true)
			if err != nil {
				t.Fatalf("reapTenantAgentPods: %v", err)
			}
			if remaining != tc.want {
				t.Errorf("remaining = %d, want %d", remaining, tc.want)
			}
		})
	}
}

func TestReapTenantAgentPodsDue_RateLimitsPerTenant(t *testing.T) {
	rec := &TenantReconciler{}
	now := time.Now()
	if !rec.reapTenantAgentPodsDue("payments", now) {
		t.Fatal("the first list was not due")
	}
	if rec.reapTenantAgentPodsDue("payments", now.Add(30*time.Second)) {
		t.Error("a second list inside one minute was due")
	}
	if !rec.reapTenantAgentPodsDue("search", now.Add(30*time.Second)) {
		t.Error("another tenant's list was held by this tenant's limit")
	}
	if !rec.reapTenantAgentPodsDue("payments", now.Add(61*time.Second)) {
		t.Error("a list after one minute was not due")
	}
}

// podReaperClient adds an in-memory agent pod list to testClient. It
// records whether the tenant ServiceAccount was already deleted when a pod
// delete arrived.
type podReaperClient struct {
	*testClient
	pods               []corev1.Pod
	deleted            []string
	deletedAfterSAGone bool
}

func (c *podReaperClient) ListTenantAgentPods(_ context.Context, namespace string) ([]corev1.Pod, error) {
	var out []corev1.Pod
	for _, p := range c.pods {
		if p.Namespace == namespace {
			out = append(out, p)
		}
	}
	return out, nil
}

func (c *podReaperClient) DeletePodWithUID(_ context.Context, namespace, name string, uid types.UID) error {
	for i, p := range c.pods {
		if p.Namespace == namespace && p.Name == name && p.UID == uid {
			for _, d := range c.deletedWithNamespace {
				if d.kind == "ServiceAccount" && d.name == tenantAgentServiceAccountName("payments") {
					c.deletedAfterSAGone = true
				}
			}
			c.deleted = append(c.deleted, name)
			c.pods = append(c.pods[:i], c.pods[i+1:]...)
			return nil
		}
	}
	return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)
}

func TestTenantReconciler_TeardownHoldsFinalizerUntilAgentPodsAreGone(t *testing.T) {
	base := newTenantTestClient()
	base.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(base)

	// No cell object exists, so teardown would release the finalizer on
	// the first tick if no agent pod remained.
	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "payments",
			UID:        types.UID("10000000-0000-0000-0000-000000000738"),
			Finalizers: []string{v1alpha1.TenantFinalizerTenantPlacement},
		},
		Spec: v1alpha1.TenantSpec{Namespaces: []string{"payments-ns"}},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{
				Cluster: "core", Cell: "cell-gone", CellNamespace: "cells-ns", Generation: 3,
			},
			Pipelines: &v1alpha1.TenantPipelinesStatus{
				WebhookToken: "tok", WebhookSecret: "sec", AgentNamespace: "payments-ns",
			},
		},
	}
	base.tenants["payments"] = tenant
	crdstore.MustSeed(base.store, tenant)

	client := &podReaperClient{
		testClient: base,
		pods:       []corev1.Pod{agentPod("payments-default-abc", "varroa-agent-payments", true, time.Now())},
	}
	rec := NewTenantReconciler(client, base.store, tenantTestNS, slog.Default())
	rec.SetCellsConfig(CellsConfig{MaxTenantsPerCell: 10})
	rec.SetPipelinesGates("1", "s3://bucket/prefix")
	rec.SetClusterIdentity("core")
	rec.SetPurgeRequester(succeedingPurgeFunc)
	rec.SetSyncAnnouncer(succeedingAnnounceFunc)
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}

	// Tick one: the fresh pod is deleted at once, but it still counts,
	// so the finalizer and the agent namespace stay.
	if _, err := rec.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if len(client.deleted) != 1 || client.deleted[0] != "payments-default-abc" {
		t.Fatalf("deleted pods = %v, want [payments-default-abc]", client.deleted)
	}
	if client.deletedAfterSAGone {
		t.Fatal("the agent pod was deleted after the tenant ServiceAccount")
	}
	saDeleted := false
	for _, d := range base.deletedWithNamespace {
		if d.kind == "ServiceAccount" && d.name == "varroa-agent-payments" {
			saDeleted = true
		}
	}
	if !saDeleted {
		t.Fatal("stage one did not delete the tenant ServiceAccount on the same tick")
	}
	if !tenantHoldsPlacementFinalizer(t, base) {
		t.Fatal("the finalizer was released while an agent pod remained")
	}
	status := lastTenantStatus(base)
	if status == nil || status.Pipelines == nil || status.Pipelines.AgentNamespace != "payments-ns" {
		t.Fatalf("the agent namespace was not kept for the next tick: %+v", status)
	}
	if got := placedReasonOf(status); got != v1alpha1.TenantReasonTenantTeardownPending {
		t.Fatalf("Placed reason = %q, want TenantTeardownPending", got)
	}

	// Tick two: no pod remains, so teardown completes.
	if _, err := rec.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if tenantHoldsPlacementFinalizer(t, base) {
		t.Fatal("the finalizer was not released once the agent pods were gone")
	}
}

// releaseReaper is an in-memory tenantAgentPodReaper whose delete answers
// the error named for a pod, so NotFound and Conflict paths run.
type releaseReaper struct {
	pods    []corev1.Pod
	errs    map[string]error
	deleted []string
	uids    []types.UID
}

func (r *releaseReaper) ListTenantAgentPods(_ context.Context, _ string) ([]corev1.Pod, error) {
	return r.pods, nil
}

func (r *releaseReaper) DeletePodWithUID(_ context.Context, _, name string, uid types.UID) error {
	if err := r.errs[name]; err != nil {
		return err
	}
	r.deleted = append(r.deleted, name)
	r.uids = append(r.uids, uid)
	return nil
}

// TestReleaseNamedAgentPods_DeletesOnlyNamedTenantPods proves a release
// deletes only the named pods of the tenant's ServiceAccount that carry
// the controller label, each with its UID as a precondition, and that
// NotFound and Conflict are not errors.
func TestReleaseNamedAgentPods_DeletesOnlyNamedTenantPods(t *testing.T) {
	now := time.Now()
	const sa = "varroa-agent-payments"
	named := agentPod("named", sa, true, now)
	foreignSA := agentPod("foreign-sa", "jenkins-agent", true, now)
	unlabelled := agentPod("unlabelled", sa, false, now)
	unnamed := agentPod("unnamed", sa, true, now)
	gone := agentPod("gone", sa, true, now)
	conflict := agentPod("conflict", sa, true, now)
	reaper := &releaseReaper{
		pods: []corev1.Pod{named, foreignSA, unlabelled, unnamed, gone, conflict},
		errs: map[string]error{
			"gone":     apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "gone"),
			"conflict": apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "conflict", nil),
		},
	}

	deleted, err := releaseNamedAgentPods(context.Background(), reaper, "payments", "payments-ns",
		[]string{"named", "foreign-sa", "unlabelled", "gone", "conflict"})
	if err != nil {
		t.Fatalf("releaseNamedAgentPods: %v", err)
	}
	if deleted != 1 || len(reaper.deleted) != 1 || reaper.deleted[0] != "named" {
		t.Fatalf("deleted = %d %v, want only named", deleted, reaper.deleted)
	}
	if reaper.uids[0] != named.UID {
		t.Errorf("delete UID = %q, want the listed UID %q", reaper.uids[0], named.UID)
	}
}

// TestReleaseNamedAgentPods_OtherErrorIsReported proves a delete that
// fails for another reason is returned, and the rest still go.
func TestReleaseNamedAgentPods_OtherErrorIsReported(t *testing.T) {
	now := time.Now()
	const sa = "varroa-agent-payments"
	a := agentPod("a", sa, true, now)
	b := agentPod("b", sa, true, now)
	reaper := &releaseReaper{
		pods: []corev1.Pod{a, b},
		errs: map[string]error{"a": apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "a", nil)},
	}
	deleted, err := releaseNamedAgentPods(context.Background(), reaper, "payments", "payments-ns", []string{"a", "b"})
	if err == nil {
		t.Fatal("want the forbidden delete reported")
	}
	if deleted != 1 || len(reaper.deleted) != 1 || reaper.deleted[0] != "b" {
		t.Fatalf("deleted = %d %v, want b only", deleted, reaper.deleted)
	}
}

// TestStaleInstanceAgentPods proves the instance filter deletes only an idle
// pod of the tenant that no live JVM refreshed and that no resume can adopt.
func TestStaleInstanceAgentPods(t *testing.T) {
	now := time.Now()
	const sa = "varroa-agent-payments"
	refreshedBefore := now.Add(-4 * time.Minute)
	stale := agentPod("stale", sa, true, now.Add(-5*time.Minute))
	adoptable := agentPod("adoptable", sa, true, now.Add(-5*time.Minute))
	fresh := agentPod("fresh", sa, true, now.Add(-time.Minute))
	floorInside := agentPod("floor-inside", sa, true, now.Add(-2*time.Minute))
	afterConfirm := agentPod("after-confirm", sa, true, now.Add(-3*time.Minute))
	foreignSA := agentPod("foreign-sa", "jenkins-agent", true, now.Add(-5*time.Minute))
	unlabelled := agentPod("unlabelled", sa, false, now.Add(-5*time.Minute))
	unannotated := agentPod("unannotated", sa, true, time.Time{})
	deleting := agentPod("deleting", sa, true, now.Add(-5*time.Minute))
	deletedAt := metav1.NewTime(now)
	deleting.DeletionTimestamp = &deletedAt

	pods := []corev1.Pod{stale, adoptable, fresh, floorInside, afterConfirm, foreignSA, unlabelled, unannotated, deleting}
	got := staleInstanceAgentPods(pods, "payments", []string{"adoptable"}, refreshedBefore, now)
	if len(got) != 1 || got[0].Name != "stale" {
		names := make([]string, 0, len(got))
		for _, pod := range got {
			names = append(names, pod.Name)
		}
		t.Fatalf("selected = %v, want [stale]", names)
	}

	// A pod refreshed after the confirmation is kept, even when its refresh
	// is older than the floor: a live pod at a later refresh is not this
	// instance's leftover.
	later := staleInstanceAgentPods([]corev1.Pod{afterConfirm}, "payments", nil,
		now.Add(-10*time.Minute), now)
	if len(later) != 0 {
		t.Fatalf("pod refreshed after the confirmation was selected: %v", later)
	}
	// A pod refreshed at or before the confirmation but inside the floor stays.
	inside := staleInstanceAgentPods([]corev1.Pod{floorInside}, "payments", nil,
		now.Add(-time.Minute), now)
	if len(inside) != 0 {
		t.Fatalf("pod inside the three-minute floor was selected: %v", inside)
	}
}

// TestReleaseInstanceAgentPods_DeletesStaleIdleWithUIDAndToleratesNotFoundConflict
// proves the delete pass names each stale pod with its UID, treats NotFound
// and Conflict as no error, and keeps going after a Forbidden error.
func TestReleaseInstanceAgentPods_DeletesStaleIdleWithUIDAndToleratesNotFoundConflict(t *testing.T) {
	now := time.Now()
	const sa = "varroa-agent-payments"
	stale := agentPod("stale", sa, true, now.Add(-5*time.Minute))
	gone := agentPod("gone", sa, true, now.Add(-5*time.Minute))
	conflict := agentPod("conflict", sa, true, now.Add(-5*time.Minute))
	forbidden := agentPod("forbidden", sa, true, now.Add(-5*time.Minute))
	kept := agentPod("kept", sa, true, now.Add(-5*time.Minute))
	reaper := &releaseReaper{
		pods: []corev1.Pod{stale, gone, conflict, forbidden, kept},
		errs: map[string]error{
			"gone":      apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "gone"),
			"conflict":  apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "conflict", nil),
			"forbidden": apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "forbidden", nil),
		},
	}
	refreshedBefore := now.Add(-4 * time.Minute)
	deleted, err := releaseInstanceAgentPods(context.Background(), reaper, "payments", "payments-ns",
		[]string{"kept"}, refreshedBefore, now)
	if err == nil {
		t.Fatal("want the forbidden delete reported")
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	want := []string{"stale"}
	for _, name := range reaper.deleted {
		if name != "stale" {
			t.Fatalf("deleted %v, want only %v", reaper.deleted, want)
		}
	}
	if len(reaper.uids) != 1 || reaper.uids[0] != stale.UID {
		t.Fatalf("delete UIDs = %v, want the listed UID %q", reaper.uids, stale.UID)
	}

	// NotFound and Conflict alone return no error.
	clean := &releaseReaper{
		pods: []corev1.Pod{gone, conflict},
		errs: map[string]error{
			"gone":     apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "gone"),
			"conflict": apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "conflict", nil),
		},
	}
	if _, err := releaseInstanceAgentPods(context.Background(), clean, "payments", "payments-ns",
		nil, refreshedBefore, now); err != nil {
		t.Fatalf("NotFound or Conflict returned an error: %v", err)
	}
}
