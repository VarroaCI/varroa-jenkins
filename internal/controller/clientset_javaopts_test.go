package controller

import (
	"strings"
	"testing"
)

// TestStatefulSet_JavaOpts_UpstreamOffAddsNeverFlags verifies that the
// usage-statistics flag is always present and that the update-site and
// download-service flags appear only when the upstream host is off.
func TestStatefulSet_JavaOpts_UpstreamOffAddsNeverFlags(t *testing.T) {
	const usage = "-Dhudson.model.UsageStatistics.disabled=true"
	const never = "-Dhudson.model.UpdateCenter.never=true -Dhudson.model.DownloadService.never=true"

	on := buildStatefulSet(StatefulSetSpec{Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci"})
	onOpts := builtContainerEnv(t, on, "jenkins")["JAVA_OPTS"]
	if !strings.Contains(onOpts, usage) {
		t.Errorf("upstream on: JAVA_OPTS missing usage-statistics flag, got: %q", onOpts)
	}
	if strings.Contains(onOpts, "UpdateCenter.never") || strings.Contains(onOpts, "DownloadService.never") {
		t.Errorf("upstream on: JAVA_OPTS must not carry the never flags, got: %q", onOpts)
	}

	off := buildStatefulSet(StatefulSetSpec{Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci", JenkinsUpstreamOff: true})
	offOpts := builtContainerEnv(t, off, "jenkins")["JAVA_OPTS"]
	if !strings.Contains(offOpts, usage) || !strings.Contains(offOpts, never) {
		t.Errorf("upstream off: JAVA_OPTS = %q, want usage flag and %q", offOpts, never)
	}
}
