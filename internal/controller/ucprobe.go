package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

const (
	downloadProbeTimeout = 5 * time.Second
	// downloadProbeTTL bounds the cost of an unreachable URL to one timeout
	// per window, because a provisioning tick runs the check on each pass.
	downloadProbeTTL = 10 * time.Minute
)

type downloadProbeVerdict int

const (
	// downloadProbeNoVerdict covers a network error, a timeout, and any status
	// other than 200, 206, 404, or 410. The operator can reach a different
	// network than the pod, so these must not change the condition.
	downloadProbeNoVerdict downloadProbeVerdict = iota
	downloadProbeReachable
	downloadProbeNotFound
)

func (v downloadProbeVerdict) String() string {
	switch v {
	case downloadProbeReachable:
		return "reachable"
	case downloadProbeNotFound:
		return "not-found"
	default:
		return "no-verdict"
	}
}

type downloadProbeResult struct {
	Verdict downloadProbeVerdict
	URL     string
	Plugin  string
	Status  int
}

type downloadProbeEntry struct {
	result  downloadProbeResult
	expires time.Time
}

// downloadURLProber checks that an explicit plugin download URL serves a
// pinned plugin. Reconciles run concurrently, so the cache is guarded.
type downloadURLProber struct {
	client *http.Client
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]downloadProbeEntry
}

func newDownloadURLProber() *downloadURLProber {
	return &downloadURLProber{
		client: &http.Client{Timeout: downloadProbeTimeout},
		now:    time.Now,
		cache:  make(map[string]downloadProbeEntry),
	}
}

// Probe fetches one byte of <base>/<name>/<version>/<name>.hpi, the URL
// jenkins-plugin-cli builds from JENKINS_UC_DOWNLOAD_URL. It sends a ranged
// GET, not HEAD: the Varroa update center answers 405 to HEAD.
func (p *downloadURLProber) Probe(ctx context.Context, base, name, version string) downloadProbeResult {
	url := strings.TrimSuffix(base, "/") + "/" + name + "/" + version + "/" + name + ".hpi"
	now := p.now()
	p.mu.Lock()
	if e, ok := p.cache[url]; ok && now.Before(e.expires) {
		p.mu.Unlock()
		return e.result
	}
	p.mu.Unlock()

	res := p.fetch(ctx, url)
	res.URL = url
	res.Plugin = name + ":" + version

	p.mu.Lock()
	p.cache[url] = downloadProbeEntry{result: res, expires: now.Add(downloadProbeTTL)}
	for k, e := range p.cache {
		if !now.Before(e.expires) {
			delete(p.cache, k)
		}
	}
	p.mu.Unlock()
	return res
}

func (p *downloadURLProber) fetch(ctx context.Context, url string) downloadProbeResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return downloadProbeResult{}
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := p.client.Do(req)
	if err != nil {
		return downloadProbeResult{}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	res := downloadProbeResult{Status: resp.StatusCode}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		res.Verdict = downloadProbeReachable
	case http.StatusNotFound, http.StatusGone:
		res.Verdict = downloadProbeNotFound
	}
	return res
}

// firstVersionedPin returns the first name:version line of a plugins.txt
// line list. pluginsInitScript checks the same pin, so both checks agree.
func firstVersionedPin(lines []string) (name, version string, ok bool) {
	for _, l := range lines {
		n, v, found := strings.Cut(strings.TrimSpace(l), ":")
		if found && n != "" && v != "" {
			return n, v, true
		}
	}
	return "", "", false
}

// applyDownloadURLProbe writes the UpdateCenterFallback verdict of the
// download-URL check. A pass clears only its own DownloadURLNotFound verdict.
func applyDownloadURLProbe(conds []v1alpha1.ControllerCondition, res downloadProbeResult) []v1alpha1.ControllerCondition {
	switch res.Verdict {
	case downloadProbeNotFound:
		return setCondition(conds, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionUpdateCenterFallback,
			Status:  metav1.ConditionTrue,
			Reason:  v1alpha1.ReasonDownloadURLNotFound,
			Message: downloadURLNotFoundMessage("download URL check", res.URL, res.Plugin, fmt.Sprint(res.Status)),
		})
	case downloadProbeReachable:
		if c := findCondition(conds, v1alpha1.ConditionUpdateCenterFallback); c != nil && c.Status == metav1.ConditionTrue && c.Reason == v1alpha1.ReasonDownloadURLNotFound {
			return setCondition(conds, v1alpha1.ControllerCondition{
				Type:    v1alpha1.ConditionUpdateCenterFallback,
				Status:  metav1.ConditionFalse,
				Reason:  v1alpha1.ReasonDownloadURLReachable,
				Message: "download URL check: " + res.URL + " serves the plugin",
			})
		}
	}
	return conds
}
