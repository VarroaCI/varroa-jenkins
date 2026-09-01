package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/jenkinsver"
)

// UpgradeTrackerInterval is how often UpgradeTrackerReconciler ticks. The
// tickerRunnable registering it in cmd/operator/main.go is leader-gated
// (everyReplica: false), since every tick performs outbound HTTP probes and
// CRD writes that must not race across operator replicas.
const UpgradeTrackerInterval = 6 * time.Hour

// maxPatchProbeSteps bounds the same-line newer-patch probe: a Jenkins LTS
// line typically ships on the order of a dozen patches a year, so 20 gives
// headroom for a line that has gone unwatched across several ticks, without
// an unbounded fetch loop.
const maxPatchProbeSteps = 20

// upstreamUpdatesBaseURL is the upstream Jenkins update-center host this
// tracker probes. Always the real internet — unlike UpdateCenterReconciler's
// configurable ucBaseURL, this reconciler has no in-cluster mode.
const upstreamUpdatesBaseURL = "https://updates.jenkins.io"

// UpgradeTrackerReconciler discovers upstream Jenkins LTS patch/line movement
// on a periodic tick: a same-line newer patch than a tracked
// JenkinsVersionProfile's current ResolveVersion becomes a ProfileCandidate; a
// newer LTS line than every tracked profile's line is a one-shot informational
// event. See docs/operations/jenkins-upgrades.md.
type UpgradeTrackerReconciler struct {
	store             crdstore.Backend
	activityPublisher activity.EventSink
	logger            *slog.Logger

	// httpDoer performs HTTP requests. Default is http.DefaultClient.
	httpDoer interface {
		Do(*http.Request) (*http.Response, error)
	}

	// seenFacts dedupes upgrade.upstream.observed across ticks: keyed by the
	// discovered fact, e.g. "newLine:2.570", never by profile name — a newer
	// line is a single global fact. Process-local: an operator restart may
	// re-emit one event per still-true fact.
	seenFacts map[string]struct{}
}

// NewUpgradeTrackerReconciler creates a new UpgradeTrackerReconciler.
func NewUpgradeTrackerReconciler(store crdstore.Backend, activityPublisher activity.EventSink, logger *slog.Logger) *UpgradeTrackerReconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &UpgradeTrackerReconciler{
		store:             store,
		activityPublisher: activityPublisher,
		logger:            logger,
		httpDoer:          http.DefaultClient,
		seenFacts:         make(map[string]struct{}),
	}
}

// Reconcile runs one tick: the once-per-tick new-LTS-line fetch, the
// per-profile same-line newer-patch probe (which may create or supersede
// ProfileCandidates), and the new-line informational event. One profile's
// failure is logged and does not stop the rest — never a hard failure.
func (r *UpgradeTrackerReconciler) Reconcile(ctx context.Context) {
	profiles, err := crdstore.List[v1alpha1.JenkinsVersionProfile](ctx, r.store, "", "")
	if err != nil {
		r.logger.Warn("upgrade tracker: list JenkinsVersionProfiles failed", "error", err)
		return
	}

	var tracked []*v1alpha1.JenkinsVersionProfile
	for _, p := range profiles {
		if p.Spec.Channel == "lts" && isTwoSegmentVersion(p.Spec.Version) {
			tracked = append(tracked, p)
		}
	}

	discoveredLine, ok := r.fetchNewLTSLine(ctx)

	for _, p := range tracked {
		r.reconcileProfile(ctx, p)
	}

	if ok {
		r.emitNewLineIfNewer(discoveredLine, tracked)
	}
}

// isTwoSegmentVersion reports whether version has exactly two dot-separated
// segments (e.g. "2.479") — the scoping rule applied before considering a
// profile tracked as an "LTS line".
func isTwoSegmentVersion(version string) bool {
	return len(strings.Split(version, ".")) == 2
}

// fetchNewLTSLine issues a GET against upstream's "current stable LTS"
// alias, following the redirect (an ordinary http.Client does this by
// default) onto the per-patch document, decoding only its top-level
// core.version into the major.minor line. Never a hard failure: any
// fetch/parse problem logs at warn and returns ok=false, which skips this
// tick's new-line check only.
func (r *UpgradeTrackerReconciler) fetchNewLTSLine(ctx context.Context) (string, bool) {
	url := upstreamUpdatesBaseURL + "/stable/update-center.actual.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		r.logger.Warn("upgrade tracker: build stable/ request failed", "error", err)
		return "", false
	}
	resp, err := r.httpDoer.Do(req)
	if err != nil {
		r.logger.Warn("upgrade tracker: fetch stable/ failed", "error", err)
		return "", false
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		r.logger.Warn("upgrade tracker: unexpected status fetching stable/", "status", resp.StatusCode)
		return "", false
	}
	var doc struct {
		Core struct {
			Version string `json:"version"`
		} `json:"core"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		r.logger.Warn("upgrade tracker: parse stable/ response failed", "error", err)
		return "", false
	}
	segs, ok := jenkinsver.Core(doc.Core.Version)
	if !ok || len(segs) < 2 {
		r.logger.Warn("upgrade tracker: unparseable core.version in stable/ response", "version", doc.Core.Version)
		return "", false
	}
	return fmt.Sprintf("%d.%d", segs[0], segs[1]), true
}

// reconcileProfile probes same-line newer patches for a single tracked
// profile, starting just above the profile's current ResolveVersion, and on
// any discovery creates/supersedes ProfileCandidates accordingly. A
// transport/parse error at any probe step logs at warn and skips this
// profile for this tick only.
func (r *UpgradeTrackerReconciler) reconcileProfile(ctx context.Context, profile *v1alpha1.JenkinsVersionProfile) {
	segs, ok := jenkinsver.Core(profile.Spec.ResolveVersion)
	if !ok || len(segs) != 3 {
		r.logger.Warn("upgrade tracker: skipping profile with unparseable resolveVersion",
			"profile", profile.Name, "resolveVersion", profile.Spec.ResolveVersion)
		return
	}
	major, minor, patch := segs[0], segs[1], segs[2]

	lastSuccess := -1
	k := patch + 1
	for step := 0; step < maxPatchProbeSteps; step++ {
		url := fmt.Sprintf("%s/dynamic-stable-%d.%d.%d/update-center.actual.json", upstreamUpdatesBaseURL, major, minor, k)
		status, err := r.probeStatus(ctx, url)
		if err != nil {
			r.logger.Warn("upgrade tracker: patch probe failed", "profile", profile.Name, "url", url, "error", err)
			return
		}
		if status == http.StatusNotFound {
			break
		}
		if status != http.StatusOK {
			r.logger.Warn("upgrade tracker: unexpected status on patch probe",
				"profile", profile.Name, "url", url, "status", status)
			return
		}
		lastSuccess = k
		k++
	}
	if lastSuccess == -1 {
		// First probe 404'd: the profile's current patch is already the
		// newest on its line. No candidate, no error.
		return
	}

	discoveredVersion := fmt.Sprintf("%d.%d.%d", major, minor, lastSuccess)
	r.createCandidate(ctx, profile, discoveredVersion)
	r.supersedeOlder(ctx, profile.Name, discoveredVersion)
}

// probeStatus issues a GET against url and returns its status code. The body
// is drained (not parsed — only the status code matters here) so keep-alive
// connections can be reused.
func (r *UpgradeTrackerReconciler) probeStatus(ctx context.Context, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := r.httpDoer.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// candidateName computes the deterministic ProfileCandidate name: the
// profile name plus the discovered patch with dots replaced by dashes, e.g.
// "jenkins-version-2-555" + "2.555.4" -> "jenkins-version-2-555-2-555-4".
func candidateName(profileName, resolveVersion string) string {
	return profileName + "-" + strings.ReplaceAll(resolveVersion, ".", "-")
}

// createCandidate makes an ApplyOwned call with owned hard-coded false — any
// existing object of this name, in any phase, is left untouched; ErrNotOwned
// is a silent no-op meaning "already discovered" — followed by the separate
// PatchStatus call setting Phase: Pending that ProfileCandidate's
// subresource:status marker requires.
func (r *UpgradeTrackerReconciler) createCandidate(ctx context.Context, profile *v1alpha1.JenkinsVersionProfile, discoveredVersion string) {
	name := candidateName(profile.Name, discoveredVersion)
	candidate := &v1alpha1.ProfileCandidate{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ProfileCandidateSpec{
			ProfileRef:      profile.Name,
			ObservedVersion: profile.Spec.ResolveVersion,
			ResolveVersion:  discoveredVersion,
		},
	}
	if err := crdstore.ApplyOwned(ctx, r.store, candidate, func(*unstructured.Unstructured) bool { return false }); err != nil {
		if errors.Is(err, crdstore.ErrNotOwned) {
			return
		}
		r.logger.Warn("upgrade tracker: create candidate failed", "candidate", name, "error", err)
		return
	}
	if err := crdstore.PatchStatus[v1alpha1.ProfileCandidate](ctx, r.store, name, "", &v1alpha1.ProfileCandidateStatus{
		Phase: v1alpha1.ProfileCandidatePhasePending,
	}); err != nil {
		r.logger.Warn("upgrade tracker: set pending phase on new candidate failed", "candidate", name, "error", err)
	}
	r.publish(activity.Event{
		Type:     "upgrade.candidate.created",
		Source:   "operator",
		Severity: "info",
		Message:  fmt.Sprintf("discovered candidate patch %s for profile %s (currently %s)", discoveredVersion, profile.Name, profile.Spec.ResolveVersion),
	})
}

// supersedeOlder transitions any existing Pending/Ready ProfileCandidate for
// profileRef whose ResolveVersion is older than discoveredVersion to Phase:
// Superseded.
func (r *UpgradeTrackerReconciler) supersedeOlder(ctx context.Context, profileRef, discoveredVersion string) {
	newSegs, ok := jenkinsver.Core(discoveredVersion)
	if !ok {
		return
	}
	candidates, err := crdstore.List[v1alpha1.ProfileCandidate](ctx, r.store, "", "")
	if err != nil {
		r.logger.Warn("upgrade tracker: list ProfileCandidates for supersession failed", "profileRef", profileRef, "error", err)
		return
	}
	for _, c := range candidates {
		if c.Spec.ProfileRef != profileRef {
			continue
		}
		if c.Status.Phase != v1alpha1.ProfileCandidatePhasePending && c.Status.Phase != v1alpha1.ProfileCandidatePhaseReady {
			continue
		}
		oldSegs, ok := jenkinsver.Core(c.Spec.ResolveVersion)
		if !ok || jenkinsver.Compare(oldSegs, newSegs) >= 0 {
			continue
		}
		if err := crdstore.PatchStatus[v1alpha1.ProfileCandidate](ctx, r.store, c.Name, "", &v1alpha1.ProfileCandidateStatus{
			Phase: v1alpha1.ProfileCandidatePhaseSuperseded,
		}); err != nil {
			r.logger.Warn("upgrade tracker: supersede candidate failed", "candidate", c.Name, "error", err)
			continue
		}
		r.publish(activity.Event{
			Type:     "upgrade.candidate.superseded",
			Source:   "operator",
			Severity: "info",
			Message:  fmt.Sprintf("candidate %s superseded by newly discovered patch %s for profile %s", c.Name, discoveredVersion, profileRef),
		})
	}
}

// emitNewLineIfNewer emits the informational upgrade.upstream.observed event
// when discoveredLine is newer than every tracked profile's line, exactly
// once per distinct fact (never once per profile). Never creates or mutates
// a ProfileCandidate.
func (r *UpgradeTrackerReconciler) emitNewLineIfNewer(discoveredLine string, tracked []*v1alpha1.JenkinsVersionProfile) {
	if len(tracked) == 0 {
		return
	}
	discoveredSegs, ok := jenkinsver.Core(discoveredLine)
	if !ok {
		return
	}
	for _, p := range tracked {
		lineSegs, ok := jenkinsver.Core(p.Spec.Version)
		if !ok {
			continue
		}
		if jenkinsver.Compare(discoveredSegs, lineSegs) <= 0 {
			return
		}
	}

	fact := "newLine:" + discoveredLine
	if _, seen := r.seenFacts[fact]; seen {
		return
	}
	r.seenFacts[fact] = struct{}{}
	r.publish(activity.Event{
		Type:     "upgrade.upstream.observed",
		Source:   "operator",
		Severity: "info",
		Message:  fmt.Sprintf("upstream Jenkins LTS line %s observed, newer than every tracked profile line", discoveredLine),
	})
}

func (r *UpgradeTrackerReconciler) publish(e activity.Event) {
	if r.activityPublisher == nil {
		return
	}
	r.activityPublisher.Publish(e)
}
