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

func TestOrphanTenantAgentPods(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	const sa = "varroa-agent-payments"
	unparseable := agentPod("unparseable", sa, true, time.Time{})
	unparseable.Annotations[agentPodLastRefreshAnnot] = "yesterday"
	pending := agentPod("stale-pending", sa, true, now.Add(-20*time.Minute))
	pending.Status.Phase = corev1.PodPending
	running := agentPod("stale-running", sa, true, now.Add(-901*time.Second))
	running.Status.Phase = corev1.PodRunning

	cases := []struct {
		name        string
		pod         corev1.Pod
		tearingDown bool
		want        bool
	}{
		{"refreshed 60s ago is kept", agentPod("fresh", sa, true, now.Add(-60*time.Second)), false, false},
		{"stale pod of another ServiceAccount is kept", agentPod("other-sa", "varroa-agent-search", true, now.Add(-time.Hour)), false, false},
		{"stale pod with no controller label is kept", agentPod("no-label", sa, false, now.Add(-time.Hour)), false, false},
		{"pod with no annotation is kept", agentPod("no-annotation", sa, true, time.Time{}), false, false},
		{"pod with an unparseable annotation is kept", unparseable, false, false},
		{"stale running pod past 900s is deleted", running, false, true},
		{"stale pending pod is deleted", pending, false, true},
		{"teardown deletes a fresh tenant pod", agentPod("fresh-teardown", sa, true, now.Add(-10*time.Second)), true, true},
		{"teardown keeps another ServiceAccount's pod", agentPod("other-teardown", "varroa-agent-search", true, now), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := orphanTenantAgentPods([]corev1.Pod{tc.pod}, "payments", now, tenantAgentReapAfter, tc.tearingDown)
			if (len(got) == 1) != tc.want {
				t.Errorf("orphanTenantAgentPods = %d pods, want deleted=%v", len(got), tc.want)
			}
		})
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

	rec := NewTeamReconciler(&ClientsetClient{clientset: fc}, crdstore.NewFake(), teamTestNS, slog.Default())
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
	rec := NewTeamReconciler(&ClientsetClient{clientset: fc}, crdstore.NewFake(), teamTestNS, slog.Default())
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
			rec := NewTeamReconciler(&ClientsetClient{clientset: fc}, crdstore.NewFake(), teamTestNS, slog.Default())
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

func TestReapTenantAgentPodsDue_RateLimitsPerTeam(t *testing.T) {
	rec := &TeamReconciler{}
	now := time.Now()
	if !rec.reapTenantAgentPodsDue("payments", now) {
		t.Fatal("the first list was not due")
	}
	if rec.reapTenantAgentPodsDue("payments", now.Add(30*time.Second)) {
		t.Error("a second list inside one minute was due")
	}
	if !rec.reapTenantAgentPodsDue("search", now.Add(30*time.Second)) {
		t.Error("another team's list was held by this team's limit")
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

func TestTeamReconciler_TeardownHoldsFinalizerUntilAgentPodsAreGone(t *testing.T) {
	base := newTeamTestClient()
	base.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(base)

	// No cell object exists, so teardown would release the finalizer on
	// the first tick if no agent pod remained.
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "payments",
			UID:        types.UID("10000000-0000-0000-0000-000000000738"),
			Finalizers: []string{v1alpha1.TeamFinalizerTenantPlacement},
		},
		Spec: v1alpha1.TeamSpec{Namespaces: []string{"payments-ns"}},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{
				Cluster: "core", Cell: "cell-gone", CellNamespace: "cells-ns", Generation: 3,
			},
			Pipelines: &v1alpha1.TeamPipelinesStatus{
				WebhookToken: "tok", WebhookSecret: "sec", AgentNamespace: "payments-ns",
			},
		},
	}
	base.teams["payments"] = team
	crdstore.MustSeed(base.store, team)

	client := &podReaperClient{
		testClient: base,
		pods:       []corev1.Pod{agentPod("payments-default-abc", "varroa-agent-payments", true, time.Now())},
	}
	rec := NewTeamReconciler(client, base.store, teamTestNS, slog.Default())
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
	if !teamHoldsPlacementFinalizer(t, base) {
		t.Fatal("the finalizer was released while an agent pod remained")
	}
	status := lastTeamStatus(base)
	if status == nil || status.Pipelines == nil || status.Pipelines.AgentNamespace != "payments-ns" {
		t.Fatalf("the agent namespace was not kept for the next tick: %+v", status)
	}
	if got := placedReasonOf(status); got != v1alpha1.TeamReasonTenantTeardownPending {
		t.Fatalf("Placed reason = %q, want TenantTeardownPending", got)
	}

	// Tick two: no pod remains, so teardown completes.
	if _, err := rec.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if teamHoldsPlacementFinalizer(t, base) {
		t.Fatal("the finalizer was not released once the agent pods were gone")
	}
}
