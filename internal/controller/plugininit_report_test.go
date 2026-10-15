package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

const (
	notFoundMarker = "varroa:download-url-not-found url=https://uc.example.com/download/git/5.2.0/git.hpi plugin=git:5.2.0 status=404"
	fallbackMarker = "varroa:archive-fallback count=3 plugin=git url=https://uc.example.com/download/git/5.2.0/git.hpi"
)

func podWithPluginsInit(s corev1.ContainerStatus) *corev1.Pod {
	s.Name = "plugins-init"
	return &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{s}}}
}

func terminated(code int32, msg string) *corev1.ContainerStateTerminated {
	return &corev1.ContainerStateTerminated{ExitCode: code, Message: msg, Reason: "Error"}
}

func TestPluginInitUCReport(t *testing.T) {
	tests := []struct {
		name       string
		status     corev1.ContainerStatus
		wantKind   pluginInitUCKind
		wantPlugin string
		wantCount  int
	}{
		{
			name:     "no init container status gives no verdict",
			wantKind: pluginInitUCNone,
		},
		{
			name:     "running first attempt gives no verdict",
			status:   corev1.ContainerStatus{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			wantKind: pluginInitUCNone,
		},
		{
			name:       "current termination exit 42 with not-found marker",
			status:     corev1.ContainerStatus{State: corev1.ContainerState{Terminated: terminated(42, notFoundMarker)}},
			wantKind:   pluginInitUCNotFound,
			wantPlugin: "git:5.2.0",
		},
		{
			name: "last termination exit 42 after a restart",
			status: corev1.ContainerStatus{
				RestartCount:         2,
				State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				LastTerminationState: corev1.ContainerState{Terminated: terminated(42, notFoundMarker)},
			},
			wantKind:   pluginInitUCNotFound,
			wantPlugin: "git:5.2.0",
		},
		{
			name: "last termination while waiting in back-off",
			status: corev1.ContainerStatus{
				RestartCount:         1,
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: terminated(42, notFoundMarker)},
			},
			wantKind:   pluginInitUCNotFound,
			wantPlugin: "git:5.2.0",
		},
		{
			name:       "exit 0 with fallback marker",
			status:     corev1.ContainerStatus{State: corev1.ContainerState{Terminated: terminated(0, fallbackMarker+"\n")}},
			wantKind:   pluginInitUCArchiveFallback,
			wantPlugin: "git",
			wantCount:  3,
		},
		{
			name:       "exit 1 with fallback marker",
			status:     corev1.ContainerStatus{State: corev1.ContainerState{Terminated: terminated(1, fallbackMarker)}},
			wantKind:   pluginInitUCArchiveFallback,
			wantPlugin: "git",
			wantCount:  3,
		},
		{
			name:     "clean exit 0",
			status:   corev1.ContainerStatus{State: corev1.ContainerState{Terminated: terminated(0, "")}},
			wantKind: pluginInitUCClean,
		},
		{
			name:     "failure without marker gives no verdict",
			status:   corev1.ContainerStatus{State: corev1.ContainerState{Terminated: terminated(1, "")}},
			wantKind: pluginInitUCNone,
		},
		{
			name:     "exit 42 without marker gives no verdict",
			status:   corev1.ContainerStatus{State: corev1.ContainerState{Terminated: terminated(42, "")}},
			wantKind: pluginInitUCNone,
		},
		{
			name: "current clean exit wins over old failure history",
			status: corev1.ContainerStatus{
				RestartCount:         1,
				State:                corev1.ContainerState{Terminated: terminated(0, "")},
				LastTerminationState: corev1.ContainerState{Terminated: terminated(42, notFoundMarker)},
			},
			wantKind: pluginInitUCClean,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{}
			if tt.status.State != (corev1.ContainerState{}) || tt.status.RestartCount > 0 {
				pod = podWithPluginsInit(tt.status)
			}
			got := pluginInitUCReport(pod)
			if got.Kind != tt.wantKind {
				t.Fatalf("kind = %v, want %v", got.Kind, tt.wantKind)
			}
			if got.Plugin != tt.wantPlugin {
				t.Errorf("plugin = %q, want %q", got.Plugin, tt.wantPlugin)
			}
			if got.Count != tt.wantCount {
				t.Errorf("count = %d, want %d", got.Count, tt.wantCount)
			}
		})
	}
}

func TestApplyPluginInitUCReport(t *testing.T) {
	cond := func(status metav1.ConditionStatus, reason string) []v1alpha1.ControllerCondition {
		return []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionUpdateCenterFallback, Status: status, Reason: reason}}
	}
	tests := []struct {
		name       string
		conds      []v1alpha1.ControllerCondition
		report     pluginInitUCResult
		wantStatus metav1.ConditionStatus
		wantReason string
		wantMsg    string
	}{
		{
			name:       "not found raises the condition",
			report:     pluginInitUCResult{Kind: pluginInitUCNotFound, URL: "https://uc.example.com/download/git/5.2.0/git.hpi", Plugin: "git:5.2.0", Status: "404"},
			wantStatus: metav1.ConditionTrue,
			wantReason: v1alpha1.ReasonDownloadURLNotFound,
			wantMsg:    "/download/plugins",
		},
		{
			name:       "archive fallback overrides an available reason",
			conds:      cond(metav1.ConditionFalse, "UpdateCenterAvailable"),
			report:     pluginInitUCResult{Kind: pluginInitUCArchiveFallback, URL: "https://uc.example.com/download/git/5.2.0/git.hpi", Plugin: "git", Count: 3},
			wantStatus: metav1.ConditionTrue,
			wantReason: v1alpha1.ReasonPluginArchiveFallback,
			wantMsg:    "3 plugin",
		},
		{
			name:       "clean exit clears its own reason",
			conds:      cond(metav1.ConditionTrue, v1alpha1.ReasonPluginArchiveFallback),
			report:     pluginInitUCResult{Kind: pluginInitUCClean},
			wantStatus: metav1.ConditionFalse,
			wantReason: v1alpha1.ReasonPluginDownloadsVerified,
		},
		{
			name:       "clean exit clears a not-found verdict",
			conds:      cond(metav1.ConditionTrue, v1alpha1.ReasonDownloadURLNotFound),
			report:     pluginInitUCResult{Kind: pluginInitUCClean},
			wantStatus: metav1.ConditionFalse,
			wantReason: v1alpha1.ReasonPluginDownloadsVerified,
		},
		{
			name:       "clean exit keeps the update-center gate reason",
			conds:      cond(metav1.ConditionTrue, "UpdateCenterUnavailable"),
			report:     pluginInitUCResult{Kind: pluginInitUCClean},
			wantStatus: metav1.ConditionTrue,
			wantReason: "UpdateCenterUnavailable",
		},
		{
			name:       "no verdict writes nothing",
			conds:      cond(metav1.ConditionTrue, v1alpha1.ReasonDownloadURLNotFound),
			report:     pluginInitUCResult{Kind: pluginInitUCNone},
			wantStatus: metav1.ConditionTrue,
			wantReason: v1alpha1.ReasonDownloadURLNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := applyPluginInitUCReport(tt.conds, tt.report)
			c := findCondition(got, v1alpha1.ConditionUpdateCenterFallback)
			if c == nil {
				t.Fatal("UpdateCenterFallback not set")
			}
			if c.Status != tt.wantStatus || c.Reason != tt.wantReason {
				t.Fatalf("condition = %s/%s, want %s/%s", c.Status, c.Reason, tt.wantStatus, tt.wantReason)
			}
			if !strings.Contains(c.Message, tt.wantMsg) {
				t.Errorf("message %q does not contain %q", c.Message, tt.wantMsg)
			}
		})
	}
}

func TestApplyPluginInitUCReport_CleanWithoutConditionWritesNothing(t *testing.T) {
	got := applyPluginInitUCReport(nil, pluginInitUCResult{Kind: pluginInitUCClean})
	if c := findCondition(got, v1alpha1.ConditionUpdateCenterFallback); c != nil {
		t.Fatalf("condition = %+v, want none", c)
	}
}

func TestPluginInitFailureMessage_RestartedContainerReadsLastTermination(t *testing.T) {
	pod := podWithPluginsInit(corev1.ContainerStatus{
		RestartCount:         1,
		State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		LastTerminationState: corev1.ContainerState{Terminated: terminated(1, "")},
	})
	msg, failed := pluginInitFailureMessage(pod)
	if !failed {
		t.Fatal("failed = false, want true for a restarted plugins-init")
	}
	if !strings.Contains(msg, "exited with code 1") || !strings.Contains(msg, "1 restarts") {
		t.Fatalf("message = %q", msg)
	}
}

func TestPluginInitFailureMessage_MarkerIsReadable(t *testing.T) {
	pod := podWithPluginsInit(corev1.ContainerStatus{State: corev1.ContainerState{Terminated: terminated(42, notFoundMarker)}})
	msg, failed := pluginInitFailureMessage(pod)
	if !failed {
		t.Fatal("failed = false, want true")
	}
	if strings.HasPrefix(msg, "varroa:") || !strings.Contains(msg, "git:5.2.0") {
		t.Fatalf("message = %q, want readable text that names the plugin", msg)
	}
}

func TestPluginInitFailureMessage_RecoveredContainerIgnoresHistory(t *testing.T) {
	pod := podWithPluginsInit(corev1.ContainerStatus{
		RestartCount:         1,
		State:                corev1.ContainerState{Terminated: terminated(0, "")},
		LastTerminationState: corev1.ContainerState{Terminated: terminated(1, "")},
	})
	if _, failed := pluginInitFailureMessage(pod); failed {
		t.Fatal("failed = true, want false for a recovered plugins-init")
	}
}
