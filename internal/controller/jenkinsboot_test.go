package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/overlay"
)

func TestJenkinsBootFailureMessage(t *testing.T) {
	jenkins := func(cs corev1.ContainerStatus) *corev1.Pod {
		cs.Name = overlay.JenkinsContainerName
		return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{cs}}}
	}
	cases := []struct {
		name   string
		pod    *corev1.Pod
		want   string
		failed bool
	}{
		{"crash loop", jenkins(corev1.ContainerStatus{
			RestartCount:         283,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 5, Reason: "Error"}},
		}), "jenkins container exited with code 5 (283 restarts): CrashLoopBackOff: back-off 5m0s", true},
		// LastTerminationState is kept for the life of the pod, so readiness is
		// what separates "still re-booting" from "crashed once, long since
		// recovered".
		{"restarted, running but not yet ready", jenkins(corev1.ContainerStatus{
			RestartCount:         1,
			Ready:                false,
			State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 5, Reason: "Error"}},
		}), "jenkins container exited with code 5 (1 restarts): Error", true},
		{"crashed once, now ready", jenkins(corev1.ContainerStatus{
			RestartCount:         1,
			Ready:                true,
			State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 5, Reason: "Error"}},
		}), "", false},
		{"image pull", jenkins(corev1.ContainerStatus{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "manifest unknown"}},
		}), "ImagePullBackOff: manifest unknown", true},
		{"healthy", jenkins(corev1.ContainerStatus{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}), "", false},
		{"still creating", jenkins(corev1.ContainerStatus{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}), "", false},
		{"no jenkins container", &corev1.Pod{}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, failed := jenkinsBootFailureMessage(tc.pod)
			if failed != tc.failed || got != tc.want {
				t.Fatalf("got (%q,%v) want (%q,%v)", got, failed, tc.want, tc.failed)
			}
		})
	}
}

func TestPluginVersionConflict_MessageIncludesRemediation(t *testing.T) {
	coreSet := []pluginlock.PluginEntry{{ArtifactID: "kubernetes", Version: "4547.v52f3080db_8cd"}}
	bundlePinned := &bundle.MaterializedBundle{PluginsYAML: "plugins:\n  - artifactId: kubernetes\n    version: 4540.v612369217f87\n"}
	prefix := "plugin kubernetes requested at 4540.v612369217f87 conflicts with profile lock 4547.v52f3080db_8cd; "
	suffix := " to 4547.v52f3080db_8cd (the JenkinsVersionProfile lock for this Jenkins version) or drop the pin to defer to the lock"

	cases := []struct {
		name     string
		cr       *v1alpha1.Controller
		resolved *bundle.MaterializedBundle
		want     string
	}{
		{
			// The bundle holds the pin, so that is what the operator edits.
			name:     "bundle pin",
			cr:       &v1alpha1.Controller{},
			resolved: bundlePinned,
			want:     prefix + "pin the bundle" + suffix,
		},
		{
			// spec.pluginSpec outranks the bundle in nonCorePluginEntries, so
			// the message must not send the operator to the bundle.
			name: "spec.pluginSpec pin outranks the bundle",
			cr: &v1alpha1.Controller{Spec: v1alpha1.ControllerSpec{
				PluginSpec: &v1alpha1.PluginSpec{Entries: []v1alpha1.PluginEntry{
					{ArtifactId: "kubernetes", Version: "4540.v612369217f87"},
				}},
			}},
			resolved: bundlePinned,
			want:     prefix + "pin spec.pluginSpec" + suffix,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if msg := pluginVersionConflict(tc.cr, tc.resolved, coreSet); msg != tc.want {
				t.Fatalf("got %q\nwant %q", msg, tc.want)
			}
		})
	}
}

func TestReconcileJenkinsBootFailure(t *testing.T) {
	crashLoopPod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:                 overlay.JenkinsContainerName,
		RestartCount:         283,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 5, Reason: "Error"}},
	}}}}
	readyPod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:  overlay.JenkinsContainerName,
		Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}}}
	startingPod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:  overlay.JenkinsContainerName,
		Ready: false,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}}}

	cases := []struct {
		name       string
		pod        *corev1.Pod
		wantStatus metav1.ConditionStatus
		wantReason string
		wantMsg    string
	}{
		{"crash loop", crashLoopPod, metav1.ConditionTrue, v1alpha1.ReasonJenkinsBootFailed,
			"jenkins container exited with code 5 (283 restarts): CrashLoopBackOff: back-off 5m0s"},
		// Not-failing splits by readiness: a Ready container has booted, an
		// unready one has not booted yet.
		{"ready", readyPod, metav1.ConditionFalse, "Booted", ""},
		{"still starting", startingPod, metav1.ConditionFalse, "BootPending", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClientWithBundle()
			client.controllerPod = tc.pod
			rec := newTestReconciler(client)
			cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)

			rec.reconcileJenkinsBootFailure(context.Background(), cr)

			cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionJenkinsBootFailed)
			if cond == nil {
				t.Fatal("ConditionJenkinsBootFailed absent")
			}
			if cond.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", cond.Status, tc.wantStatus)
			}
			if cond.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", cond.Reason, tc.wantReason)
			}
			if cond.Message != tc.wantMsg {
				t.Errorf("Message = %q, want %q", cond.Message, tc.wantMsg)
			}
		})
	}
}

// TestReconcile_StampsLastReconciledAt asserts the top-level Reconcile stamps
// LastReconciledAt on a successful pass in a non-Connected phase, and leaves it
// alone when the pass errors out — BroodVerbReconcile treats a fresh stamp as
// proof the pass succeeded.
func TestReconcile_StampsLastReconciledAt(t *testing.T) {
	t.Run("success stamps", func(t *testing.T) {
		client := newTestClientWithBundle()
		cr := testController("test-ctrl", "test-ns", v1alpha1.ControllerPhaseProvisioning)
		client.controllers = []*v1alpha1.Controller{cr}
		crdstore.MustSeed(client.store, cr)
		rec := newTestReconciler(client)

		req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "test-ns", Name: "test-ctrl"}}
		if _, err := rec.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if client.lastPatchedStatus == nil {
			t.Fatal("no status patched")
		}
		if client.lastPatchedStatus.LastReconciledAt == nil {
			t.Fatal("LastReconciledAt not stamped on a successful pass")
		}
	})

	t.Run("blocked pass does not stamp", func(t *testing.T) {
		client := newTestClientWithBundle()
		cr := testController("test-ctrl", "test-ns", v1alpha1.ControllerPhase("Bogus"))
		client.controllers = []*v1alpha1.Controller{cr}
		crdstore.MustSeed(client.store, cr)
		rec := newTestReconciler(client)

		req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "test-ns", Name: "test-ctrl"}}
		if _, err := rec.Reconcile(context.Background(), req); err == nil {
			t.Fatal("expected an error from a blocked reconcile pass")
		}
		if client.lastPatchedStatus != nil && client.lastPatchedStatus.LastReconciledAt != nil {
			t.Fatal("LastReconciledAt stamped on a blocked pass")
		}
	})
}

// TestReconcileConnectedJenkinsBoot covers the Connected-phase gate: a
// connected mite is a sibling container, so it is not proof that Jenkins
// booted. Only the mite's health verdict is, and the pod is fetched solely
// when that verdict does not vouch for Jenkins.
func TestReconcileConnectedJenkinsBoot(t *testing.T) {
	crashLoopPod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:                 overlay.JenkinsContainerName,
		RestartCount:         283,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 5, Reason: "Error"}},
	}}}}
	const crashMsg = "jenkins container exited with code 5 (283 restarts): CrashLoopBackOff: back-off 5m0s"

	t.Run("mite reports unhealthy: crash-looping Jenkins is surfaced", func(t *testing.T) {
		client := newTestClientWithBundle()
		client.controllerPod = crashLoopPod
		rec := newTestReconciler(client)
		cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
		cr.Status.MiteStatus = &v1alpha1.MiteStatus{Connected: true, JenkinsHealth: "unhealthy"}

		rec.reconcileConnectedJenkinsBoot(context.Background(), cr)

		cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionJenkinsBootFailed)
		if cond == nil {
			t.Fatal("ConditionJenkinsBootFailed absent")
		}
		if cond.Status != metav1.ConditionTrue {
			t.Errorf("Status = %q, want True", cond.Status)
		}
		if cond.Reason != v1alpha1.ReasonJenkinsBootFailed {
			t.Errorf("Reason = %q, want %q", cond.Reason, v1alpha1.ReasonJenkinsBootFailed)
		}
		if cond.Message != crashMsg {
			t.Errorf("Message = %q, want %q", cond.Message, crashMsg)
		}
	})

	t.Run("mite reports healthy: cleared without fetching the pod", func(t *testing.T) {
		client := newTestClientWithBundle()
		client.controllerPod = crashLoopPod // must not be consulted
		rec := newTestReconciler(client)
		cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
		cr.Status.MiteStatus = &v1alpha1.MiteStatus{Connected: true, JenkinsHealth: "healthy"}

		rec.reconcileConnectedJenkinsBoot(context.Background(), cr)

		cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionJenkinsBootFailed)
		if cond == nil {
			t.Fatal("ConditionJenkinsBootFailed absent")
		}
		if cond.Status != metav1.ConditionFalse || cond.Reason != "Booted" {
			t.Errorf("got %q/%q, want False/Booted", cond.Status, cond.Reason)
		}
		if client.getControllerPodCalls != 0 {
			t.Errorf("fetched the pod %d times on a healthy Connected tick, want 0", client.getControllerPodCalls)
		}
	})

	t.Run("Running to Connected does not erase a True while Jenkins is unhealthy", func(t *testing.T) {
		client := newTestClientWithBundle()
		client.controllerPod = crashLoopPod
		rec := newTestReconciler(client)
		cr := testController("test", "ns1", v1alpha1.ControllerPhaseRunning)
		// handleRunning flagged the crash loop before the mite connected.
		rec.reconcileJenkinsBootFailure(context.Background(), cr)
		before := findCondition(cr.Status.Conditions, v1alpha1.ConditionJenkinsBootFailed)
		if before == nil || before.Status != metav1.ConditionTrue {
			t.Fatalf("setup: want True in Running, got %+v", before)
		}

		// The mite connects while Jenkins is still crash-looping.
		cr.Status.Phase = v1alpha1.ControllerPhaseConnected
		cr.Status.MiteStatus = &v1alpha1.MiteStatus{Connected: true, JenkinsHealth: "unreachable"}
		rec.reconcileConnectedJenkinsBoot(context.Background(), cr)

		cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionJenkinsBootFailed)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			t.Fatalf("Connected tick erased the boot failure: got %+v", cond)
		}
		if cond.Message != crashMsg {
			t.Errorf("Message = %q, want %q", cond.Message, crashMsg)
		}
	})
}

// TestReconcileConnectedJenkinsBoot_CachedHealthyNeverClearsTrue pins the
// asymmetry: MiteStatus.JenkinsHealth is a cached verdict that lags the probe
// interval and survives ticks carrying no snapshot, so it may keep the
// condition False but must never retire a recorded boot failure on its own.
func TestReconcileConnectedJenkinsBoot_CachedHealthyNeverClearsTrue(t *testing.T) {
	crashLoopPod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:                 overlay.JenkinsContainerName,
		RestartCount:         283,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 5, Reason: "Error"}},
	}}}}
	// A pod that crashed, restarted and has since gone Ready. Kubernetes keeps
	// LastTerminationState for the life of the pod, so the recovery is only
	// visible in Ready.
	recoveredPod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:                 overlay.JenkinsContainerName,
		RestartCount:         283,
		Ready:                true,
		State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 5, Reason: "Error"}},
	}}}}

	// Both cases enter the tick with a boot failure already recorded and a
	// stale snapshot still claiming the process is fine.
	setup := func(pod *corev1.Pod) (*testClient, *Reconciler, *v1alpha1.Controller) {
		client := newTestClientWithBundle()
		client.controllerPod = pod
		rec := newTestReconciler(client)
		cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
		cr.Status.MiteStatus = &v1alpha1.MiteStatus{Connected: true, JenkinsHealth: "healthy"}
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionJenkinsBootFailed,
			Status:  metav1.ConditionTrue,
			Reason:  v1alpha1.ReasonJenkinsBootFailed,
			Message: "jenkins container exited with code 5 (283 restarts): CrashLoopBackOff: back-off 5m0s",
		})
		return client, rec, cr
	}

	t.Run("still crash-looping: stays True", func(t *testing.T) {
		client, rec, cr := setup(crashLoopPod)

		rec.reconcileConnectedJenkinsBoot(context.Background(), cr)

		cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionJenkinsBootFailed)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			t.Fatalf("a cached healthy snapshot cleared a live crash loop: got %+v", cond)
		}
		if client.getControllerPodCalls != 1 {
			t.Errorf("GetControllerPod calls = %d, want 1", client.getControllerPodCalls)
		}
	})

	t.Run("pod recovered: clears to False", func(t *testing.T) {
		_, rec, cr := setup(recoveredPod)

		rec.reconcileConnectedJenkinsBoot(context.Background(), cr)

		cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionJenkinsBootFailed)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Fatalf("a recovered pod did not clear the boot failure: got %+v", cond)
		}
		// A Ready container has booted; the reason must not read as pending.
		if cond.Reason != "Booted" {
			t.Errorf("Reason = %q, want %q", cond.Reason, "Booted")
		}
	})
}
