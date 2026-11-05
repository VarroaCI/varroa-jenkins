package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func waitingStatus(name, reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:  name,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
	}
}

func terminatedStatus(name string, startedAt time.Time) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name: name,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode:  0,
			StartedAt: metav1.NewTime(startedAt),
		}},
	}
}

func TestImagePullWait(t *testing.T) {
	since := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	now := since.Add(8 * time.Minute)
	initSpec := []corev1.Container{
		{Name: "plugins-init", Image: "jenkins/jenkins:2.528"},
		{Name: "init-groovy", Image: "mite:1"},
	}

	tests := []struct {
		name     string
		pod      *corev1.Pod
		pulls    map[string]pullWindow
		wantWait time.Duration
		wantPull *imagePull
	}{
		{
			name: "first init container pulls now",
			pod: &corev1.Pod{
				Spec:   corev1.PodSpec{InitContainers: initSpec},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{waitingStatus("plugins-init", "PodInitializing")}},
			},
			pulls:    map[string]pullWindow{"plugins-init": {Start: since.Add(5 * time.Second)}},
			wantWait: 8*time.Minute - 5*time.Second,
			wantPull: &imagePull{Container: "plugins-init", Image: "jenkins/jenkins:2.528"},
		},
		{
			name: "create wait without a Pulling event is not a pull",
			pod: &corev1.Pod{
				Spec:   corev1.PodSpec{InitContainers: initSpec},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{waitingStatus("plugins-init", "PodInitializing")}},
			},
			wantWait: 0,
		},
		{
			name: "finished pulls of two init containers add up",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{InitContainers: initSpec},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
					terminatedStatus("plugins-init", since.Add(time.Minute)),
					terminatedStatus("init-groovy", since.Add(3*time.Minute)),
				}},
			},
			pulls: map[string]pullWindow{
				"plugins-init": {Start: since},
				"init-groovy":  {Start: since.Add(2 * time.Minute)},
			},
			wantWait: 2 * time.Minute,
		},
		{
			name: "ImagePullBackOff earns no credit",
			pod: &corev1.Pod{
				Spec:   corev1.PodSpec{InitContainers: initSpec},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{waitingStatus("plugins-init", "ImagePullBackOff")}},
			},
			pulls:    map[string]pullWindow{"plugins-init": {Start: since.Add(5 * time.Second)}},
			wantWait: 0,
		},
		{
			name: "window that starts before the budget is clipped",
			pod: &corev1.Pod{
				Spec:   corev1.PodSpec{InitContainers: initSpec},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{terminatedStatus("plugins-init", since.Add(time.Minute))}},
			},
			pulls:    map[string]pullWindow{"plugins-init": {Start: since.Add(-2 * time.Minute)}},
			wantWait: time.Minute,
		},
		{
			name: "restarted container earns no credit",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{InitContainers: initSpec},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{func() corev1.ContainerStatus {
					s := waitingStatus("plugins-init", "PodInitializing")
					s.RestartCount = 1
					return s
				}()}},
			},
			pulls:    map[string]pullWindow{"plugins-init": {Start: since}},
			wantWait: 0,
		},
		{
			name: "finished pull of a restarted container earns no credit",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{InitContainers: initSpec},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{func() corev1.ContainerStatus {
					s := terminatedStatus("plugins-init", since.Add(time.Minute))
					s.RestartCount = 1
					return s
				}()}},
			},
			pulls:    map[string]pullWindow{"plugins-init": {Start: since}},
			wantWait: 0,
		},
		{
			name: "restarted container that pulls again after its last exit is credited",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{InitContainers: initSpec},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{func() corev1.ContainerStatus {
					s := waitingStatus("plugins-init", "PodInitializing")
					s.RestartCount = 1
					s.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode:   1,
						FinishedAt: metav1.NewTime(since.Add(time.Minute)),
					}}
					return s
				}()}},
			},
			pulls:    map[string]pullWindow{"plugins-init": {Start: since.Add(2 * time.Minute)}},
			wantWait: 6 * time.Minute,
			wantPull: &imagePull{Container: "plugins-init", Image: "jenkins/jenkins:2.528"},
		},
		{
			name: "pull before a restart is credited up to the previous run start",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{InitContainers: initSpec},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{func() corev1.ContainerStatus {
					s := waitingStatus("plugins-init", "CrashLoopBackOff")
					s.RestartCount = 1
					s.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode:   1,
						StartedAt:  metav1.NewTime(since.Add(time.Minute)),
						FinishedAt: metav1.NewTime(since.Add(2 * time.Minute)),
					}}
					return s
				}()}},
			},
			pulls:    map[string]pullWindow{"plugins-init": {Start: since}},
			wantWait: time.Minute,
		},
		{
			name: "main container pulls after the init containers finish",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: initSpec,
					Containers:     []corev1.Container{{Name: "mite", Image: "mite:2"}},
				},
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						terminatedStatus("plugins-init", since),
						terminatedStatus("init-groovy", since.Add(time.Minute)),
					},
					ContainerStatuses: []corev1.ContainerStatus{waitingStatus("mite", "ContainerCreating")},
				},
			},
			pulls:    map[string]pullWindow{"mite": {Start: since.Add(2 * time.Minute)}},
			wantWait: 6 * time.Minute,
			wantPull: &imagePull{Container: "mite", Image: "mite:2"},
		},
		{
			name: "create wait after a Pulled event earns credit up to the Pulled event",
			pod: &corev1.Pod{
				Spec:   corev1.PodSpec{InitContainers: initSpec},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{waitingStatus("plugins-init", "PodInitializing")}},
			},
			pulls:    map[string]pullWindow{"plugins-init": {Start: since.Add(5 * time.Second), End: since.Add(2 * time.Minute)}},
			wantWait: 2*time.Minute - 5*time.Second,
		},
		{
			name: "Pulled event in the Pulling second ends the pull",
			pod: &corev1.Pod{
				Spec:   corev1.PodSpec{InitContainers: initSpec},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{waitingStatus("plugins-init", "PodInitializing")}},
			},
			pulls:    map[string]pullWindow{"plugins-init": {Start: since.Add(time.Minute), End: since.Add(time.Minute)}},
			wantWait: 0,
		},
		{
			name: "Pulled event before the container start caps the credit",
			pod: &corev1.Pod{
				Spec:   corev1.PodSpec{InitContainers: initSpec},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{terminatedStatus("plugins-init", since.Add(3*time.Minute))}},
			},
			pulls:    map[string]pullWindow{"plugins-init": {Start: since, End: since.Add(time.Minute)}},
			wantWait: time.Minute,
		},
		{
			name: "Pulled event after the container start keeps the start as the end",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{InitContainers: initSpec},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
					Name:  "plugins-init",
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(since.Add(time.Minute))}},
				}}},
			},
			pulls:    map[string]pullWindow{"plugins-init": {Start: since, End: since.Add(2 * time.Minute)}},
			wantWait: time.Minute,
		},
		{
			name: "pull before one restart ends at its Pulled event",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{InitContainers: initSpec},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{func() corev1.ContainerStatus {
					s := waitingStatus("plugins-init", "CrashLoopBackOff")
					s.RestartCount = 1
					s.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode:   1,
						StartedAt:  metav1.NewTime(since.Add(3 * time.Minute)),
						FinishedAt: metav1.NewTime(since.Add(4 * time.Minute)),
					}}
					return s
				}()}},
			},
			pulls:    map[string]pullWindow{"plugins-init": {Start: since, End: since.Add(time.Minute)}},
			wantWait: time.Minute,
		},
		{
			name: "main container that waits after its Pulled event names no pull",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: initSpec,
					Containers:     []corev1.Container{{Name: "mite", Image: "mite:2"}},
				},
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						terminatedStatus("plugins-init", since),
						terminatedStatus("init-groovy", since.Add(time.Minute)),
					},
					ContainerStatuses: []corev1.ContainerStatus{waitingStatus("mite", "ContainerCreating")},
				},
			},
			pulls:    map[string]pullWindow{"mite": {Start: since.Add(2 * time.Minute), End: since.Add(3 * time.Minute)}},
			wantWait: time.Minute,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wait, pull := imagePullWait(tc.pod, tc.pulls, since, now)
			if wait != tc.wantWait {
				t.Errorf("wait = %v, want %v", wait, tc.wantWait)
			}
			if (pull == nil) != (tc.wantPull == nil) {
				t.Fatalf("pull = %+v, want %+v", pull, tc.wantPull)
			}
			if pull != nil && *pull != *tc.wantPull {
				t.Errorf("pull = %+v, want %+v", *pull, *tc.wantPull)
			}
		})
	}
}

func TestPullWindowsByContainer(t *testing.T) {
	base := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	initPlugins := "spec.initContainers{plugins-init}"
	events := []corev1.Event{
		{Reason: "Pulling", InvolvedObject: corev1.ObjectReference{FieldPath: initPlugins},
			LastTimestamp: metav1.NewTime(base.Add(time.Minute))},
		{Reason: "Pulling", InvolvedObject: corev1.ObjectReference{FieldPath: initPlugins},
			LastTimestamp: metav1.NewTime(base.Add(2 * time.Minute))},
		{Reason: "Pulled", InvolvedObject: corev1.ObjectReference{FieldPath: initPlugins},
			LastTimestamp: metav1.NewTime(base.Add(90 * time.Second))},
		{Reason: "Pulled", InvolvedObject: corev1.ObjectReference{FieldPath: initPlugins},
			LastTimestamp: metav1.NewTime(base.Add(5 * time.Minute))},
		{Reason: "Pulled", InvolvedObject: corev1.ObjectReference{FieldPath: initPlugins},
			LastTimestamp: metav1.NewTime(base.Add(4 * time.Minute))},
		{Reason: "Created", InvolvedObject: corev1.ObjectReference{FieldPath: initPlugins},
			LastTimestamp: metav1.NewTime(base.Add(3 * time.Minute))},
		{Reason: "Pulled", InvolvedObject: corev1.ObjectReference{FieldPath: initPlugins}},
		{Reason: "Pulling", InvolvedObject: corev1.ObjectReference{FieldPath: "spec.containers{mite}"},
			EventTime: metav1.NewMicroTime(base.Add(3 * time.Minute))},
		{Reason: "Pulled", InvolvedObject: corev1.ObjectReference{FieldPath: "spec.containers{mite}"},
			LastTimestamp: metav1.NewTime(base.Add(time.Minute))},
		{Reason: "Pulling", InvolvedObject: corev1.ObjectReference{FieldPath: "spec.initContainers{init-groovy}"},
			LastTimestamp: metav1.NewTime(base.Add(6 * time.Minute))},
		{Reason: "Pulled", InvolvedObject: corev1.ObjectReference{FieldPath: "spec.initContainers{init-groovy}"},
			LastTimestamp: metav1.NewTime(base.Add(6 * time.Minute))},
		{Reason: "Pulled", InvolvedObject: corev1.ObjectReference{FieldPath: "spec.containers{jenkins}"},
			LastTimestamp: metav1.NewTime(base)},
		{Reason: "Pulling", InvolvedObject: corev1.ObjectReference{FieldPath: "spec.initContainers{"}, LastTimestamp: metav1.NewTime(base)},
		{Reason: "Pulling", InvolvedObject: corev1.ObjectReference{FieldPath: "spec.containers{}"}, LastTimestamp: metav1.NewTime(base)},
		{Reason: "Pulling", InvolvedObject: corev1.ObjectReference{FieldPath: "pod"}, LastTimestamp: metav1.NewTime(base)},
		{Reason: "Pulling", InvolvedObject: corev1.ObjectReference{FieldPath: "spec.containers{casc}"}},
	}
	got := pullWindowsByContainer(events)
	if len(got) != 3 {
		t.Fatalf("got %d containers, want 3: %v", len(got), got)
	}
	want := map[string]pullWindow{
		"plugins-init": {Start: base.Add(2 * time.Minute), End: base.Add(4 * time.Minute)},
		"mite":         {Start: base.Add(3 * time.Minute)},
		"init-groovy":  {Start: base.Add(6 * time.Minute), End: base.Add(6 * time.Minute)},
	}
	for name, w := range want {
		if !got[name].Start.Equal(w.Start) || !got[name].End.Equal(w.End) {
			t.Errorf("%s = %+v, want %+v", name, got[name], w)
		}
	}
	if _, ok := got["jenkins"]; ok {
		t.Errorf("jenkins has only a Pulled event and must have no window")
	}
}

func TestNextStartWaiter(t *testing.T) {
	sidecar := corev1.ContainerRestartPolicyAlways
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "running init container",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{InitContainers: []corev1.Container{{Name: "plugins-init"}}},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
					Name:  "plugins-init",
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}}},
			},
			want: "",
		},
		{
			name: "first init container waits",
			pod: &corev1.Pod{
				Spec:   corev1.PodSpec{InitContainers: []corev1.Container{{Name: "plugins-init"}, {Name: "init-groovy"}}},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{waitingStatus("plugins-init", "PodInitializing")}},
			},
			want: "plugins-init",
		},
		{
			name: "all init containers done and main waits",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "plugins-init"}},
					Containers:     []corev1.Container{{Name: "mite"}},
				},
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{terminatedStatus("plugins-init", time.Now())},
					ContainerStatuses:     []corev1.ContainerStatus{waitingStatus("mite", "ContainerCreating")},
				},
			},
			want: "mite",
		},
		{
			name: "running sidecar init container is skipped",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{InitContainers: []corev1.Container{
					{Name: "sidecar", RestartPolicy: &sidecar},
					{Name: "init-groovy"},
				}},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
					{Name: "sidecar", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
					waitingStatus("init-groovy", "PodInitializing"),
				}},
			},
			want: "init-groovy",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextStartWaiter(tc.pod); got != tc.want {
				t.Errorf("nextStartWaiter = %q, want %q", got, tc.want)
			}
		})
	}
}

// pluginInitStatus returns the status of the plugins-init container.
func pluginInitStatus(state corev1.ContainerState, restarts int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: pluginsInitName, State: state, RestartCount: restarts}
}

func TestPluginDownloadWait(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	since := now.Add(-8 * time.Minute)
	at := func(d time.Duration) metav1.Time { return metav1.NewTime(now.Add(d)) }
	tests := []struct {
		name        string
		statuses    []corev1.ContainerStatus
		wantWait    time.Duration
		wantRunning bool
	}{
		{
			name: "running since one minute in",
			statuses: []corev1.ContainerStatus{pluginInitStatus(
				corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: at(-7 * time.Minute)}}, 0)},
			wantWait: 7 * time.Minute, wantRunning: true,
		},
		{
			name: "exited zero after three minutes",
			statuses: []corev1.ContainerStatus{pluginInitStatus(corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0, StartedAt: at(-7 * time.Minute), FinishedAt: at(-4 * time.Minute)}}, 0)},
			wantWait: 3 * time.Minute, wantRunning: false,
		},
		{
			name: "exited non-zero earns no credit",
			statuses: []corev1.ContainerStatus{pluginInitStatus(corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, StartedAt: at(-7 * time.Minute), FinishedAt: at(-4 * time.Minute)}}, 0)},
			wantWait: 0, wantRunning: false,
		},
		{
			name: "restarted run earns no credit",
			statuses: []corev1.ContainerStatus{pluginInitStatus(
				corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: at(-7 * time.Minute)}}, 1)},
			wantWait: 0, wantRunning: false,
		},
		{
			name: "waiting to start earns no credit",
			statuses: []corev1.ContainerStatus{pluginInitStatus(
				corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}, 0)},
			wantWait: 0, wantRunning: false,
		},
		{
			name: "run started before the window is clipped to it",
			statuses: []corev1.ContainerStatus{pluginInitStatus(
				corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: at(-10 * time.Minute)}}, 0)},
			wantWait: 8 * time.Minute, wantRunning: true,
		},
		{
			name: "exit before the window earns no credit",
			statuses: []corev1.ContainerStatus{pluginInitStatus(corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0, StartedAt: at(-10 * time.Minute), FinishedAt: at(-9 * time.Minute)}}, 0)},
			wantWait: 0, wantRunning: false,
		},
		{
			name: "zero start time earns credit while running",
			statuses: []corev1.ContainerStatus{pluginInitStatus(
				corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, 0)},
			wantWait: 0, wantRunning: true,
		},
		{
			name:     "no plugins-init status",
			statuses: []corev1.ContainerStatus{{Name: "other", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: at(-7 * time.Minute)}}}},
			wantWait: 0, wantRunning: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: tc.statuses}}
			wait, running := pluginDownloadWait(pod, since, now)
			if wait != tc.wantWait || running != tc.wantRunning {
				t.Errorf("pluginDownloadWait = (%v, %v), want (%v, %v)", wait, running, tc.wantWait, tc.wantRunning)
			}
		})
	}
}

func TestProvisioningBudgetForReadsDownloadInsideBase(t *testing.T) {
	rec := newTestReconciler(newTestClientWithBundle())
	now := time.Now()
	started := metav1.NewTime(now.Add(-3 * time.Minute))
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID},
		Status:     v1alpha1.ControllerStatus{ProvisioningStartedAt: &started},
	}
	pod := provisionDownloadPod(now.Add(-2*time.Minute), 0)

	b := rec.provisioningBudgetFor(context.Background(), cr, pod, provisioningLimitsFrom(nil), now)
	if !b.Downloading {
		t.Error("Downloading = false, want true while plugins-init runs")
	}
	if b.DownloadWait != 2*time.Minute {
		t.Errorf("DownloadWait = %v, want 2m0s", b.DownloadWait)
	}
}

func TestSetProvisioningStepCondition(t *testing.T) {
	pull := &imagePull{Container: "plugins-init", Image: "jenkins/jenkins:2.528"}
	find := func(cr *v1alpha1.Controller) *v1alpha1.ControllerCondition {
		return findCondition(cr.Status.Conditions, v1alpha1.ConditionProvisioningStep)
	}

	cr := &v1alpha1.Controller{}
	setProvisioningStepCondition(cr, provisioningBudget{Pull: pull})
	if c := find(cr); c == nil || c.Status != metav1.ConditionTrue || c.Reason != v1alpha1.ReasonImagePullInProgress {
		t.Fatalf("pull: ProvisioningStep = %+v, want True with ImagePullInProgress", c)
	}

	cr = &v1alpha1.Controller{}
	setProvisioningStepCondition(cr, provisioningBudget{Downloading: true})
	c := find(cr)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != v1alpha1.ReasonPluginDownloadInProgress {
		t.Fatalf("download: ProvisioningStep = %+v, want True with PluginDownloadInProgress", c)
	}
	if !strings.Contains(c.Message, pluginsInitName) {
		t.Errorf("download message = %q, want the container name", c.Message)
	}

	cr = &v1alpha1.Controller{}
	setProvisioningStepCondition(cr, provisioningBudget{Pull: pull, Downloading: true})
	if c := find(cr); c == nil || c.Reason != v1alpha1.ReasonImagePullInProgress {
		t.Fatalf("both: ProvisioningStep = %+v, want the pull to win", c)
	}

	setProvisioningStepCondition(cr, provisioningBudget{})
	if c := find(cr); c != nil {
		t.Errorf("none: ProvisioningStep = %+v, want removed", c)
	}
}

func TestProvisioningBudgetExceeded(t *testing.T) {
	def := provisioningLimitsFrom(nil)
	long := provisioningLimitsFrom(&v1alpha1.ProvisioningDefaults{Spec: v1alpha1.ProvisioningDefaultsSpec{ProvisioningTimeoutSec: 900}})
	tests := []struct {
		name string
		b    provisioningBudget
		want bool
	}{
		{"six minutes without a pull", provisioningBudget{Elapsed: 6 * time.Minute, Limits: def}, true},
		{"eight minutes with four minutes of pull", provisioningBudget{Elapsed: 8 * time.Minute, PullWait: 4 * time.Minute, Limits: def}, false},
		{"twenty-one minutes with a long pull", provisioningBudget{Elapsed: 21 * time.Minute, PullWait: 18 * time.Minute, Limits: def}, true},
		{"four minutes", provisioningBudget{Elapsed: 4 * time.Minute, Limits: def}, false},
		{"six minutes within a 900-second budget", provisioningBudget{Elapsed: 6 * time.Minute, Limits: long}, false},
		{"sixteen minutes past a 900-second budget", provisioningBudget{Elapsed: 16 * time.Minute, Limits: long}, true},
		{"twenty-five minutes with eighteen minutes of pull", provisioningBudget{Elapsed: 25 * time.Minute, PullWait: 18 * time.Minute, Limits: long}, false},
		{"thirty-one minutes with twenty-five minutes of pull", provisioningBudget{Elapsed: 31 * time.Minute, PullWait: 25 * time.Minute, Limits: long}, true},
		{"eight minutes with four minutes of download", provisioningBudget{Elapsed: 8 * time.Minute, DownloadWait: 4 * time.Minute, Limits: def}, false},
		{"nine minutes with a pull and a download", provisioningBudget{Elapsed: 9 * time.Minute, PullWait: 2 * time.Minute, DownloadWait: 3 * time.Minute, Limits: def}, false},
		{"twenty-one minutes with a long download", provisioningBudget{Elapsed: 21 * time.Minute, DownloadWait: 18 * time.Minute, Limits: def}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.b.exceeded(); got != tc.want {
				t.Errorf("exceeded() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProvisioningBudgetFailureMessage(t *testing.T) {
	def := provisioningLimitsFrom(nil)
	long := provisioningLimitsFrom(&v1alpha1.ProvisioningDefaults{Spec: v1alpha1.ProvisioningDefaultsSpec{ProvisioningTimeoutSec: 900}})
	tests := []struct {
		name string
		b    provisioningBudget
		want string
	}{
		{
			name: "hard cap while a pull runs",
			b:    provisioningBudget{Elapsed: 21 * time.Minute, PullWait: 18 * time.Minute, Pull: &imagePull{Container: "mite", Image: "mite:2"}, Limits: def},
			want: "provisioning exceeded 20m0s; container mite still pulls image mite:2",
		},
		{
			name: "hard cap without a pull",
			b:    provisioningBudget{Elapsed: 21 * time.Minute, Limits: def},
			want: "provisioning exceeded 20m0s",
		},
		{
			name: "budget with pull credit",
			b:    provisioningBudget{Elapsed: 8 * time.Minute, PullWait: 4*time.Minute + 300*time.Millisecond, Limits: def},
			want: "provisioning exceeded 5m0s, not counting 4m0s of image pulls",
		},
		{
			name: "plain budget",
			b:    provisioningBudget{Elapsed: 6 * time.Minute, Limits: def},
			want: "provisioning exceeded 5m0s",
		},
		{
			name: "configured base budget",
			b:    provisioningBudget{Elapsed: 11 * time.Minute, Limits: provisioningLimitsFrom(&v1alpha1.ProvisioningDefaults{Spec: v1alpha1.ProvisioningDefaultsSpec{ProvisioningTimeoutSec: 600}})},
			want: "provisioning exceeded 10m0s",
		},
		{
			name: "hard cap while a download runs",
			b:    provisioningBudget{Elapsed: 21 * time.Minute, DownloadWait: 18 * time.Minute, Downloading: true, Limits: def},
			want: "provisioning exceeded 20m0s; container plugins-init still downloads plugins",
		},
		{
			name: "budget with download credit",
			b:    provisioningBudget{Elapsed: 8 * time.Minute, DownloadWait: 2 * time.Minute, Limits: def},
			want: "provisioning exceeded 5m0s, not counting 2m0s of plugin downloads",
		},
		{
			name: "budget with pull and download credit",
			b:    provisioningBudget{Elapsed: 9 * time.Minute, PullWait: time.Minute, DownloadWait: 2 * time.Minute, Limits: def},
			want: "provisioning exceeded 5m0s, not counting 1m0s of image pulls and 2m0s of plugin downloads",
		},
		{
			name: "configured hard cap",
			b:    provisioningBudget{Elapsed: 31 * time.Minute, Limits: long},
			want: "provisioning exceeded 30m0s",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.b.failureMessage(); got != tc.want {
				t.Errorf("failureMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProvisioningLimitsFrom(t *testing.T) {
	sec := func(n int) *v1alpha1.ProvisioningDefaults {
		return &v1alpha1.ProvisioningDefaults{Spec: v1alpha1.ProvisioningDefaultsSpec{ProvisioningTimeoutSec: n}}
	}
	tests := []struct {
		name     string
		defaults *v1alpha1.ProvisioningDefaults
		want     provisioningLimits
	}{
		{"nil defaults", nil, provisioningLimits{Base: 5 * time.Minute, Hard: 20 * time.Minute}},
		{"unset", sec(0), provisioningLimits{Base: 5 * time.Minute, Hard: 20 * time.Minute}},
		{"negative", sec(-5), provisioningLimits{Base: 5 * time.Minute, Hard: 20 * time.Minute}},
		{"minimum", sec(60), provisioningLimits{Base: time.Minute, Hard: 16 * time.Minute}},
		{"configured", sec(900), provisioningLimits{Base: 15 * time.Minute, Hard: 30 * time.Minute}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := provisioningLimitsFrom(tc.defaults); got != tc.want {
				t.Errorf("provisioningLimitsFrom() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestListPodPullEventsKeepsPullReasons(t *testing.T) {
	cs := fake.NewClientset()
	var gotNS, gotFields string
	cs.PrependReactor("list", "events", func(action clienttesting.Action) (bool, runtime.Object, error) {
		gotNS = action.GetNamespace()
		la := action.(clienttesting.ListAction)
		gotFields = la.GetListRestrictions().Fields.String()
		return true, &corev1.EventList{Items: []corev1.Event{
			{Reason: "Scheduled"},
			{Reason: "Pulling"},
			{Reason: "Pulled"},
			{Reason: "Created"},
			{Reason: "Started"},
		}}, nil
	})
	c := &ClientsetClient{clientset: cs}
	uid := types.UID("pod-uid-1")
	events, err := c.ListPodPullEvents(context.Background(), "ns", uid)
	if err != nil {
		t.Fatalf("ListPodPullEvents: %v", err)
	}
	if gotNS != "ns" {
		t.Errorf("namespace = %q, want ns", gotNS)
	}
	if want := "involvedObject.uid=pod-uid-1"; gotFields != want {
		t.Errorf("field selector = %q, want %q", gotFields, want)
	}
	if len(events) != 2 || events[0].Reason != "Pulling" || events[1].Reason != "Pulled" {
		t.Fatalf("events = %+v, want the Pulling and Pulled events", events)
	}
}
