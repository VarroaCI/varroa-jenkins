package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestDownloadURLProber_Verdicts(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   downloadProbeVerdict
	}{
		{name: "404 is not found", status: http.StatusNotFound, want: downloadProbeNotFound},
		{name: "410 is not found", status: http.StatusGone, want: downloadProbeNotFound},
		{name: "200 is reachable", status: http.StatusOK, want: downloadProbeReachable},
		{name: "206 is reachable", status: http.StatusPartialContent, want: downloadProbeReachable},
		{name: "403 gives no verdict", status: http.StatusForbidden, want: downloadProbeNoVerdict},
		{name: "500 gives no verdict", status: http.StatusInternalServerError, want: downloadProbeNoVerdict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath, gotRange, gotMethod string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotRange, gotMethod = r.URL.Path, r.Header.Get("Range"), r.Method
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()
			p := newDownloadURLProber()
			res := p.Probe(context.Background(), srv.URL+"/download/plugins/", "git", "5.2.0")
			if res.Verdict != tt.want {
				t.Fatalf("verdict = %v, want %v", res.Verdict, tt.want)
			}
			if gotMethod != http.MethodGet || gotRange != "bytes=0-0" {
				t.Errorf("request = %s Range=%q, want GET bytes=0-0", gotMethod, gotRange)
			}
			if gotPath != "/download/plugins/git/5.2.0/git.hpi" {
				t.Errorf("path = %q", gotPath)
			}
			if res.URL != srv.URL+"/download/plugins/git/5.2.0/git.hpi" {
				t.Errorf("URL = %q", res.URL)
			}
		})
	}
}

func TestDownloadURLProber_TimeoutGivesNoVerdict(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	p := newDownloadURLProber()
	p.client.Timeout = 50 * time.Millisecond
	res := p.Probe(context.Background(), srv.URL, "git", "5.2.0")
	if res.Verdict != downloadProbeNoVerdict {
		t.Fatalf("verdict = %v, want no verdict", res.Verdict)
	}
}

func TestDownloadURLProber_NetworkErrorGivesNoVerdict(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	res := newDownloadURLProber().Probe(context.Background(), url, "git", "5.2.0")
	if res.Verdict != downloadProbeNoVerdict {
		t.Fatalf("verdict = %v, want no verdict", res.Verdict)
	}
}

func TestDownloadURLProber_CachesPerURL(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	now := time.Unix(1000, 0)
	p := newDownloadURLProber()
	p.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if res := p.Probe(context.Background(), srv.URL, "git", "5.2.0"); res.Verdict != downloadProbeNotFound {
			t.Fatalf("verdict = %v", res.Verdict)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("server hits = %d, want 1 within the cache window", n)
	}
	p.Probe(context.Background(), srv.URL, "credentials", "1.0")
	if n := hits.Load(); n != 2 {
		t.Fatalf("server hits = %d, want 2 for a second pin", n)
	}
	now = now.Add(downloadProbeTTL + time.Second)
	p.Probe(context.Background(), srv.URL, "git", "5.2.0")
	if n := hits.Load(); n != 3 {
		t.Fatalf("server hits = %d, want 3 after the cache expires", n)
	}
}

func TestFirstVersionedPin(t *testing.T) {
	name, version, ok := firstVersionedPin([]string{"", "workflow-aggregator", "git:5.2.0", "credentials:1.0"})
	if !ok || name != "git" || version != "5.2.0" {
		t.Fatalf("got %q %q %v", name, version, ok)
	}
	if _, _, ok := firstVersionedPin([]string{"git"}); ok {
		t.Fatal("ok = true for a list with no version")
	}
}

func TestApplyDownloadURLProbe(t *testing.T) {
	cond := func(status metav1.ConditionStatus, reason string) []v1alpha1.ControllerCondition {
		return []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionUpdateCenterFallback, Status: status, Reason: reason}}
	}
	notFound := downloadProbeResult{Verdict: downloadProbeNotFound, URL: "https://m.example.org/download/git/5.2.0/git.hpi", Plugin: "git:5.2.0", Status: 404}
	got := findCondition(applyDownloadURLProbe(nil, notFound), v1alpha1.ConditionUpdateCenterFallback)
	if got == nil || got.Status != metav1.ConditionTrue || got.Reason != v1alpha1.ReasonDownloadURLNotFound {
		t.Fatalf("not found: condition = %+v", got)
	}
	if !strings.Contains(got.Message, notFound.URL) || !strings.Contains(got.Message, "/download/plugins") {
		t.Errorf("message %q must name the URL and the expected shape", got.Message)
	}

	reachable := downloadProbeResult{Verdict: downloadProbeReachable}
	got = findCondition(applyDownloadURLProbe(cond(metav1.ConditionTrue, v1alpha1.ReasonDownloadURLNotFound), reachable), v1alpha1.ConditionUpdateCenterFallback)
	if got.Status != metav1.ConditionFalse || got.Reason != v1alpha1.ReasonDownloadURLReachable {
		t.Fatalf("reachable after not found: condition = %s/%s", got.Status, got.Reason)
	}
	got = findCondition(applyDownloadURLProbe(cond(metav1.ConditionTrue, v1alpha1.ReasonPluginArchiveFallback), reachable), v1alpha1.ConditionUpdateCenterFallback)
	if got.Reason != v1alpha1.ReasonPluginArchiveFallback {
		t.Fatalf("reachable must keep the plugins-init verdict, got %s", got.Reason)
	}
	got = findCondition(applyDownloadURLProbe(cond(metav1.ConditionTrue, v1alpha1.ReasonDownloadURLNotFound), downloadProbeResult{}), v1alpha1.ConditionUpdateCenterFallback)
	if got.Reason != v1alpha1.ReasonDownloadURLNotFound {
		t.Fatalf("no verdict must write nothing, got %s", got.Reason)
	}
}

func explicitDownloadDefaults() *v1alpha1.ProvisioningDefaults {
	return &v1alpha1.ProvisioningDefaults{Spec: v1alpha1.ProvisioningDefaultsSpec{
		PluginUpdateCenterDownloadURL: "https://mirror.example.org/download",
	}}
}

func readyUpdateCenter() *v1alpha1.UpdateCenter {
	return &v1alpha1.UpdateCenter{
		ObjectMeta: metav1.ObjectMeta{Name: "varroa-update-center"},
		Status: v1alpha1.UpdateCenterStatus{Conditions: []v1alpha1.UpdateCenterCondition{
			{Type: "Ready", Status: metav1.ConditionTrue},
		}},
	}
}

func TestHandleProvisioning_ExplicitDownloadURLNotFound(t *testing.T) {
	client := newTestClientWithBundle()
	client.provisioningDefaults = explicitDownloadDefaults()
	client.updateCenter = readyUpdateCenter()
	rec := newTestReconciler(client)
	rec.SetUpdateCenterBaseURL("http://uc.svc:8080")
	var probed []string
	rec.downloadProbe = func(_ context.Context, base, name, version string) downloadProbeResult {
		probed = append(probed, base+" "+name+":"+version)
		return downloadProbeResult{Verdict: downloadProbeNotFound, URL: base + "/" + name, Plugin: name + ":" + version, Status: 404}
	}
	cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)

	_ = rec.handleProvisioning(context.Background(), cr)

	if len(probed) != 1 || !strings.HasPrefix(probed[0], "https://mirror.example.org/download ") {
		t.Fatalf("probed = %v, want one check of the explicit URL", probed)
	}
	c := findCondition(cr.Status.Conditions, v1alpha1.ConditionUpdateCenterFallback)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != v1alpha1.ReasonDownloadURLNotFound {
		t.Fatalf("UpdateCenterFallback = %+v, want True/DownloadURLNotFound", c)
	}
	if len(client.stsSpecs) == 0 {
		t.Fatal("the check must not block the StatefulSet")
	}
}

func TestHandleProvisioning_ExplicitDownloadURLNoVerdictKeepsGateSilent(t *testing.T) {
	client := newTestClientWithBundle()
	client.provisioningDefaults = explicitDownloadDefaults()
	client.updateCenter = readyUpdateCenter()
	rec := newTestReconciler(client)
	rec.SetUpdateCenterBaseURL("http://uc.svc:8080")
	cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)

	_ = rec.handleProvisioning(context.Background(), cr)

	if c := findCondition(cr.Status.Conditions, v1alpha1.ConditionUpdateCenterFallback); c != nil {
		t.Fatalf("UpdateCenterFallback = %s/%s, want no claim for an explicit download URL", c.Status, c.Reason)
	}
}

func TestHandleProvisioning_TierTwoDownloadURLIsNotChecked(t *testing.T) {
	client := newTestClientWithBundle()
	client.updateCenter = readyUpdateCenter()
	rec := newTestReconciler(client)
	rec.SetUpdateCenterBaseURL("http://uc.svc:8080")
	rec.downloadProbe = func(context.Context, string, string, string) downloadProbeResult {
		t.Error("a tier-2 download URL must not be checked")
		return downloadProbeResult{}
	}
	cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)

	_ = rec.handleProvisioning(context.Background(), cr)

	c := findCondition(cr.Status.Conditions, v1alpha1.ConditionUpdateCenterFallback)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "UpdateCenterAvailable" {
		t.Fatalf("UpdateCenterFallback = %+v, want False/UpdateCenterAvailable", c)
	}
}

func TestHandleProvisioning_PluginsInitFallbackMarkerRaisesCondition(t *testing.T) {
	client := newTestClientWithBundle()
	client.updateCenter = readyUpdateCenter()
	client.controllerPod = podWithPluginsInit(corev1.ContainerStatus{
		State: corev1.ContainerState{Terminated: terminated(0, fallbackMarker)},
	})
	rec := newTestReconciler(client)
	rec.SetUpdateCenterBaseURL("http://uc.svc:8080")
	cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)

	_ = rec.handleProvisioning(context.Background(), cr)

	c := findCondition(cr.Status.Conditions, v1alpha1.ConditionUpdateCenterFallback)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != v1alpha1.ReasonPluginArchiveFallback {
		t.Fatalf("UpdateCenterFallback = %+v, want True/PluginArchiveFallback", c)
	}
}

func TestHandleProvisioning_ExplicitDownloadURLClearsStaleGateFallback(t *testing.T) {
	client := newTestClientWithBundle()
	client.provisioningDefaults = explicitDownloadDefaults()
	client.updateCenter = readyUpdateCenter()
	rec := newTestReconciler(client)
	rec.SetUpdateCenterBaseURL("http://uc.svc:8080")
	cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)
	cr.Status.Conditions = []v1alpha1.ControllerCondition{{
		Type: v1alpha1.ConditionUpdateCenterFallback, Status: metav1.ConditionTrue, Reason: "UpdateCenterUnavailable",
	}}

	_ = rec.handleProvisioning(context.Background(), cr)

	c := findCondition(cr.Status.Conditions, v1alpha1.ConditionUpdateCenterFallback)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "UpdateCenterAvailable" {
		t.Fatalf("UpdateCenterFallback = %+v, want the stale gate verdict cleared", c)
	}
}

func TestHandleProvisioning_ExplicitDownloadURLKeepsOwnVerdict(t *testing.T) {
	client := newTestClientWithBundle()
	client.provisioningDefaults = explicitDownloadDefaults()
	client.updateCenter = readyUpdateCenter()
	rec := newTestReconciler(client)
	rec.SetUpdateCenterBaseURL("http://uc.svc:8080")
	cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)
	cr.Status.Conditions = []v1alpha1.ControllerCondition{{
		Type: v1alpha1.ConditionUpdateCenterFallback, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonPluginArchiveFallback,
	}}

	_ = rec.handleProvisioning(context.Background(), cr)

	c := findCondition(cr.Status.Conditions, v1alpha1.ConditionUpdateCenterFallback)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != v1alpha1.ReasonPluginArchiveFallback {
		t.Fatalf("UpdateCenterFallback = %+v, want the plugins-init verdict kept", c)
	}
}
