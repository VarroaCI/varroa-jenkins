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
	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// UpgradeTrackerInterval is how often UpgradeTrackerReconciler ticks. The
// tickerRunnable registering it in cmd/operator/main.go is leader-gated
// (everyReplica: false), since every tick performs outbound HTTP reads and
// CRD writes that must not race across operator replicas.
const UpgradeTrackerInterval = 6 * time.Hour

// UpgradeTrackerReconciler discovers upstream Jenkins LTS patch/line movement
// on a periodic tick: a newer patch on a tracked JenkinsVersionProfile's line
// becomes a ProfileCandidate; a newer LTS line than every tracked profile's
// line is a one-shot informational event. See
// docs/operations/jenkins-upgrades.md.
type UpgradeTrackerReconciler struct {
	store             crdstore.Backend
	activityPublisher activity.EventSink
	logger            *slog.Logger

	// baseURL is the Jenkins upstream host the tracker reads, with the
	// updates.jenkins.io layout. The operator passes "" when upstream reads are
	// off; Reconcile then does nothing.
	baseURL string

	// httpDoer performs HTTP requests. The default client has a timeout so a
	// dropped connection cannot hold a tick open.
	httpDoer interface {
		Do(*http.Request) (*http.Response, error)
	}

	// seenFacts dedupes upgrade.upstream.observed across ticks: keyed by the
	// discovered fact, e.g. "newLine:2.570", never by profile name — a newer
	// line is a single global fact. Process-local: an operator restart may
	// re-emit one event per still-true fact.
	seenFacts map[string]struct{}
}

// NewUpgradeTrackerReconciler creates a new UpgradeTrackerReconciler that
// reads from baseURL. An empty baseURL makes the tracker inert.
func NewUpgradeTrackerReconciler(store crdstore.Backend, activityPublisher activity.EventSink, baseURL string, logger *slog.Logger) *UpgradeTrackerReconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &UpgradeTrackerReconciler{
		store:             store,
		activityPublisher: activityPublisher,
		logger:            logger,
		baseURL:           strings.TrimRight(baseURL, "/"),
		httpDoer:          &http.Client{Timeout: candidateHTTPTimeout},
		seenFacts:         make(map[string]struct{}),
	}
}

// Reconcile runs one tick: it reads the stable core and tiers.json once, then
// checks each tracked profile for a newer patch on its line (which may create
// or supersede ProfileCandidates), and emits the new-line informational
// event. One profile's failure is logged and does not stop the rest — never a
// hard failure.
func (r *UpgradeTrackerReconciler) Reconcile(ctx context.Context) {
	if r.baseURL == "" {
		return
	}
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

	stableCore, stableOK := r.fetchStableCore(ctx)
	tiers, err := ucmeta.FetchTiers(ctx, r.httpDoer, r.baseURL)
	if err != nil {
		r.logger.Warn("upgrade tracker: read tiers.json failed; using the stable core only", "error", err)
	}
	observed := append([]string(nil), tiers.StableCores...)
	if stableOK {
		observed = append(observed, stableCore)
	}

	for _, p := range tracked {
		r.reconcileProfile(ctx, p, observed)
	}

	if line, ok := newestObservedLine(observed); ok {
		r.emitNewLineIfNewer(line, tracked)
	}
}

// newestObservedLine returns the major.minor line of the newest observed
// patch. A version that is not an exact patch is skipped.
func newestObservedLine(observed []string) (string, bool) {
	var best []int
	for _, v := range observed {
		segs, ok := jenkinsver.Core(strings.TrimSpace(v))
		if !ok || len(segs) != 3 {
			continue
		}
		if best == nil || jenkinsver.Compare(segs, best) > 0 {
			best = segs
		}
	}
	if best == nil {
		return "", false
	}
	return fmt.Sprintf("%d.%d", best[0], best[1]), true
}

// isTwoSegmentVersion reports whether version has exactly two dot-separated
// segments (e.g. "2.479") — the scoping rule applied before considering a
// profile tracked as an "LTS line".
func isTwoSegmentVersion(version string) bool {
	return len(strings.Split(version, ".")) == 2
}

// stableDocumentMaxBytes caps the stable/ update-center document read. The
// document is a full update-center listing of several megabytes, so the cap
// must sit above its real size. It only guards against an unbounded body.
const stableDocumentMaxBytes = 32 << 20

// latestCoreMaxBytes caps the stable/latestCore.txt read. The file holds one
// version string.
const latestCoreMaxBytes = 64

// parseLTSPatch returns the exact MAJOR.MINOR.PATCH form of v. It rejects a
// version with any other segment count.
func parseLTSPatch(v string) (string, bool) {
	segs, ok := jenkinsver.Core(strings.TrimSpace(v))
	if !ok || len(segs) != 3 {
		return "", false
	}
	return fmt.Sprintf("%d.%d.%d", segs[0], segs[1], segs[2]), true
}

// readStableDocumentCore reads core.version from upstream's "current stable
// LTS" document, following the redirect that an ordinary http.Client follows.
func (r *UpgradeTrackerReconciler) readStableDocumentCore(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/stable/update-center.actual.json", nil)
	if err != nil {
		return "", err
	}
	resp, err := r.httpDoer.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var doc struct {
		Core struct {
			Version string `json:"version"`
		} `json:"core"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, stableDocumentMaxBytes)).Decode(&doc); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	core, ok := parseLTSPatch(doc.Core.Version)
	if !ok {
		return "", fmt.Errorf("unparseable core.version %q", doc.Core.Version)
	}
	return core, nil
}

// readStableLatestCore reads the plain-text stable core from
// stable/latestCore.txt.
func (r *UpgradeTrackerReconciler) readStableLatestCore(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/stable/latestCore.txt", nil)
	if err != nil {
		return "", err
	}
	resp, err := r.httpDoer.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, latestCoreMaxBytes))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	core, ok := parseLTSPatch(string(body))
	if !ok {
		return "", fmt.Errorf("unparseable latestCore.txt body %q", string(body))
	}
	return core, nil
}

// fetchStableCore returns the newest stable LTS core. It reads the stable/
// document and falls back to stable/latestCore.txt only when that read fails.
// Never a hard failure: when both reads fail it logs at warn and returns
// ok=false, which skips the stable-core input for this tick only.
func (r *UpgradeTrackerReconciler) fetchStableCore(ctx context.Context) (string, bool) {
	core, err := r.readStableDocumentCore(ctx)
	if err == nil {
		return core, true
	}
	r.logger.Warn("upgrade tracker: read stable/ core.version failed; reading stable/latestCore.txt", "error", err)
	core, err = r.readStableLatestCore(ctx)
	if err != nil {
		r.logger.Warn("upgrade tracker: read stable/latestCore.txt failed", "error", err)
		return "", false
	}
	return core, true
}

// reconcileProfile finds the newest observed patch on the profile's line that
// is above its current ResolveVersion, and on any discovery creates or
// supersedes ProfileCandidates accordingly. It performs no HTTP request. The
// observed versions are the stable core and every tiers.json entry, on any line.
func (r *UpgradeTrackerReconciler) reconcileProfile(ctx context.Context, profile *v1alpha1.JenkinsVersionProfile, observed []string) {
	segs, ok := jenkinsver.Core(profile.Spec.ResolveVersion)
	if !ok || len(segs) != 3 {
		r.logger.Warn("upgrade tracker: skipping profile with unparseable resolveVersion",
			"profile", profile.Name, "resolveVersion", profile.Spec.ResolveVersion)
		return
	}
	major, minor := segs[0], segs[1]
	current := segs

	var best []int
	for _, v := range observed {
		vs, ok := jenkinsver.Core(strings.TrimSpace(v))
		if !ok || len(vs) != 3 {
			continue
		}
		if vs[0] != major || vs[1] != minor {
			continue
		}
		if jenkinsver.Compare(vs, current) <= 0 {
			continue
		}
		if best == nil || jenkinsver.Compare(vs, best) > 0 {
			best = vs
		}
	}
	if best == nil {
		return
	}

	discoveredVersion := fmt.Sprintf("%d.%d.%d", best[0], best[1], best[2])
	r.createCandidate(ctx, profile, discoveredVersion)
	r.supersedeOlder(ctx, profile.Name, discoveredVersion)
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
