package controller

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// The marker prefixes and the exit code are a contract with pluginsInitScript,
// which writes them to the plugins-init termination log.
const (
	markerDownloadURLNotFound = "varroa:download-url-not-found"
	markerArchiveFallback     = "varroa:archive-fallback"
	exitDownloadURLNotFound   = 42
	pluginArchiveBaseURL      = "https://archives.jenkins.io/"
	downloadURLShapeHint      = "the download URL must be <mirror>/download/plugins, for example https://updates.jenkins.io/download/plugins"
)

// pluginInitUCKind is the verdict pluginInitUCReport reads from plugins-init.
type pluginInitUCKind int

const (
	// pluginInitUCNone gives no verdict: no termination record, or a failure
	// with no marker. The condition stays as it is.
	pluginInitUCNone pluginInitUCKind = iota
	// pluginInitUCClean is exit 0 with no marker.
	pluginInitUCClean
	// pluginInitUCNotFound is exit 42 with the download-url-not-found marker.
	pluginInitUCNotFound
	// pluginInitUCArchiveFallback is the archive-fallback marker with any
	// exit code other than 42.
	pluginInitUCArchiveFallback
)

func (k pluginInitUCKind) String() string {
	switch k {
	case pluginInitUCClean:
		return "clean"
	case pluginInitUCNotFound:
		return "not-found"
	case pluginInitUCArchiveFallback:
		return "archive-fallback"
	default:
		return "none"
	}
}

// pluginInitUCResult is the parsed plugins-init report.
type pluginInitUCResult struct {
	Kind   pluginInitUCKind
	URL    string
	Plugin string
	Status string
	Count  int
}

// parsePluginInitMarker reads the first marker line of a termination message.
// It returns the marker name and its key=value fields.
func parsePluginInitMarker(msg string) (string, map[string]string) {
	for _, line := range strings.Split(msg, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "varroa:") {
			continue
		}
		parts := strings.Fields(line)
		fields := make(map[string]string, len(parts)-1)
		for _, p := range parts[1:] {
			if k, v, ok := strings.Cut(p, "="); ok {
				fields[k] = v
			}
		}
		return parts[0], fields
	}
	return "", nil
}

// pluginsInitTermination returns the termination record that describes the
// newest finished run of plugins-init. The current state wins. While the
// container runs or waits again after a restart, the last termination is the
// newest record.
func pluginsInitTermination(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	if pod == nil {
		return nil
	}
	for _, s := range pod.Status.InitContainerStatuses {
		if s.Name != "plugins-init" {
			continue
		}
		if s.State.Terminated != nil {
			return s.State.Terminated
		}
		if s.RestartCount > 0 {
			return s.LastTerminationState.Terminated
		}
		return nil
	}
	return nil
}

// pluginInitUCReport reads the download report of plugins-init from its
// termination message.
func pluginInitUCReport(pod *corev1.Pod) pluginInitUCResult {
	t := pluginsInitTermination(pod)
	if t == nil {
		return pluginInitUCResult{}
	}
	name, fields := parsePluginInitMarker(t.Message)
	switch {
	case t.ExitCode == exitDownloadURLNotFound && name == markerDownloadURLNotFound:
		return pluginInitUCResult{Kind: pluginInitUCNotFound, URL: fields["url"], Plugin: fields["plugin"], Status: fields["status"]}
	case t.ExitCode != exitDownloadURLNotFound && name == markerArchiveFallback:
		count, _ := strconv.Atoi(fields["count"])
		return pluginInitUCResult{Kind: pluginInitUCArchiveFallback, URL: fields["url"], Plugin: fields["plugin"], Count: count}
	case t.ExitCode == 0 && name == "":
		return pluginInitUCResult{Kind: pluginInitUCClean}
	}
	return pluginInitUCResult{}
}

// describePluginInitMarker turns a marker line into readable text. It
// returns false when msg carries no known marker.
func describePluginInitMarker(msg string) (string, bool) {
	name, fields := parsePluginInitMarker(msg)
	switch name {
	case markerDownloadURLNotFound:
		return downloadURLNotFoundMessage("plugins-init", fields["url"], fields["plugin"], fields["status"]), true
	case markerArchiveFallback:
		count, _ := strconv.Atoi(fields["count"])
		return archiveFallbackMessage(count, fields["plugin"], fields["url"]), true
	}
	return "", false
}

func downloadURLNotFoundMessage(source, url, plugin, status string) string {
	return fmt.Sprintf("%s: %s answered %s for plugin %s; %s", source, url, status, plugin, downloadURLShapeHint)
}

func archiveFallbackMessage(count int, plugin, url string) string {
	return fmt.Sprintf("plugins-init fetched %d plugin(s) from %s instead of the configured download URL; the first was %s, which failed at %s",
		count, pluginArchiveBaseURL, plugin, url)
}

// ucFallbackOwnReason reports whether reason belongs to the plugins-init
// report or to the download-URL check. Those writers clear only their own
// reasons, so a clean run never hides the update-center gate verdict.
func ucFallbackOwnReason(reason string) bool {
	return reason == v1alpha1.ReasonDownloadURLNotFound || reason == v1alpha1.ReasonPluginArchiveFallback
}

// applyPluginInitUCReport writes the UpdateCenterFallback verdict of a
// plugins-init report.
func applyPluginInitUCReport(conds []v1alpha1.ControllerCondition, rep pluginInitUCResult) []v1alpha1.ControllerCondition {
	switch rep.Kind {
	case pluginInitUCNotFound:
		return setCondition(conds, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionUpdateCenterFallback,
			Status:  metav1.ConditionTrue,
			Reason:  v1alpha1.ReasonDownloadURLNotFound,
			Message: downloadURLNotFoundMessage("plugins-init", rep.URL, rep.Plugin, rep.Status),
		})
	case pluginInitUCArchiveFallback:
		return setCondition(conds, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionUpdateCenterFallback,
			Status:  metav1.ConditionTrue,
			Reason:  v1alpha1.ReasonPluginArchiveFallback,
			Message: archiveFallbackMessage(rep.Count, rep.Plugin, rep.URL),
		})
	case pluginInitUCClean:
		if c := findCondition(conds, v1alpha1.ConditionUpdateCenterFallback); c != nil && c.Status == metav1.ConditionTrue && ucFallbackOwnReason(c.Reason) {
			return setCondition(conds, v1alpha1.ControllerCondition{
				Type:    v1alpha1.ConditionUpdateCenterFallback,
				Status:  metav1.ConditionFalse,
				Reason:  v1alpha1.ReasonPluginDownloadsVerified,
				Message: "plugins-init downloaded every plugin from the configured source",
			})
		}
	}
	return conds
}
