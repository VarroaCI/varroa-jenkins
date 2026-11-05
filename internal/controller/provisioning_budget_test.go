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
		name       string
		pod        *corev1.Pod
		pullStarts map[string]time.Time
		wantWait   time.Duration
		wantPull   *imagePull
	}{
		{
			name: "first init container pulls now",
			pod: &corev1.Pod{
				Spec:   corev1.PodSpec{InitContainers: initSpec},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{waitingStatus("plugins-init", "PodInitializing")}},
			},
			pullStarts: map[string]time.Time{"plugins-init": since.Add(5 * time.Second)},
			wantWait:   8*time.Minute - 5*time.Second,
			wantPull:   &imagePull{Container: "plugins-init", Image: "jenkins/jenkins:2.528"},
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
			pullStarts: map[string]time.Time{
				"plugins-init": since,
				"init-groovy":  since.Add(2 * time.Minute),
			},
			wantWait: 2 * time.Minute,
		},
		{
			name: "ImagePullBackOff earns no credit",
			pod: &corev1.Pod{
				Spec:   corev1.PodSpec{InitContainers: initSpec},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{waitingStatus("plugins-init", "ImagePullBackOff")}},
			},
			pullStarts: map[string]time.Time{"plugins-init": since.Add(5 * time.Second)},
			wantWait:   0,
		},
		{
			name: "window that starts before the budget is clipped",
			pod: &corev1.Pod{
				Spec:   corev1.PodSpec{InitContainers: initSpec},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{terminatedStatus("plugins-init", since.Add(time.Minute))}},
			},
			pullStarts: map[string]time.Time{"plugins-init": since.Add(-2 * time.Minute)},
			wantWait:   time.Minute,
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
			pullStarts: map[string]time.Time{"plugins-init": since},
			wantWait:   0,
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
			pullStarts: map[string]time.Time{"plugins-init": since},
			wantWait:   0,
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
			pullStarts: map[string]time.Time{"plugins-init": since.Add(2 * time.Minute)},
			wantWait:   6 * time.Minute,
			wantPull:   &imagePull{Container: "plugins-init", Image: "jenkins/jenkins:2.528"},
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
			pullStarts: map[string]time.Time{"plugins-init": since},
			wantWait:   time.Minute,
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
			pullStarts: map[string]time.Time{"mite": since.Add(2 * time.Minute)},
			wantWait:   6 * time.Minute,
			wantPull:   &imagePull{Container: "mite", Image: "mite:2"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wait, pull := imagePullWait(tc.pod, tc.pullStarts, since, now)
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

func TestPullStartsByContainer(t *testing.T) {
	base := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	events := []corev1.Event{
		{Reason: "Pulling", InvolvedObject: corev1.ObjectReference{FieldPath: "spec.initContainers{plugins-init}"},
			LastTimestamp: metav1.NewTime(base.Add(time.Minute))},
		{Reason: "Pulling", InvolvedObject: corev1.ObjectReference{FieldPath: "spec.initContainers{plugins-init}"},
			LastTimestamp: metav1.NewTime(base.Add(2 * time.Minute))},
		{Reason: "Pulling", InvolvedObject: corev1.ObjectReference{FieldPath: "spec.containers{mite}"},
			EventTime: metav1.NewMicroTime(base.Add(3 * time.Minute))},
		{Reason: "Pulling", InvolvedObject: corev1.ObjectReference{FieldPath: "spec.initContainers{"}, LastTimestamp: metav1.NewTime(base)},
		{Reason: "Pulling", InvolvedObject: corev1.ObjectReference{FieldPath: "spec.containers{}"}, LastTimestamp: metav1.NewTime(base)},
		{Reason: "Pulling", InvolvedObject: corev1.ObjectReference{FieldPath: "pod"}, LastTimestamp: metav1.NewTime(base)},
		{Reason: "Pulled", InvolvedObject: corev1.ObjectReference{FieldPath: "spec.containers{jenkins}"}, LastTimestamp: metav1.NewTime(base)},
		{Reason: "Pulling", InvolvedObject: corev1.ObjectReference{FieldPath: "spec.containers{casc}"}},
	}
	got := pullStartsByContainer(events)
	if len(got) != 2 {
		t.Fatalf("got %d containers, want 2: %v", len(got), got)
	}
	if !got["plugins-init"].Equal(base.Add(2 * time.Minute)) {
		t.Errorf("plugins-init = %v, want the latest event", got["plugins-init"])
	}
	if !got["mite"].Equal(base.Add(3 * time.Minute)) {
		t.Errorf("mite = %v, want the EventTime fallback", got["mite"])
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

func TestProvisioningBudgetExceeded(t *testing.T) {
	tests := []struct {
		name string
		b    provisioningBudget
		want bool
	}{
		{"six minutes without a pull", provisioningBudget{Elapsed: 6 * time.Minute}, true},
		{"eight minutes with four minutes of pull", provisioningBudget{Elapsed: 8 * time.Minute, PullWait: 4 * time.Minute}, false},
		{"twenty-one minutes with a long pull", provisioningBudget{Elapsed: 21 * time.Minute, PullWait: 18 * time.Minute}, true},
		{"four minutes", provisioningBudget{Elapsed: 4 * time.Minute}, false},
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
	tests := []struct {
		name string
		b    provisioningBudget
		want string
	}{
		{
			name: "hard cap while a pull runs",
			b:    provisioningBudget{Elapsed: 21 * time.Minute, PullWait: 18 * time.Minute, Pull: &imagePull{Container: "mite", Image: "mite:2"}},
			want: "provisioning exceeded 20m0s; container mite still pulls image mite:2",
		},
		{
			name: "hard cap without a pull",
			b:    provisioningBudget{Elapsed: 21 * time.Minute},
			want: "provisioning exceeded 20m0s",
		},
		{
			name: "budget with pull credit",
			b:    provisioningBudget{Elapsed: 8 * time.Minute, PullWait: 4*time.Minute + 300*time.Millisecond},
			want: "provisioning exceeded 5m0s, not counting 4m0s of image pulls",
		},
		{
			name: "plain budget",
			b:    provisioningBudget{Elapsed: 6 * time.Minute},
			want: "provisioning exceeded 5m0s",
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

func TestListPodPullEventsSelectsPodAndReason(t *testing.T) {
	cs := fake.NewClientset()
	var gotNS, gotFields string
	cs.PrependReactor("list", "events", func(action clienttesting.Action) (bool, runtime.Object, error) {
		gotNS = action.GetNamespace()
		la := action.(clienttesting.ListAction)
		gotFields = la.GetListRestrictions().Fields.String()
		return true, &corev1.EventList{}, nil
	})
	c := &ClientsetClient{clientset: cs}
	uid := types.UID("pod-uid-1")
	if _, err := c.ListPodPullEvents(context.Background(), "ns", uid); err != nil {
		t.Fatalf("ListPodPullEvents: %v", err)
	}
	if gotNS != "ns" {
		t.Errorf("namespace = %q, want ns", gotNS)
	}
	for _, want := range []string{"involvedObject.uid=pod-uid-1", "reason=Pulling"} {
		if !strings.Contains(gotFields, want) {
			t.Errorf("field selector %q lacks %q", gotFields, want)
		}
	}
}
