package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/api/logbuffer"
	"github.com/varroaci/varroa-jenkins/internal/api/sse"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/auth/identity"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/observability"
	"github.com/varroaci/varroa-jenkins/internal/overlay"
	"github.com/varroaci/varroa-jenkins/internal/preflight"
)

// Server holds handler methods for the BFF API.
type Server struct {
	deps *Dependencies
}

// NewServer creates a new API server with the given dependencies.
func NewServer(deps *Dependencies) *Server {
	return &Server{deps: deps}
}

// notifyActivity publishes a user-action activity event through the bus
// Publisher (the single write path for user actions). The off-mode ring is
// fed by the activity.> subscriber, not by direct store writes, per the
// single-ring-writer rule.
func (s *Server) notifyActivity(e activity.Event) {
	if s.deps.ActivityPublisher != nil {
		s.deps.ActivityPublisher.Publish(e)
	}
}

// meResponse is the JSON shape for GET /api/v1/me.
type meResponse struct {
	PreferredUsername string           `json:"preferredUsername,omitempty"`
	Subject           string           `json:"subject"`
	Email             string           `json:"email"`
	Name              string           `json:"name"`
	Groups            []string         `json:"groups"`
	DisplayName       string           `json:"displayName,omitempty"`
	Preferences       *userPreferences `json:"preferences,omitempty"`
	AuthMode          string           `json:"authMode"`
	LastLogin         *time.Time       `json:"lastLogin,omitempty"`
}

type userPreferences struct {
	Theme          string `json:"theme,omitempty"`
	Accent         string `json:"accent,omitempty"`
	DefaultLanding string `json:"defaultLanding,omitempty"`
}

// HandleMe returns the current user from JWT claims, enriched with User CRD data.
func (s *Server) HandleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		cookie, err := r.Cookie("varroa_token")
		if err != nil || cookie.Value == "" {
			s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		if s.deps.Auth != nil {
			parsed, err := s.deps.Auth.Verify(r.Context(), cookie.Value)
			if err != nil {
				s.writeJSONError(w, http.StatusUnauthorized, "invalid token")
				return
			}
			claims = parsed
		}
	}

	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	resp := meResponse{
		Subject:           claims.Subject,
		PreferredUsername: claims.PreferredUsername,
		Email:             claims.Email,
		Name:              claims.Name,
		Groups:            claims.Groups,
	}

	// Populate auth mode from the provider. Default to oidc when no
	// provider is configured (e.g. dev/noop mode).
	resp.AuthMode = "oidc"
	if s.deps.Auth != nil {
		resp.AuthMode = string(s.deps.Auth.Mode())
	}

	// Enrich with User CRD if it exists. Look up by subject-derived name
	// rather than email so local users (named by username) are found.
	if s.deps.OperatorNamespace != "" {
		ctx := context.Background()
		userName := identity.UserResourceName(claims,
			auth.AuthMode(s.deps.IdentityConfig.Mode))
		if userName != "" {
			user, err := crdstore.Get[v1alpha1.User](ctx, s.deps.Store, userName, s.deps.OperatorNamespace)
			if err == nil && user != nil {
				resp.DisplayName = user.Spec.DisplayName
				if user.Status.Preferences != nil {
					resp.Preferences = &userPreferences{
						Theme:          user.Status.Preferences.Theme,
						Accent:         user.Status.Preferences.Accent,
						DefaultLanding: user.Status.Preferences.DefaultLanding,
					}
				}
				if user.Status.LastLogin != nil {
					t := user.Status.LastLogin.Time
					resp.LastLogin = &t
				}
			}
		}
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// HandleLogout clears the session cookie, sets the interactive-login marker,
// and returns JSON redirect (no Location header). In OIDC mode, reads the ID
// token before clearing for provider end-session when available.
func (s *Server) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Read the current ID token before clearing the cookie (for provider logout).
	idToken := ""
	if cookie, err := r.Cookie("varroa_token"); err == nil {
		idToken = cookie.Value
	}

	// Emit logout event when an authenticated user logs out.
	if s.deps.ActivityStore != nil {
		if claims := auth.ClaimsFromContext(r.Context()); ActorFrom(claims) != "" {
			s.notifyActivity(activity.Event{
				Type:    "logout",
				Source:  "user",
				Actor:   ActorFrom(claims),
				Message: ActorFrom(claims) + " logged out",
			})
		}
	}

	cookieDomain := ""
	if s.deps.Auth != nil {
		cookieDomain = s.deps.Auth.CookieDomain()
	}

	s.setCookie(w, &http.Cookie{
		Name:   "varroa_token",
		Value:  "",
		Domain: cookieDomain,
		Path:   "/",
		MaxAge: -1,
	})

	// Determine redirect destination.
	redirectTo := "/login"
	if s.deps.Auth != nil && (s.deps.Auth.Mode() == auth.AuthModeLocal || s.deps.Auth.Mode() == auth.AuthModeLDAP) {
		redirectTo = "/"
	}

	// In OIDC mode, set interactive marker and attempt provider end-session.
	if s.deps.Auth != nil && s.deps.Auth.Mode() == auth.AuthModeOIDC {
		// Set interactive-login marker cookie (consumed by /api/v1/auth/login).
		s.setCookie(w, &http.Cookie{
			Name:   interactiveCookieName,
			Value:  "1",
			Path:   "/api/v1/auth",
			MaxAge: stateCookieMaxAge,
		})

		// Build provider end-session URL when supported.
		if validator, ok := s.validator(); ok && validator.EndSessionEndpoint() != "" && idToken != "" {
			u, err := url.Parse(validator.EndSessionEndpoint())
			if err == nil {
				q := u.Query()
				q.Set("id_token_hint", idToken)
				postLogoutURI := s.deps.DashboardURL + "/login"
				q.Set("post_logout_redirect_uri", postLogoutURI)
				u.RawQuery = q.Encode()
				redirectTo = u.String()
				s.deps.Logger.Debug("provider end-session redirect", "url", u.Redacted())
			}
		}
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"redirect": redirectTo})
}

// controllerResponse is the JSON shape for a controller in the list.
type controllerResponse struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Cluster   string `json:"cluster"`
	Phase     string `json:"phase"`
	Endpoint  string `json:"endpoint"`
	// Version is the desired Jenkins version (from spec.version); the list row
	// pairs it with the actual running JenkinsVersion to surface upgrade drift.
	Version string `json:"version,omitempty"`
	// MiteStatus fields, only set when connected.
	MiteConnected  bool   `json:"miteConnected"`
	MiteVersion    string `json:"miteVersion,omitempty"`
	JenkinsVersion string `json:"jenkinsVersion,omitempty"`
	JenkinsHealth  string `json:"jenkinsHealth,omitempty"`
	// Bundle reference (field from the CRD spec). Nil means the controller names
	// no bundle; see EffectiveBundle for what it is actually running. This field
	// stays the raw spec value because the UI writes it back — populating it with
	// the resolved value would materialize an explicit ref on the next PATCH.
	ComposedBundleRef *v1alpha1.ComposedBundleRef `json:"composedBundleRef,omitempty"`
	// EffectiveBundle is the bundle the controller is actually using, resolved.
	// Read-only.
	EffectiveBundle *effectiveBundleJSON `json:"effectiveBundle,omitempty"`
	// RoutingMode is the normalized routing mode: "subdomain" or "path".
	RoutingMode string `json:"routingMode"`
	// AppliedBundleHash is the last delivered bundle hash (for fan-out view).
	AppliedBundleHash string `json:"appliedBundleHash,omitempty"`
	// RolloutWave from the reconciliation policy (for fan-out grouping).
	RolloutWave int `json:"rolloutWave,omitempty"`
}

// controllerDetailResponse is the JSON shape for a single controller.
type controllerDetailResponse struct {
	// Spec is the controller's full spec, projected verbatim so the UI spec
	// editor can render and diff it. Additive — the flattened fields below
	// remain for the consumers that predate the editor.
	Spec      v1alpha1.ControllerSpec `json:"spec"`
	Name      string                  `json:"name"`
	Namespace string                  `json:"namespace"`
	Cluster   string                  `json:"cluster"`
	Phase     string                  `json:"phase"`
	Endpoint  string                  `json:"endpoint"`
	Version   string                  `json:"version"`
	// MiteStatus fields.
	// PowerState reflects spec.powerState (Running or Stopped).
	PowerState string `json:"powerState,omitempty"`
	// Hibernated reflects status.hibernated: true when the controller is parked.
	Hibernated bool `json:"hibernated,omitempty"`
	// HibernatedAt is the RFC 3339 timestamp of the last hibernation (status.hibernatedAt).
	HibernatedAt   string `json:"hibernatedAt,omitempty"`
	MiteConnected  bool   `json:"miteConnected"`
	MiteVersion    string `json:"miteVersion,omitempty"`
	JenkinsVersion string `json:"jenkinsVersion,omitempty"`
	JenkinsHealth  string `json:"jenkinsHealth,omitempty"`
	LastSeen       string `json:"lastSeen,omitempty"`
	CertExpiry     string `json:"certExpiry,omitempty"`
	// Hashes.
	DesiredStateHash string `json:"desiredStateHash,omitempty"`
	ConfigHash       string `json:"configHash,omitempty"`
	RBACHash         string `json:"rbacHash,omitempty"`
	// Reconciliation policy and pending restart.
	ReconciliationPolicy *ReconciliationPolicyJSON `json:"reconciliationPolicy,omitempty"`
	PendingRestart       *PendingRestartJSON       `json:"pendingRestart,omitempty"`
	// Plugin-roll field.
	PendingPluginRoll    *PendingPluginRollJSON `json:"pendingPluginRoll,omitempty"`
	PendingItemDeletions []PendingDeletionJSON  `json:"pendingItemDeletions,omitempty"`
	FirstConnectedAt     string                 `json:"firstConnectedAt,omitempty"`
	LastReconciledAt     string                 `json:"lastReconciledAt,omitempty"`
	// Apply result and history.
	LastApplyResult *ApplyResultJSON  `json:"lastApplyResult,omitempty"`
	ApplyHistory    []ApplyResultJSON `json:"applyHistory,omitempty"`
	// Bundle reference (field from the CRD spec). See the note on
	// controllerSummary.ComposedBundleRef: this is the raw spec value because the
	// UI writes it back.
	ComposedBundleRef *v1alpha1.ComposedBundleRef `json:"composedBundleRef,omitempty"`
	// EffectiveBundle is the bundle the controller is actually using, resolved.
	// Read-only.
	EffectiveBundle *effectiveBundleJSON `json:"effectiveBundle,omitempty"`
	// Resource override knobs (from the CRD spec) so the UI overlay editor can
	// pre-populate when editing a controller that already has overrides.
	PodOverrides    *v1alpha1.PodOverrides    `json:"podOverrides,omitempty"`
	Probes          *v1alpha1.ProbesSpec      `json:"probes,omitempty"`
	ResourceOverlay *v1alpha1.ResourceOverlay `json:"resourceOverlay,omitempty"`
	// Observability is the normalized observability model (nil when not available).
	Observability *controllerObservabilityJSON `json:"observability,omitempty"`
	// RoutingMode is the normalized routing mode: "subdomain" or "path".
	RoutingMode string `json:"routingMode"`
	// LiveDrift is the controller's live-drift status (C9).
	LiveDrift *LiveDriftJSON `json:"liveDrift,omitempty"`
	// Rollout is the controller's rollout gate status (C10).
	Rollout *RolloutJSON `json:"rollout,omitempty"`
	// AppliedBundleHash is the ComposedBundle.ResolvedHash last delivered to this controller.
	AppliedBundleHash string `json:"appliedBundleHash,omitempty"`
	// VersionStatus projects the version-roll (A) and upgrade-guard (B) conditions
	// into a read-only surface for the UI; nil when neither condition is present.
	VersionStatus *VersionStatusJSON `json:"versionStatus,omitempty"`
	// MiteImageStatus projects the actually-running mite image and whether it
	// is stale vs. the operator-desired image; nil when never observed.
	MiteImageStatus *MiteImageStatusJSON `json:"miteImageStatus,omitempty"`
	// ReconcileBlocked projects whether the controller's reconcile loop is
	// currently blocked by an unresolved error (C3). Always present (never nil),
	// so it is a required field with no omitempty — matches the OpenAPI contract.
	ReconcileBlocked *ReconcileBlockedJSON `json:"reconcileBlocked"`
	// PluginConflict projects ConditionPluginConflict (C4) — an existing,
	// general-purpose plugin-pin-vs-lock check surfaced here, not a new
	// detection mechanism. Nil when the condition has never been recorded.
	PluginConflict *PluginConflictJSON `json:"pluginConflict,omitempty"`
	// PluginInventory is the bounded plugin inventory summary from
	// Controller.status.pluginInventory. Nil when never classified.
	PluginInventory *PluginInventorySummaryJSON `json:"pluginInventory,omitempty"`
	// UnappliedRemovals names requested spec removals (explicit nulls) that did
	// not take effect because another field manager still owns the field. Only
	// present on the update response, and only when at least one removal was
	// blocked — an empty or absent array leaves the ordinary success path
	// unchanged.
	UnappliedRemovals []bus.UnappliedRemoval `json:"unappliedRemovals,omitempty"`
}

// VersionStatusJSON is a read-only projection of the two version-related
// controller conditions (written by changes A and B) for the detail DTO.
type VersionStatusJSON struct {
	RollPending    *bool  `json:"rollPending,omitempty"`
	RollReason     string `json:"rollReason,omitempty"`
	RollMessage    string `json:"rollMessage,omitempty"`
	UpgradeBlocked *bool  `json:"upgradeBlocked,omitempty"`
	BlockedReason  string `json:"blockedReason,omitempty"`
	BlockedMessage string `json:"blockedMessage,omitempty"`
}

// buildVersionStatus projects the two version-related conditions (written by
// changes A and B) into the detail DTO. Returns nil when neither is present, so
// the field is omitted for pre-A/B controllers. It reads existing condition
// types only and introduces no new ones.
func buildVersionStatus(conds []v1alpha1.ControllerCondition) *VersionStatusJSON {
	var vs *VersionStatusJSON
	ensure := func() *VersionStatusJSON {
		if vs == nil {
			vs = &VersionStatusJSON{}
		}
		return vs
	}
	for i := range conds {
		c := conds[i]
		isTrue := c.Status == metav1.ConditionTrue
		switch c.Type {
		case v1alpha1.ConditionVersionRollPending:
			v := ensure()
			b := isTrue
			v.RollPending = &b
			v.RollReason = c.Reason
			v.RollMessage = c.Message
		case v1alpha1.ConditionVersionUpgradeBlocked:
			v := ensure()
			b := isTrue
			v.UpgradeBlocked = &b
			v.BlockedReason = c.Reason
			v.BlockedMessage = c.Message
		}
	}
	return vs
}

// MiteImageStatusJSON is a read-only projection of the mite image staleness
// condition (written by the detect-stale-mite-images change) for the detail
// DTO. Not modeled as a generic conditions array — the Controller detail DTO
// does not pass one through.
type MiteImageStatusJSON struct {
	Image *string `json:"image,omitempty"`
	Stale *bool   `json:"stale,omitempty"`
}

// buildMiteImageStatus projects cr.Status.MiteStatus.Image and the
// MiteImageStale condition into the detail DTO. Returns nil when the mite
// image has never been observed, so the field is omitted entirely (not
// present with empty/zero values) for a controller that has never had a Pod
// or StatefulSet resolve an image.
func buildMiteImageStatus(ms *v1alpha1.MiteStatus, conds []v1alpha1.ControllerCondition) *MiteImageStatusJSON {
	if ms == nil || ms.Image == "" {
		return nil
	}
	out := &MiteImageStatusJSON{Image: &ms.Image}
	for i := range conds {
		if conds[i].Type == v1alpha1.ConditionMiteImageStale {
			stale := conds[i].Status == metav1.ConditionTrue
			out.Stale = &stale
			break
		}
	}
	return out
}

// buildReconcileBlockedJSON projects the ConditionReconcileBlocked condition
// and LastReconcileError/LastReconcileErrorAt fields into the detail DTO.
// Always returns a non-nil *ReconcileBlockedJSON — the object itself is
// always present on the response, only its inner reason/message/since are
// conditional on blocked.
func buildReconcileBlockedJSON(cr *v1alpha1.Controller) *ReconcileBlockedJSON {
	for _, c := range cr.Status.Conditions {
		if c.Type == v1alpha1.ConditionReconcileBlocked {
			if c.Status == metav1.ConditionTrue {
				result := &ReconcileBlockedJSON{Blocked: true, Reason: c.Reason, Message: c.Message}
				if cr.Status.LastReconcileErrorAt != nil {
					s := cr.Status.LastReconcileErrorAt.Format(time.RFC3339)
					result.Since = &s
				}
				return result
			}
			return &ReconcileBlockedJSON{Blocked: false}
		}
	}
	return &ReconcileBlockedJSON{Blocked: false}
}

// PluginConflictJSON is a read-only projection of ConditionPluginConflict.
type PluginConflictJSON struct {
	Active  *bool  `json:"active,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// buildPluginConflict projects ConditionPluginConflict into the detail DTO.
// Returns nil if the condition has never been recorded on the controller.
func buildPluginConflict(conds []v1alpha1.ControllerCondition) *PluginConflictJSON {
	for i := range conds {
		if conds[i].Type == v1alpha1.ConditionPluginConflict {
			active := conds[i].Status == metav1.ConditionTrue
			return &PluginConflictJSON{
				Active:  &active,
				Reason:  conds[i].Reason,
				Message: conds[i].Message,
			}
		}
	}
	return nil
}

// PluginInventorySummaryJSON is the bounded plugin inventory summary projected
// from Controller.status.pluginInventory into the detail DTO.
type PluginInventorySummaryJSON struct {
	Hash                 string                      `json:"hash"`
	CollectedAt          string                      `json:"collectedAt,omitempty"`
	ObservedAt           string                      `json:"observedAt,omitempty"`
	Source               string                      `json:"source"`
	Stale                bool                        `json:"stale"`
	Degraded             bool                        `json:"degraded"`
	BootstrapApproximate bool                        `json:"bootstrapApproximate"`
	OptionalEdgesDropped bool                        `json:"optionalEdgesDropped"`
	Truncated            bool                        `json:"truncated"`
	Total                int                         `json:"total"`
	Counts               map[string]int              `json:"counts,omitempty"`
	Drift                []PluginInventoryDriftJSON  `json:"drift,omitempty"`
	VersionDrift         []PluginInventoryDriftJSON  `json:"versionDrift,omitempty"`
	DriftTruncated       bool                        `json:"driftTruncated"`
	PendingCollect       *PluginInventoryPendingJSON `json:"pendingCollect"`
}

// PluginInventoryDriftJSON is one row of a drift or version-drift list.
type PluginInventoryDriftJSON struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Class   string `json:"class,omitempty"`
	Verdict string `json:"verdict,omitempty"`
	Message string `json:"message,omitempty"`
}

// PluginInventoryPendingJSON is the pending-collect status projected to the DTO.
type PluginInventoryPendingJSON struct {
	CommandID string `json:"commandId"`
	IssuedAt  string `json:"issuedAt"`
}

// buildPluginInventorySummary projects Controller.status.pluginInventory
// into the detail DTO. Returns nil when never classified.
func buildPluginInventorySummary(pi *v1alpha1.PluginInventoryStatus) *PluginInventorySummaryJSON {
	if pi == nil {
		return nil
	}
	s := &PluginInventorySummaryJSON{
		Hash:                 pi.Hash,
		Source:               pi.Source,
		Stale:                pi.Stale,
		Degraded:             pi.Degraded,
		BootstrapApproximate: pi.BootstrapApproximate,
		OptionalEdgesDropped: pi.OptionalEdgesDropped,
		Truncated:            pi.Truncated,
		Total:                pi.Total,
		Counts:               pi.Counts,
		DriftTruncated:       pi.DriftTruncated,
	}
	if pi.CollectedAt != nil {
		s.CollectedAt = pi.CollectedAt.Format(time.RFC3339)
	}
	if pi.ObservedAt != nil {
		s.ObservedAt = pi.ObservedAt.Format(time.RFC3339)
	}
	for _, d := range pi.Drift {
		s.Drift = append(s.Drift, PluginInventoryDriftJSON{
			Name: d.Name, Version: d.Version, Class: d.Class, Verdict: d.Verdict, Message: d.Message,
		})
	}
	for _, d := range pi.VersionDrift {
		s.VersionDrift = append(s.VersionDrift, PluginInventoryDriftJSON{
			Name: d.Name, Version: d.Version, Class: d.Class, Verdict: d.Verdict, Message: d.Message,
		})
	}
	if pi.PendingCollect != nil {
		s.PendingCollect = &PluginInventoryPendingJSON{
			CommandID: pi.PendingCollect.CommandID,
			IssuedAt:  pi.PendingCollect.IssuedAt.Format(time.RFC3339),
		}
	}
	return s
}

// ReconciliationPolicyJSON is the JSON shape for reconciliation policy.
type ReconciliationPolicyJSON struct {
	Mode                string `json:"mode,omitempty"`
	Interval            string `json:"interval,omitempty"`
	MaxDeferSeconds     int    `json:"maxDeferSeconds,omitempty"`
	DrainTimeoutSeconds int    `json:"drainTimeoutSeconds,omitempty"`
}

// PendingRestartJSON is the JSON shape for a pending restart.
type PendingRestartJSON struct {
	DetectedAt       string   `json:"detectedAt"`
	DesiredStateHash string   `json:"desiredStateHash"`
	Changes          []string `json:"changes,omitempty"`
}

// PendingPluginRollJSON is the JSON shape for a pending plugin roll.
type PendingPluginRollJSON struct {
	TargetChecksum string   `json:"targetChecksum"`
	Since          string   `json:"since"`
	Changes        []string `json:"changes,omitempty"`
}

// ApplySectionResultJSON is the JSON shape for one apply section result.
type ApplySectionResultJSON struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// ApplyResultJSON is the JSON shape for an apply result with RFC3339 timestamp.
type ApplyResultJSON struct {
	Hash      string                   `json:"hash,omitempty"`
	Timestamp string                   `json:"timestamp"`
	Succeeded bool                     `json:"succeeded"`
	Sections  []ApplySectionResultJSON `json:"sections"`
}

// PendingDeletionJSON is the JSON shape for a pending item deletion.
type PendingDeletionJSON struct {
	Path       string `json:"path"`
	Reason     string `json:"reason,omitempty"`
	DetectedAt string `json:"detectedAt,omitempty"`
}

// LiveDriftJSON is the JSON shape for live-drift status.
type LiveDriftJSON struct {
	Detected       bool    `json:"detected"`
	LiveConfigHash string  `json:"liveConfigHash,omitempty"`
	DetectedAt     *string `json:"detectedAt,omitempty"`
}

// ReconcileBlockedJSON is the JSON shape for the reconcile-blocked status (C3).
type ReconcileBlockedJSON struct {
	Blocked bool    `json:"blocked"`
	Reason  string  `json:"reason,omitempty"`
	Message string  `json:"message,omitempty"`
	Since   *string `json:"since,omitempty"` // RFC3339, from LastReconcileErrorAt
}

// RolloutJSON is the JSON shape for rollout gate status.
type RolloutJSON struct {
	TargetBundleHash string   `json:"targetBundleHash,omitempty"`
	Blocked          bool     `json:"blocked"`
	Paused           bool     `json:"paused,omitempty"`
	WaitingOn        []string `json:"waitingOn,omitempty"`
	BlockedSince     *string  `json:"blockedSince,omitempty"`
}

// controllerEndpoint returns the mode-aware external endpoint for a controller.
// Status.Endpoint takes precedence; when empty the endpoint is derived from
// IngressSpec: subdomain mode -> "https://<Host>", path mode -> "https://<Host>/jenkins/<ns>/<name>/".
func (s *Server) controllerEndpoint(cr *v1alpha1.Controller) string {
	if cr.Status.Endpoint != "" {
		return cr.Status.Endpoint
	}
	host := ""
	if cr.Spec.IngressSpec != nil {
		host = cr.Spec.IngressSpec.Host
	}
	if host == "" {
		defaults, _ := crdstore.Get[v1alpha1.ProvisioningDefaults](context.Background(), s.deps.Store, "varroa-defaults", "")
		if defaults != nil && defaults.Spec.RootDomain != "" {
			isSubdomain := cr.Spec.IngressSpec == nil || cr.Spec.IngressSpec.Mode == "" || cr.Spec.IngressSpec.Mode == "subdomain"
			if isSubdomain {
				host = cr.Name + "." + defaults.Spec.RootDomain
			}
		}
	}
	if host == "" {
		return ""
	}
	if cr.Spec.IngressSpec != nil && cr.Spec.IngressSpec.RoutingMode() == v1alpha1.RoutingModePath {
		return "https://" + host + v1alpha1.PathPrefix(cr.Namespace, cr.Name) + "/"
	}
	return "https://" + host
}

// visibleControllers lists controllers the caller may read. It lists cluster-wide,
// narrows to nsFilter when non-empty (narrow only — never widens beyond the caller's
// grants), and keeps a row only when the Authorizer permits reading it. It is the single
// authorization gate for the controller collection endpoints (list + search); the
// ?namespace= param is enforced here, not trusted. Deny-by-default: when the Authorizer
// is nil it returns an empty slice, never the unfiltered inventory.
func (s *Server) visibleControllers(ctx context.Context, claims *auth.Claims, nsFilter string) ([]*v1alpha1.Controller, error) {
	controllers, err := crdstore.List[v1alpha1.Controller](ctx, s.deps.Store, "", "") // cluster-wide; authz decides visibility
	if err != nil {
		return nil, err
	}
	if s.deps.Authorizer == nil {
		return nil, nil // deny-by-default
	}
	out := make([]*v1alpha1.Controller, 0, len(controllers))
	for _, cr := range controllers {
		if nsFilter != "" && cr.Namespace != nsFilter {
			continue
		}
		if s.deps.Authorizer.CanReadController(claims, cr.Namespace, cr.Name) {
			out = append(out, cr)
		}
	}
	return out, nil
}

// HandleControllers returns the list of controllers enriched with live mite status.
// Supports ?namespace= and ?cluster= query parameters.
func (s *Server) HandleControllers(w http.ResponseWriter, r *http.Request) {
	s.handleControllersFiltered(w, r, r.URL.Query().Get("cluster"))
}

// handleControllersFiltered lists controllers, optionally restricted to one
// cluster. The path-scoped /clusters/{cluster}/controllers route passes its
// cluster segment here, overriding any ?cluster= query parameter.
func (s *Server) handleControllersFiltered(w http.ResponseWriter, r *http.Request, clusterFilter string) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	namespace := r.URL.Query().Get("namespace")
	claims := auth.ClaimsFromContext(r.Context())

	ctx := context.Background()

	// Validate cluster filter if set.
	if clusterFilter != "" && s.deps.Brood != nil && !s.deps.Brood.IsKnown(ctx, clusterFilter) {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown cluster"})
		return
	}

	var allControllers []*v1alpha1.Controller
	var clusterLabels []string
	var clustersStatus []ClusterFanoutStatus

	if s.deps.Brood != nil {
		cc, cs, err := s.deps.Brood.ListAll(ctx, namespace, clusterFilter)
		if err != nil {
			s.deps.Logger.Error("list all controllers failed", "error", err)
		}
		for _, c := range cc {
			if s.deps.Authorizer == nil || s.deps.Authorizer.CanReadController(claims, c.CR.Namespace, c.CR.Name) {
				allControllers = append(allControllers, c.CR)
				clusterLabels = append(clusterLabels, c.Cluster)
			}
		}
		clustersStatus = cs
	} else {
		// Fallback: local-only via visibleControllers.
		controllers, err := s.visibleControllers(ctx, claims, namespace)
		if err != nil {
			s.deps.Logger.Error("list controllers failed", "error", err)
			s.writeJSONError(w, http.StatusInternalServerError, "failed to list controllers")
			return
		}
		allControllers = controllers
	}

	out := make([]controllerResponse, 0, len(allControllers))
	for i, cr := range allControllers {
		cluster := ""
		if i < len(clusterLabels) {
			cluster = clusterLabels[i]
		}
		crResp := controllerResponse{
			Name:              cr.Name,
			Namespace:         cr.Namespace,
			Cluster:           cluster,
			Phase:             string(cr.Status.Phase),
			Endpoint:          s.controllerEndpoint(cr),
			Version:           cr.Spec.Version,
			ComposedBundleRef: cr.Spec.ComposedBundleRef,
			EffectiveBundle:   s.effectiveBundleFor(cr),
			RoutingMode:       cr.Spec.IngressSpec.RoutingMode(),
			AppliedBundleHash: cr.Status.AppliedBundleHash,
		}
		if cr.Spec.ReconciliationPolicy != nil {
			crResp.RolloutWave = cr.Spec.ReconciliationPolicy.RolloutWave
		}
		// Mite telemetry lives in the local cluster's KV only — a lookup for a
		// remote row is a guaranteed-miss NATS round-trip. Remote rows use the
		// remote operator's own view from the CR status instead.
		if cluster == "" || s.deps.Brood == nil || cluster == s.deps.Brood.LocalCluster() {
			s.mergeMiteStatus(cr.Name, cr.Namespace, &crResp)
		} else if ms := cr.Status.MiteStatus; ms != nil {
			crResp.MiteConnected = ms.Connected
			crResp.MiteVersion = ms.Version
			if ms.JenkinsVersion != "" && ms.JenkinsVersion != "NORMAL" {
				crResp.JenkinsVersion = ms.JenkinsVersion
			}
			crResp.JenkinsHealth = ms.JenkinsHealth
		}
		out = append(out, crResp)
	}

	resp := map[string]interface{}{
		"items": out,
	}
	if clustersStatus != nil {
		resp["clusters"] = clustersStatus
	} else if s.deps.Brood != nil {
		resp["clusters"] = []ClusterFanoutStatus{}
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleControllerDetail returns a single controller with full mite telemetry.
func (s *Server) handleControllerDetail(w http.ResponseWriter, r *http.Request, cluster, namespace, name string) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	claims := auth.ClaimsFromContext(r.Context())
	if !s.deps.Authorizer.CanReadController(claims, namespace, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	cr, err := s.getController(context.Background(), cluster, namespace, name)
	if err != nil {
		s.deps.Logger.Error("get controller failed", "cluster", cluster, "namespace", namespace, "name", name, "error", err)
		var fe *BroodError
		switch {
		case errors.As(err, &fe) && fe.Code == bus.CodeNotFound,
			!errors.As(err, &fe) && strings.Contains(err.Error(), "not found"):
			s.writeJSONError(w, http.StatusNotFound, "controller not found")
		case errors.As(err, &fe):
			// Remote-cluster failure that is not a 404 (timeout, internal, …).
			s.writeJSONError(w, http.StatusBadGateway, "cluster unreachable or errored")
		default:
			s.writeJSONError(w, http.StatusInternalServerError, "failed to get controller")
		}
		return
	}

	resp := s.controllerDetail(cr, cluster)
	// Detail-only enrichment on top of the shared projection.
	resp.DesiredStateHash = cr.Status.DesiredStateHash
	resp.ConfigHash = cr.Status.ConfigHash
	resp.RBACHash = cr.Status.RBACHash
	resp.PodOverrides = cr.Spec.PodOverrides
	resp.ResourceOverlay = cr.Spec.ResourceOverlay
	// Version conditions (A: VersionRollPending, B: VersionUpgradeBlocked).
	resp.VersionStatus = buildVersionStatus(cr.Status.Conditions)
	// Pending restart from status.
	if cr.Status.PendingRestart != nil {
		resp.PendingRestart = &PendingRestartJSON{
			DetectedAt:       cr.Status.PendingRestart.DetectedAt.Format(time.RFC3339),
			DesiredStateHash: cr.Status.PendingRestart.DesiredStateHash,
			Changes:          cr.Status.PendingRestart.Changes,
		}
	}
	// Pending plugin roll from status.
	if cr.Status.PendingPluginRoll != nil {
		resp.PendingPluginRoll = &PendingPluginRollJSON{
			TargetChecksum: cr.Status.PendingPluginRoll.TargetChecksum,
			Since:          cr.Status.PendingPluginRoll.Since.Format(time.RFC3339),
			Changes:        cr.Status.PendingPluginRoll.Changes,
		}
	}
	// Pending item deletions from status.
	for _, d := range cr.Status.PendingItemDeletions {
		detectedAt := ""
		if !d.DetectedAt.IsZero() {
			detectedAt = d.DetectedAt.Format(time.RFC3339)
		}
		resp.PendingItemDeletions = append(resp.PendingItemDeletions, PendingDeletionJSON{
			Path:       d.Path,
			Reason:     d.Reason,
			DetectedAt: detectedAt,
		})
	}
	if cr.Status.FirstConnectedAt != nil {
		resp.FirstConnectedAt = cr.Status.FirstConnectedAt.Format(time.RFC3339)
	}
	if cr.Status.LastReconciledAt != nil {
		resp.LastReconciledAt = cr.Status.LastReconciledAt.Format(time.RFC3339)
	}
	// Apply result from status.
	if cr.Status.LastApplyResult != nil {
		resp.LastApplyResult = &ApplyResultJSON{
			Hash:      cr.Status.LastApplyResult.Hash,
			Timestamp: cr.Status.LastApplyResult.Timestamp.Format(time.RFC3339),
			Succeeded: cr.Status.LastApplyResult.Succeeded,
			Sections:  mapApplySections(cr.Status.LastApplyResult.Sections),
		}
	}
	if len(cr.Status.ApplyHistory) > 0 {
		resp.ApplyHistory = make([]ApplyResultJSON, len(cr.Status.ApplyHistory))
		for i, ar := range cr.Status.ApplyHistory {
			resp.ApplyHistory[i] = ApplyResultJSON{
				Hash:      ar.Hash,
				Timestamp: ar.Timestamp.Format(time.RFC3339),
				Succeeded: ar.Succeeded,
				Sections:  mapApplySections(ar.Sections),
			}
		}
	}
	// Mite telemetry lives in the local cluster's KV only — a lookup for a
	// remote controller is a guaranteed-miss NATS round-trip. Remote
	// controllers use the remote operator's own view from the CR status
	// instead (mirrors the list handler's per-row merge above).
	if cluster == "" || s.deps.Brood == nil || cluster == s.deps.Brood.LocalCluster() {
		s.mergeMiteTelemetry(cr.Name, cr.Namespace, &resp)
	} else if ms := cr.Status.MiteStatus; ms != nil {
		resp.MiteConnected = ms.Connected
		resp.MiteVersion = ms.Version
		if ms.JenkinsVersion != "" && ms.JenkinsVersion != "NORMAL" {
			resp.JenkinsVersion = ms.JenkinsVersion
		}
		resp.JenkinsHealth = ms.JenkinsHealth
		if ms.LastSeen != nil {
			resp.LastSeen = ms.LastSeen.Format("2006-01-02T15:04:05Z")
		}
		if ms.CertExpiry != nil {
			resp.CertExpiry = ms.CertExpiry.Format("2006-01-02")
		}
	}

	// Populate observability model if normalizer is available.
	if s.deps.ObsNormalizer != nil {
		annotations := s.observabilityIntentAnnotations(r.Context(), cluster, cr)
		obs := s.deps.ObsNormalizer.normalize(namespace, name, annotations)
		resp.Observability = &obs
	}

	// Serialize live-drift status.
	if cr.Status.LiveDrift != nil {
		resp.LiveDrift = &LiveDriftJSON{
			Detected:       cr.Status.LiveDrift.Detected,
			LiveConfigHash: cr.Status.LiveDrift.LiveConfigHash,
		}
		if cr.Status.LiveDrift.DetectedAt != nil {
			ts := cr.Status.LiveDrift.DetectedAt.Format(time.RFC3339)
			resp.LiveDrift.DetectedAt = &ts
		}
	}

	// Serialize rollout status.
	if cr.Status.Rollout != nil {
		resp.Rollout = &RolloutJSON{
			TargetBundleHash: cr.Status.Rollout.TargetBundleHash,
			Blocked:          cr.Status.Rollout.Blocked,
			Paused:           cr.Status.Rollout.Paused,
			WaitingOn:        cr.Status.Rollout.WaitingOn,
		}
		if cr.Status.Rollout.BlockedSince != nil {
			ts := cr.Status.Rollout.BlockedSince.Format(time.RFC3339)
			resp.Rollout.BlockedSince = &ts
		}
	}

	// Serialize applied bundle hash.
	resp.AppliedBundleHash = cr.Status.AppliedBundleHash

	s.writeJSON(w, http.StatusOK, resp)
}

// handleControllerYAML returns the controller CRD rendered as YAML.
func (s *Server) handleControllerYAML(w http.ResponseWriter, r *http.Request, cluster, namespace, name string) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadController(claims, namespace, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	cr, err := s.getController(context.Background(), cluster, namespace, name)
	if err != nil {
		s.deps.Logger.Error("get controller yaml failed", "cluster", cluster, "namespace", namespace, "name", name, "error", err)
		var fe *BroodError
		if errors.As(err, &fe) && fe.Code != bus.CodeNotFound {
			s.writeJSONError(w, http.StatusBadGateway, "cluster unreachable or errored")
		} else {
			s.writeJSONError(w, http.StatusNotFound, "controller not found")
		}
		return
	}

	// Strip credential and churn fields. SanitizeObject is the single
	// implementation shared with the MCP surface, so a field added to the strip
	// list is removed from both or neither.
	obj, err := SanitizeObject(cr)
	if err != nil {
		s.deps.Logger.Error("sanitize controller failed", "cluster", cluster,
			"namespace", namespace, "name", name, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "failed to serialize controller")
		return
	}

	// Marshal with sigs.k8s.io/yaml for kubectl-compatible output.
	yamlBytes, err := yaml.Marshal(obj)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "yaml marshal failed")
		return
	}

	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	w.Write(yamlBytes)
}

// getController resolves a controller CR, routing remote clusters over the
// Brood bus. Local lookups stay direct so k8s error semantics are preserved.
func (s *Server) getController(ctx context.Context, cluster, namespace, name string) (*v1alpha1.Controller, error) {
	if s.deps.Brood != nil && cluster != s.deps.Brood.LocalCluster() {
		return s.deps.Brood.Get(ctx, cluster, namespace, name)
	}
	return crdstore.Get[v1alpha1.Controller](ctx, s.deps.Store, name, namespace)
}

// mergeMiteStatus merges live mite data into a controllerResponse.
func (s *Server) mergeMiteStatus(name, namespace string, cr *controllerResponse) {
	if s.deps.MiteRegistry == nil {
		return
	}
	miteVersion, _, _, ok := s.deps.MiteRegistry.Info(namespace, name)
	if !ok {
		return
	}
	cr.MiteConnected = true
	cr.MiteVersion = miteVersion

	if snap := s.deps.MiteRegistry.Snapshot(namespace, name); snap != nil {
		// "NORMAL" is the Jenkins mode sentinel, never a valid version — drop it.
		if snap.JenkinsVersion != "" && snap.JenkinsVersion != "NORMAL" {
			cr.JenkinsVersion = snap.JenkinsVersion
		}
		cr.JenkinsHealth = snap.JenkinsHealth
	}
}

// mergeMiteTelemetry merges full mite telemetry into a controllerDetailResponse.
func (s *Server) mergeMiteTelemetry(name, namespace string, cr *controllerDetailResponse) {
	if s.deps.MiteRegistry == nil {
		return
	}
	miteVersion, lastHeartbeat, certExpiry, ok := s.deps.MiteRegistry.Info(namespace, name)
	if !ok {
		return
	}
	cr.MiteConnected = true
	cr.MiteVersion = miteVersion
	cr.LastSeen = lastHeartbeat.Format("2006-01-02T15:04:05Z")
	cr.CertExpiry = certExpiry.Format("2006-01-02")

	if snap := s.deps.MiteRegistry.Snapshot(namespace, name); snap != nil {
		// "NORMAL" is the Jenkins mode sentinel, never a valid version — drop it.
		if snap.JenkinsVersion != "" && snap.JenkinsVersion != "NORMAL" {
			cr.JenkinsVersion = snap.JenkinsVersion
		}
		cr.JenkinsHealth = snap.JenkinsHealth
	}
}

// handleControllerLogs serves log entries for a controller, optionally as SSE.
// When a LogBuffer is injected by tests or local wiring, it polls that ring buffer.
// Otherwise the BFF streams directly from the Kubernetes pod logs API.
// Not available for remote clusters (returns 501).
func (s *Server) handleControllerLogs(w http.ResponseWriter, r *http.Request, cluster, namespace, name string) {
	if !s.requireLocalCluster(w, cluster, "logs") {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadController(claims, namespace, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	follow := r.URL.Query().Get("follow") == "true"
	key := namespace + "/" + name

	if !follow {
		// Path A: injected LogBuffer present.
		if s.deps.LogBuffer != nil {
			entries := s.deps.LogBuffer.Since(key, time.Time{})
			if entries == nil {
				entries = []logbuffer.LogEntry{}
			}
			s.writeJSON(w, http.StatusOK, itemsEnvelope(entries))
			return
		}
		// Path B: one-shot K8s tail.
		entries := s.fetchPodLogsOneShot(r.Context(), namespace, name)
		s.writeJSON(w, http.StatusOK, itemsEnvelope(entries))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeJSONError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Path A: injected LogBuffer present.
	if s.deps.LogBuffer != nil {
		s.streamFromLogBuffer(w, r, flusher, key)
		return
	}

	// Path B: stream directly from Kubernetes API.
	s.streamPodLogsSSE(w, r, flusher, namespace, name)
}

// streamFromLogBuffer polls the LogBuffer for new entries and writes them as SSE
// data events. Used when tests or local wiring inject an in-memory buffer.
func (s *Server) streamFromLogBuffer(w http.ResponseWriter, r *http.Request, flusher http.Flusher, key string) {
	var lastSent time.Time
	if s.deps.LogBuffer != nil {
		entries := s.deps.LogBuffer.Since(key, time.Time{})
		for _, e := range entries {
			fmt.Fprintf(w, "data: %s\n\n", mustMarshalLog(e))
			flusher.Flush()
			if e.Timestamp.After(lastSent) {
				lastSent = e.Timestamp
			}
		}
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if s.deps.LogBuffer == nil {
				continue
			}
			entries := s.deps.LogBuffer.Since(key, lastSent)
			for _, e := range entries {
				fmt.Fprintf(w, "data: %s\n\n", mustMarshalLog(e))
				flusher.Flush()
				if e.Timestamp.After(lastSent) {
					lastSent = e.Timestamp
				}
			}
		}
	}
}

// controllerPodName resolves the real StatefulSet pod name for a controller.
// Pods are UID-named ("<name>-<uid8>-0"), so the bare CR name must be resolved
// through the Controller CR — mirroring DeleteControllerPod. Using "<name>-0"
// targets a non-existent pod for every UID-named controller.
func (s *Server) controllerPodName(ctx context.Context, namespace, name string) (string, error) {
	cr, err := crdstore.Get[v1alpha1.Controller](ctx, s.deps.Store, name, namespace)
	if err != nil {
		return "", err
	}
	return controller.PodName(cr, 0), nil
}

// streamPodLogsSSE streams logs from both containers (jenkins + mite) of the
// controller's pod directly from the Kubernetes API, writing each line as an
// SSE data event. It reconnects on error after a 10-second delay. Exits when
// the request context is cancelled (client disconnect).
func (s *Server) streamPodLogsSSE(w http.ResponseWriter, r *http.Request, flusher http.Flusher, namespace, name string) {
	ctx := r.Context()
	podName, err := s.controllerPodName(ctx, namespace, name)
	if err != nil {
		s.deps.Logger.Warn("resolve controller pod name failed",
			"namespace", namespace, "controller", name, "error", err)
		return
	}
	containers := []string{"jenkins", "mite"}

	lineCh := make(chan logbuffer.LogEntry, 100)
	var wg sync.WaitGroup

	for _, container := range containers {
		wg.Add(1)
		go func(ctr string) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}

				rc, err := s.deps.Client.StreamPodLogs(ctx, namespace, podName, ctr, 50, true)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					s.deps.Logger.Warn("log stream failed, retrying in 10s",
						"namespace", namespace, "pod", podName, "container", ctr, "error", err)
					select {
					case <-ctx.Done():
						return
					case <-time.After(10 * time.Second):
					}
					continue
				}

				scanner := bufio.NewScanner(rc)
				scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
				for scanner.Scan() {
					line := scanner.Text()
					select {
					case lineCh <- logbuffer.LogEntry{
						Timestamp: time.Now(),
						Level:     logbuffer.DetectLogLevel(line),
						Source:    ctr,
						Message:   line,
					}:
					case <-ctx.Done():
						_ = rc.Close()
						return
					}
				}
				_ = rc.Close()

				if ctx.Err() != nil {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Second):
				}
			}
		}(container)
	}

	go func() {
		wg.Wait()
		close(lineCh)
	}()

	// SSE write loop. Checks write errors and emits periodic keepalive
	// comments so proxies/load balancers don't close idle connections.
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case entry, ok := <-lineCh:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", mustMarshalLog(entry)); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// fetchPodLogsOneShot performs a one-shot fetch of recent log lines from both
// containers (jenkins + mite) of the controller's pod. Fetches sequentially so
// the merged slice is ordered predictably (jenkins lines, then mite lines).
func (s *Server) fetchPodLogsOneShot(ctx context.Context, namespace, name string) []logbuffer.LogEntry {
	podName, err := s.controllerPodName(ctx, namespace, name)
	if err != nil {
		s.deps.Logger.Warn("resolve controller pod name failed",
			"namespace", namespace, "controller", name, "error", err)
		// Keep the response shape consistent: callers JSON-encode this
		// directly, and the success path returns [] (never nil).
		return []logbuffer.LogEntry{}
	}
	containers := []string{"jenkins", "mite"}

	var entries []logbuffer.LogEntry
	for _, container := range containers {
		rc, err := s.deps.Client.StreamPodLogs(ctx, namespace, podName, container, 500, false)
		if err != nil {
			s.deps.Logger.Warn("one-shot log fetch failed",
				"namespace", namespace, "pod", podName, "container", container, "error", err)
			continue
		}

		scanner := bufio.NewScanner(rc)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			entries = append(entries, logbuffer.LogEntry{
				Timestamp: time.Now(),
				Level:     logbuffer.DetectLogLevel(line),
				Source:    container,
				Message:   line,
			})
		}
		_ = rc.Close()
	}

	if entries == nil {
		return []logbuffer.LogEntry{}
	}
	return entries
}

func mapApplySections(sections []v1alpha1.ApplySectionResult) []ApplySectionResultJSON {
	out := make([]ApplySectionResultJSON, len(sections))
	for i, s := range sections {
		out[i] = ApplySectionResultJSON{
			Name:  s.Name,
			OK:    s.OK,
			Error: s.Error,
		}
	}
	return out
}

func mustMarshalLog(e logbuffer.LogEntry) string {
	js, _ := json.Marshal(e)
	return string(js)
}

// HandleUpdatePreferences persists user UI preferences via PUT /me/preferences.
func (s *Server) HandleUpdatePreferences(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil || claims.Email == "" {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	var prefs userPreferences
	if err := json.NewDecoder(r.Body).Decode(&prefs); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	ctx := context.Background()
	ns := s.deps.OperatorNamespace
	if ns == "" {
		s.writeJSONError(w, http.StatusInternalServerError, "operator namespace not configured")
		return
	}

	// Fetch existing or create new User CRD.
	user, err := crdstore.Get[v1alpha1.User](ctx, s.deps.Store, claims.Email, ns)
	if err != nil {
		// Create a new User CRD.
		user = &v1alpha1.User{}
		user.APIVersion = "varroa.dev/v1alpha1"
		user.Kind = "User"
		user.Name = claims.Email
		user.Namespace = ns
		user.Spec.Email = claims.Email
		user.Spec.DisplayName = claims.Name
	}

	user.Status.Preferences = &v1alpha1.UserPreferences{
		Theme:          prefs.Theme,
		Accent:         prefs.Accent,
		DefaultLanding: prefs.DefaultLanding,
	}

	if err := crdstore.Apply[v1alpha1.User](ctx, s.deps.Store, user); err != nil {
		s.deps.Logger.Error("apply user CRD failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "failed to save preferences")
		return
	}

	// Emit preferences update event.
	if s.deps.ActivityStore != nil {
		s.notifyActivity(activity.Event{
			Type:    "preferences.updated",
			Source:  "user",
			Actor:   ActorFrom(claims),
			Message: ActorFrom(claims) + " updated preferences",
		})
	}

	s.writeJSON(w, http.StatusOK, prefs)
}

// handleReconcile triggers an on-demand reconcile for a controller.
func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request, cluster, namespace, name string) {
	if r.Method != http.MethodPost {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.deps.Reconciler == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "reconciler not available")
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if !s.deps.Authorizer.CanApproveRestart(claims, namespace, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	// TriggerReconcile and WakeController share one implementation (both
	// enqueue an immediate reconcile); one bus round-trip is enough.
	s.deps.Reconciler.TriggerReconcile(cluster, name, namespace)

	// Emit reconcile trigger event when an authenticated user triggers it.
	if s.deps.ActivityStore != nil {
		if ActorFrom(claims) != "" {
			s.notifyActivity(activity.Event{
				Type:       "reconcile.triggered",
				Source:     "operator",
				Actor:      ActorFrom(claims),
				Controller: name,
				Namespace:  namespace,
				Message:    "manual reconcile triggered by " + ActorFrom(claims),
			})
		}
	}

	s.writeJSON(w, http.StatusAccepted, map[string]string{"status": "triggered"})
}

// handleApproveRestart handles POST /controllers/{ns}/{name}/approve.
// Body: {"action": "reload" | "restart"}
func (s *Server) handleApproveRestart(w http.ResponseWriter, r *http.Request, cluster, namespace, name string) {
	if r.Method != http.MethodPost {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.deps.Reconciler == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "reconciler not available")
		return
	}

	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Action != "reload" && body.Action != "restart" && body.Action != "approve" && body.Action != "force" && body.Action != "force-restart" && body.Action != "plugin-roll" {
		s.writeJSONError(w, http.StatusBadRequest, "action must be 'reload', 'restart', 'approve', 'force', 'force-restart', or 'plugin-roll'")
		return
	}

	claims := auth.ClaimsFromContext(r.Context())
	if !s.deps.Authorizer.CanApproveRestart(claims, namespace, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	if err := s.deps.Reconciler.ApproveRestart(r.Context(), cluster, namespace, name, body.Action); err != nil {
		if strings.Contains(err.Error(), "no pending restart") {
			s.writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		s.writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Emit approve event.
	if s.deps.ActivityStore != nil {
		if ActorFrom(claims) != "" {
			s.notifyActivity(activity.Event{
				Type:       "restart.approved",
				Source:     "user",
				Actor:      ActorFrom(claims),
				Controller: name,
				Namespace:  namespace,
				Message:    fmt.Sprintf("%s approved by %s", body.Action, ActorFrom(claims)),
			})
		}
	}

	s.writeJSON(w, http.StatusAccepted, map[string]string{"status": "approved", "action": body.Action})
}

// handleApproveDeletion handles POST /controllers/{ns}/{name}/approve-deletion.
// Body: {"path": "<item-path>"}
func (s *Server) handleApproveDeletion(w http.ResponseWriter, r *http.Request, cluster, namespace, name string) {
	if r.Method != http.MethodPost {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.deps.Reconciler == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "reconciler not available")
		return
	}

	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Path == "" {
		s.writeJSONError(w, http.StatusBadRequest, "path is required")
		return
	}

	claims := auth.ClaimsFromContext(r.Context())
	if !s.deps.Authorizer.CanApproveDeletion(claims, namespace, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	if err := s.deps.Reconciler.ApproveDeletion(r.Context(), cluster, namespace, name, body.Path); err != nil {
		if strings.Contains(err.Error(), "no pending deletion") {
			s.writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		s.writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.deps.ActivityStore != nil {
		if ActorFrom(claims) != "" {
			s.notifyActivity(activity.Event{
				Type:       "deletion.approved",
				Source:     "user",
				Actor:      ActorFrom(claims),
				Controller: name,
				Namespace:  namespace,
				Message:    fmt.Sprintf("item deletion of %s approved by %s", body.Path, ActorFrom(claims)),
			})
		}
	}

	s.writeJSON(w, http.StatusAccepted, map[string]string{"status": "approved", "path": body.Path})
}

// handleReprovision handles POST /controllers/{ns}/{name}/reprovision.
func (s *Server) handleReprovision(w http.ResponseWriter, r *http.Request, cluster, ns, name string) {
	if r.Method != http.MethodPost {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if !s.deps.Authorizer.CanManageController(claims, ns, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	if s.deps.Reconciler == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "reconciler not available")
		return
	}
	// Reprovision forces a full desired-state re-push to the mite (bypassing the
	// convergence short-circuit) and wakes the controller immediately.
	s.deps.Reconciler.Reprovision(cluster, ns, name)

	if s.deps.ActivityStore != nil && ActorFrom(claims) != "" {
		s.notifyActivity(activity.Event{
			Type:       "controller.reprovisioned",
			Source:     "user",
			Actor:      ActorFrom(claims),
			Controller: name,
			Namespace:  ns,
			Message:    "reprovision triggered by " + ActorFrom(claims),
		})
	}

	s.writeJSON(w, http.StatusAccepted, map[string]string{"status": "reprovisioning"})
}

// handleRestartController handles POST /controllers/{ns}/{name}/restart.
// Restart = pod delete; the StatefulSet recreates the pod. Build draining is
// the pod shutdown path's job, not this endpoint's.
func (s *Server) handleRestartController(w http.ResponseWriter, r *http.Request, cluster, ns, name string) {
	if r.Method != http.MethodPost {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if !s.deps.Authorizer.CanManageController(claims, ns, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	if s.deps.Brood != nil {
		if err := s.deps.Brood.DeletePod(r.Context(), cluster, ns, name); err != nil {
			s.deps.Logger.Error("restart failed", "cluster", cluster, "namespace", ns, "name", name, "error", err)
			writeBroodError(w, err)
			return
		}
	} else {
		if err := s.deps.Client.DeleteControllerPod(r.Context(), ns, name); err != nil {
			s.deps.Logger.Error("restart failed", "namespace", ns, "name", name, "error", err)
			s.writeJSONError(w, k8sErrorToHTTP(err), "failed to delete pod")
			return
		}
	}

	if s.deps.ActivityStore != nil && ActorFrom(claims) != "" {
		s.notifyActivity(activity.Event{
			Type:       "controller.restarted",
			Source:     "user",
			Actor:      ActorFrom(claims),
			Controller: name,
			Namespace:  ns,
			Cluster:    cluster,
			Message:    "restart triggered by " + ActorFrom(claims),
		})
	}

	s.writeJSON(w, http.StatusAccepted, map[string]string{"status": "restarting"})
}

// handleHibernate handles POST /clusters/{cluster}/controllers/{ns}/{name}/hibernate.
// It is an action route, not a spec.powerState patch: the manage permission is
// checked directly, and the request travels request-reply to the target
// cluster's operator hibernate subject.
func (s *Server) handleHibernate(w http.ResponseWriter, r *http.Request, cluster, ns, name string) {
	if r.Method != http.MethodPost {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanManageController(claims, ns, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	if s.deps.Reconciler == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "reconciler not available")
		return
	}
	if err := s.deps.Reconciler.Hibernate(r.Context(), cluster, ns, name); err != nil {
		s.writeActionError(w, err)
		return
	}
	if s.deps.ActivityStore != nil && ActorFrom(claims) != "" {
		s.notifyActivity(activity.Event{
			Type:       "controller.hibernated",
			Source:     "user",
			Actor:      ActorFrom(claims),
			Controller: name,
			Namespace:  ns,
			Cluster:    cluster,
			Message:    "hibernate triggered by " + ActorFrom(claims),
		})
	}
	s.writeJSON(w, http.StatusAccepted, map[string]string{"status": "hibernating"})
}

// handleWake handles POST /clusters/{cluster}/controllers/{ns}/{name}/wake.
// It is the authenticated request-reply counterpart to the tokenless traffic
// wake and travels on the target cluster's operator wake subject.
func (s *Server) handleWake(w http.ResponseWriter, r *http.Request, cluster, ns, name string) {
	if r.Method != http.MethodPost {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanManageController(claims, ns, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	if s.deps.Reconciler == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "reconciler not available")
		return
	}
	if err := s.deps.Reconciler.Wake(r.Context(), cluster, ns, name); err != nil {
		s.writeActionError(w, err)
		return
	}
	if s.deps.ActivityStore != nil && ActorFrom(claims) != "" {
		s.notifyActivity(activity.Event{
			Type:       "controller.woken",
			Source:     "user",
			Actor:      ActorFrom(claims),
			Controller: name,
			Namespace:  ns,
			Cluster:    cluster,
			Message:    "wake triggered by " + ActorFrom(claims),
		})
	}
	s.writeJSON(w, http.StatusAccepted, map[string]string{"status": "waking"})
}

// writeActionError maps a controller action refusal to its HTTP status. The
// operator's "conflict" code (e.g. hibernate/wake against a Stopped controller)
// is a 409; anything the operator refused with another code keeps the same
// not_found/invalid/internal mapping the brood error writer uses.
func (s *Server) writeActionError(w http.ResponseWriter, err error) {
	var ae *controller.ActionError
	if !errors.As(err, &ae) {
		s.writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	switch ae.Code {
	case bus.CodeConflict:
		s.writeJSONError(w, http.StatusConflict, ae.Msg)
	case bus.CodeNotFound:
		s.writeJSONError(w, http.StatusNotFound, ae.Msg)
	case bus.CodeInvalid:
		s.writeJSONError(w, http.StatusBadRequest, ae.Msg)
	default:
		s.writeJSONError(w, http.StatusInternalServerError, ae.Msg)
	}
}

// handleSearch searches all resource kinds visible to the caller.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if q == "" {
		s.writeJSON(w, http.StatusOK, map[string]any{"items": []interface{}{}})
		return
	}

	ctx := r.Context()
	claims := auth.ClaimsFromContext(r.Context())
	results := make([]searchResult, 0, 20)
	if s.deps.Authorizer == nil {
		s.writeJSON(w, http.StatusOK, itemsEnvelope(results))
		return
	}

	controllers, statuses, err := s.deps.Brood.ListAll(ctx, "", "")
	if err != nil {
		s.deps.Logger.Error("search controller fan-out failed", "error", err)
		s.writeJSONError(w, http.StatusServiceUnavailable, "search unavailable")
		return
	}
	for _, status := range statuses {
		if !status.OK {
			s.deps.Logger.Warn("search cluster unavailable", "cluster", status.Name, "error", status.Error)
		}
	}
	namespaces := make(map[string]searchResult)
	for _, row := range controllers {
		c := row.CR
		if c == nil || !s.deps.Authorizer.CanReadController(claims, c.Namespace, c.Name) {
			continue
		}
		if searchMatch(q, c.Name, c.Namespace, row.Cluster, "") {
			results = append(results, searchResult{Type: "controller", Cluster: row.Cluster, Namespace: c.Namespace, Name: c.Name, Link: searchControllerLink(row.Cluster, c.Namespace, c.Name)})
		}
		if strings.Contains(strings.ToLower(c.Namespace), q) {
			key := row.Cluster + "\x00" + c.Namespace
			namespaces[key] = searchResult{Type: "namespace", Cluster: row.Cluster, Namespace: c.Namespace, Name: c.Namespace, Link: "/controllers?cluster=" + url.QueryEscape(row.Cluster) + "&namespace=" + url.QueryEscape(c.Namespace)}
		}
	}
	results = append(results, mapSearchResults(namespaces)...)

	if s.deps.Authorizer.IsAdmin(claims) {
		groups, groupErr := crdstore.List[v1alpha1.Group](ctx, s.deps.Store, "", "")
		if groupErr != nil {
			s.deps.Logger.Warn("search groups failed", "error", groupErr)
		} else {
			for _, group := range groups {
				if searchMatch(q, group.Name, "", "", group.Spec.DisplayName) {
					results = append(results, searchResult{Type: "group", Name: group.Name, Description: group.Spec.DisplayName, Link: "/access/groups?query=" + url.QueryEscape(group.Name)})
				}
			}
		}
	}

	if s.deps.Authorizer.CanReadCatalogItems(claims) && s.deps.ConfigBrood != nil {
		clusters, clusterErr := s.deps.Brood.Clusters(ctx)
		if clusterErr != nil {
			s.deps.Logger.Warn("search catalog clusters failed", "error", clusterErr)
		} else {
			succeeded := 0
			for _, cluster := range clusters {
				rawItems, _, listErr := s.deps.ConfigBrood.ListCatalogItems(ctx, cluster.Name, "", CatalogItemFilter{})
				if listErr != nil {
					s.deps.Logger.Warn("search catalog cluster failed", "cluster", cluster.Name, "error", listErr)
					continue
				}
				succeeded++
				for _, raw := range rawItems {
					var item bus.CatalogItemSummary
					if json.Unmarshal(raw, &item) != nil || !searchMatch(q, item.Name, "", "", item.Description) {
						continue
					}
					results = append(results, searchResult{Type: "catalogitem", Cluster: cluster.Name, Namespace: item.Namespace, Name: item.Name, Description: item.Description, Link: "/catalog/items/" + url.PathEscape(item.Namespace) + "/" + url.PathEscape(item.Name) + "?cluster=" + url.QueryEscape(cluster.Name)})
				}
			}
			if len(clusters) > 0 && succeeded == 0 {
				s.writeJSONError(w, http.StatusServiceUnavailable, "search unavailable")
				return
			}
		}
	}
	results = rankSearchResults(results, q)
	s.writeJSON(w, http.StatusOK, itemsEnvelope(results))
}

type searchResult struct {
	Type        string `json:"type"`
	Cluster     string `json:"cluster,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Link        string `json:"link"`
}

func searchControllerLink(cluster, namespace, name string) string {
	return "/controllers/" + url.PathEscape(cluster) + "/" + url.PathEscape(namespace) + "/" + url.PathEscape(name)
}

func searchMatch(q, name, namespace, cluster, description string) bool {
	return strings.Contains(strings.ToLower(name), q) || strings.Contains(strings.ToLower(namespace), q) || strings.Contains(strings.ToLower(cluster), q) || strings.Contains(strings.ToLower(description), q)
}

func mapSearchResults(items map[string]searchResult) []searchResult {
	results := make([]searchResult, 0, len(items))
	for _, item := range items {
		results = append(results, item)
	}
	return results
}

func rankSearchResults(items []searchResult, q string) []searchResult {
	kindOrder := map[string]int{"controller": 0, "namespace": 1, "group": 2, "catalogitem": 3}
	rank := func(item searchResult) int {
		name := strings.ToLower(item.Name)
		switch {
		case name == q:
			return 0
		case strings.HasPrefix(name, q):
			return 1
		case strings.Contains(name, q):
			return 2
		default:
			return 3
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if kindOrder[items[i].Type] != kindOrder[items[j].Type] {
			return kindOrder[items[i].Type] < kindOrder[items[j].Type]
		}
		if rank(items[i]) != rank(items[j]) {
			return rank(items[i]) < rank(items[j])
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	counts := make(map[string]int)
	out := make([]searchResult, 0, 20)
	for _, item := range items {
		if counts[item.Type] < 5 {
			out = append(out, item)
			counts[item.Type]++
		}
	}
	return out
}

// handleActivity returns recent activity events.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.deps.Backfill == nil {
		s.writeJSON(w, http.StatusOK, activity.Page{Items: []activity.Event{}, RetentionMode: "off"})
		return
	}
	// Deny-by-default when the authorizer is not wired.
	if s.deps.Authorizer == nil {
		mode, days := s.deps.Backfill.Retention()
		s.writeJSON(w, http.StatusOK, activity.Page{Items: []activity.Event{}, RetentionMode: mode, RetentionDays: days})
		return
	}
	query, err := parseActivityQuery(r, s.deps.Backfill)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	query.Authorize = func(e activity.Event) bool { return s.deps.Authorizer.CanReadActivityEvent(claims, e) }
	page, err := s.deps.Backfill.Query(r.Context(), query)
	if err != nil {
		if errors.Is(err, activity.ErrInvalidCursor) {
			s.writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, activity.ErrCursorExpired) {
			s.writeJSONError(w, http.StatusGone, err.Error())
			return
		}
		s.deps.Logger.Error("activity backfill failed", "error", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load activity"})
		return
	}
	if page.Items == nil {
		page.Items = []activity.Event{}
	}
	s.writeJSON(w, http.StatusOK, page)
}

func parseActivityQuery(r *http.Request, backfill activity.Backfill) (activity.Query, error) {
	v := r.URL.Query()
	q := activity.Query{Cursor: v.Get("cursor"), Cluster: v.Get("cluster"), Controller: v.Get("controller"), Namespace: v.Get("namespace"), Source: v.Get("source"), Severity: v.Get("severity"), Actor: strings.TrimSpace(v.Get("actor")), Type: v.Get("type")}
	if raw := v.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 250 {
			return q, fmt.Errorf("limit must be between 1 and 250")
		}
		q.Limit = n
	}
	for raw, target := range map[string]**time.Time{"start": &q.Start, "end": &q.End} {
		if value := v.Get(raw); value != "" {
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return q, fmt.Errorf("%s must be RFC3339", raw)
			}
			*target = &parsed
		}
	}
	if (q.Start == nil) != (q.End == nil) {
		return q, fmt.Errorf("start and end must be provided together")
	}
	if q.Start != nil {
		if !q.Start.Before(*q.End) {
			return q, fmt.Errorf("start must be before end")
		}
		mode, days := backfill.Retention()
		if mode == "off" {
			return q, fmt.Errorf("historical ranges are unavailable when activity retention is off")
		}
		if q.Start.Before(time.Now().Add(-time.Duration(days) * 24 * time.Hour)) {
			return q, fmt.Errorf("start is outside activity retention")
		}
	}
	valid := func(value string, allowed ...string) bool {
		if value == "" {
			return true
		}
		return slices.Contains(allowed, value)
	}
	if !valid(q.Source, "operator", "mite", "jenkins", "user", "api", "mcp") || !valid(q.Severity, "info", "warning", "error") {
		return q, fmt.Errorf("invalid activity filter")
	}
	return q, nil
}

// handleBroodStreamSSE serves the brood-wide SSE stream (mite connect/
// disconnect/heartbeat/snapshot plus the global activity feed across every
// controller in every namespace). Gated on CanReadGlobalActivity — the
// existing "no-controller-scope, global feed" rule already used by the
// activity feed — because the existing per-event filter
// (isAuthorizedActivityEvent) explicitly passes non-activity.Event records
// (brood/mite events) through unfiltered, so reusing it here would leave
// the mite-telemetry half of the stream open; a connect-time gate closes
// both halves at once.
func (s *Server) handleBroodStreamSSE(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadGlobalActivity(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	if s.deps.Broadcaster == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "SSE not available")
		return
	}
	sse.HandleBroodStream(s.deps.Broadcaster)(w, r)
}

// handleActivityStream serves a live SSE stream of activity events.
func (s *Server) handleActivityStream(w http.ResponseWriter, r *http.Request) {
	if s.deps.Broadcaster == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "broadcaster not available")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeJSONError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Capture authorization context at connection time. Claims are fixed
	// for the lifetime of the connection; the Authorizer (informer-backed)
	// is re-evaluated per event for role-change reactivity.
	claims := auth.ClaimsFromContext(r.Context())
	authz := s.deps.Authorizer

	controller := r.URL.Query().Get("controller")
	filter, err := parseActivityQuery(r, s.deps.Backfill)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	var ch <-chan sse.Record
	if controller != "" {
		// Per-controller subscription: key format "namespace/controller"
		// When we only have a bare controller name, subscribe to the
		// global feed; the authz gate will enrich per-controller filtering.
		// For now, subscribe globally — the frontend filters.
		ch = s.deps.Broadcaster.SubscribeAll()
	} else {
		ch = s.deps.Broadcaster.SubscribeAll()
	}
	defer s.deps.Broadcaster.Unsubscribe("*", ch)

	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case record, ok := <-ch:
			if !ok {
				return
			}
			// Per-event authorization: skip events the caller is not
			// allowed to see. Deny-by-default when authorizer is nil.
			if !s.isAuthorizedActivityEvent(claims, authz, record) {
				continue
			}
			if e, eventOK := record.Data.(activity.Event); eventOK && !activity.Matches(e, filter) {
				continue
			}
			js, _ := json.Marshal(record.Data)
			fmt.Fprintf(w, "event: activity\ndata: %s\n\n", js)
			flusher.Flush()
		}
	}
}

// isAuthorizedActivityEvent checks whether the caller may see the activity
// event contained in an SSE record. Returns false when the authorizer is nil
// (deny-by-default).
// TODO(add-activity-persistence): The handler-level gate is superseded when
// /activity/stream migrates onto the filtered BusFanout path. That migration
// MUST wire the SubscribeFiltered closure (capturing claims + Authorizer) and
// gate persisted-history reads through CanReadActivityEvent (or per-controller
// subject-scoped consumers + the global-event rule).
func (s *Server) isAuthorizedActivityEvent(claims *auth.Claims, authz *Authorizer, record sse.Record) bool {
	if authz == nil {
		return false
	}
	e, ok := record.Data.(activity.Event)
	if !ok {
		// Not an activity event (e.g. brood heartbeat) — let it through;
		// non-activity events are not subject to activity authz rules.
		return true
	}
	return authz.CanReadActivityEvent(claims, e)
}

// ---------------------------------------------------------------------------
// Controller CRUD handlers
// ---------------------------------------------------------------------------

// handleCreateController handles POST /controllers/{ns}.
// The body is a K8s-style JSON object with apiVersion, kind, metadata, and spec.
// createControllerRequest wraps a Controller with an optional inline bundle spec.
type createControllerRequest struct {
	v1alpha1.Controller
	Bundle *v1alpha1.ComposedBundleSpec `json:"bundle,omitempty"`
}

// handlePreflightController handles POST /controllers/{ns}/preflight.
func (s *Server) handlePreflightController(w http.ResponseWriter, r *http.Request, cluster, namespace string) {
	ctx := r.Context()

	var req createControllerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Name == "" {
		s.writeJSONError(w, http.StatusBadRequest, "metadata.name is required")
		return
	}

	claims := auth.ClaimsFromContext(r.Context())
	if !s.deps.Authorizer.CanCreateController(claims, namespace, req.Name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	req.Namespace = namespace

	// The ingress rules for create (path-mode locality and dashboard-host
	// equality) live in the BFF's ValidateIngress — the operator-side preflight
	// cannot see DashboardHost — so mirror them here as a check. Otherwise
	// preflight passes green for a draft handleCreateController will 400.
	// Only emitted when the draft configures ingress at all, so the check list
	// reflects what was actually validated.
	var ingressChecks []preflight.Check
	if req.Spec.IngressSpec != nil {
		check := preflight.Check{ID: "ingress-mode", Status: "pass", Message: "ingress mode ok"}
		if serr := s.controllerSvc().ValidateIngress(req.Spec.IngressSpec, true, cluster, req.Name); serr != nil {
			check = preflight.Check{ID: "ingress-mode", Status: "fail", Message: serr.Message}
		}
		ingressChecks = append(ingressChecks, check)
	}

	if s.deps.Brood != nil {
		crJSON, _ := json.Marshal(req.Controller)
		var bundleJSON json.RawMessage
		if req.Bundle != nil {
			bundleJSON, _ = json.Marshal(req.Bundle)
		}
		checks, err := s.deps.Brood.Preflight(ctx, cluster, ControllersCreateArgs{Namespace: namespace, Controller: crJSON, Bundle: bundleJSON})
		if err != nil {
			s.deps.Logger.Error("preflight via brood failed", "cluster", cluster, "namespace", namespace, "name", req.Name, "error", err)
			writeBroodErrorChecks(w, err, checks)
			return
		}
		if checks == nil {
			checks = []bus.Check{}
		}
		for _, c := range ingressChecks {
			checks = append(checks, bus.Check(c))
		}
		s.writeJSON(w, http.StatusOK, map[string]interface{}{"checks": checks})
		return
	}

	// Fallback: local direct.
	checks := preflight.Run(ctx, PreflightStore{Store: s.deps.Store, Client: s.deps.Client}, &req.Controller, req.Bundle, preflight.Options{OperatorNamespace: s.deps.OperatorNamespace, ManagedNamespaces: s.deps.ManagedNamespaces})
	if checks == nil {
		checks = []preflight.Check{}
	}
	checks = append(checks, ingressChecks...)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"checks": checks})
}

// handleRenderController handles POST /controllers/{ns}/render.
func (s *Server) handleRenderController(w http.ResponseWriter, r *http.Request, cluster, namespace string) {
	ctx := r.Context()

	var req createControllerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Name == "" {
		s.writeJSONError(w, http.StatusBadRequest, "metadata.name is required")
		return
	}

	claims := auth.ClaimsFromContext(r.Context())
	if !s.deps.Authorizer.CanCreateController(claims, namespace, req.Name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	req.Namespace = namespace

	// Apply server-side defaults only when target == local cluster.
	isLocal := s.deps.Brood == nil || cluster == s.deps.Brood.LocalCluster()
	if isLocal {
		defaults, _ := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, s.deps.Store, "varroa-defaults", "")
		if req.Spec.Version == "" && defaults != nil {
			req.Spec.Version = defaults.Spec.DefaultVersion
		}
		if req.Spec.Resources == nil && defaults != nil {
			req.Spec.Resources = &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{},
			}
			if defaults.Spec.DefaultCPU != "" {
				req.Spec.Resources.Requests[corev1.ResourceCPU] = resource.MustParse(defaults.Spec.DefaultCPU)
			}
			if defaults.Spec.DefaultMemory != "" {
				req.Spec.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(defaults.Spec.DefaultMemory)
			}
		}
		if req.Spec.Resources != nil && defaults != nil {
			if _, ok := req.Spec.Resources.Requests[corev1.ResourceCPU]; !ok && defaults.Spec.DefaultCPU != "" {
				if req.Spec.Resources.Requests == nil {
					req.Spec.Resources.Requests = corev1.ResourceList{}
				}
				req.Spec.Resources.Requests[corev1.ResourceCPU] = resource.MustParse(defaults.Spec.DefaultCPU)
			}
			if _, ok := req.Spec.Resources.Requests[corev1.ResourceMemory]; !ok && defaults.Spec.DefaultMemory != "" {
				if req.Spec.Resources.Requests == nil {
					req.Spec.Resources.Requests = corev1.ResourceList{}
				}
				req.Spec.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(defaults.Spec.DefaultMemory)
			}
		}
		if req.Spec.Persistence == nil && defaults != nil && defaults.Spec.DefaultStorage != "" {
			req.Spec.Persistence = &v1alpha1.PersistenceSpec{Size: defaults.Spec.DefaultStorage}
		}
		if (req.Spec.IngressSpec == nil || req.Spec.IngressSpec.Mode == "" || req.Spec.IngressSpec.Mode == "subdomain") &&
			(req.Spec.IngressSpec == nil || req.Spec.IngressSpec.Host == "") &&
			defaults != nil && defaults.Spec.RootDomain != "" {
			if req.Spec.IngressSpec == nil {
				req.Spec.IngressSpec = &v1alpha1.IngressSpec{}
			}
			req.Spec.IngressSpec.Host = req.Name + "." + defaults.Spec.RootDomain
		}
	}

	// Marshal the controller to YAML.
	crBytes, err := yaml.Marshal(req.Controller)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to marshal controller")
		return
	}

	out := string(crBytes)

	// If inline bundle present, marshal it too.
	if req.Bundle != nil {
		cb := v1alpha1.ComposedBundle{
			ObjectMeta: metav1.ObjectMeta{Name: req.Name + "-bundle", Namespace: namespace},
			Spec:       *req.Bundle,
		}
		cb.APIVersion = "varroa.dev/v1alpha1"
		cb.Kind = "ComposedBundle"
		cbBytes, err := yaml.Marshal(cb)
		if err != nil {
			s.writeJSONError(w, http.StatusInternalServerError, "failed to marshal bundle")
			return
		}
		out += "\n---\n" + string(cbBytes)
	}

	w.Header().Set("Content-Type", "application/yaml")
	w.Write([]byte(out))
}

func (s *Server) handleCreateController(w http.ResponseWriter, r *http.Request, cluster, namespace string) {
	ctx := r.Context()

	var req createControllerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	cr := &req.Controller
	if cr.Name == "" {
		s.writeJSONError(w, http.StatusBadRequest, "metadata.name is required")
		return
	}

	// Validate ingress rules — path mode only when creating on local cluster.
	if serr := s.controllerSvc().ValidateIngress(cr.Spec.IngressSpec, true, cluster, cr.Name); serr != nil {
		s.writeServiceError(w, serr)
		return
	}

	claims := auth.ClaimsFromContext(r.Context())
	if !s.deps.Authorizer.CanCreateController(claims, namespace, cr.Name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	cr.Namespace = namespace
	cr.APIVersion = "varroa.dev/v1alpha1"
	cr.Kind = "Controller"

	// Validate existing bundle reference with exact-namespace resolution.
	// ComposedBundles are cluster-local (D6): only check the local API when
	// the create targets this cluster — the target cluster's operator
	// validates remote refs authoritatively (CommandCRUD.HandleCreate).
	if cluster == s.deps.Brood.LocalCluster() {
		if serr := s.controllerSvc().ValidateBundleRef(ctx, cr.Spec.ComposedBundleRef, namespace); serr != nil {
			s.writeServiceError(w, serr)
			return
		}
	}

	// Route via Brood for ALL clusters (uniform mutation). The local
	// direct-apply fallback is gone: multicluster-control-plane requires local
	// mutations to transit operator.<local>.controllers.create like every
	// other cluster, and cmd/bff wires Brood unconditionally and exits on a
	// bus-connect failure — so this branch is the only production path.
	crJSON, _ := json.Marshal(cr)
	var bundleJSON json.RawMessage
	if req.Bundle != nil {
		bundleJSON, _ = json.Marshal(req.Bundle)
	}
	created, checks, err := s.deps.Brood.Create(ctx, cluster, ControllersCreateArgs{Namespace: namespace, Controller: crJSON, Bundle: bundleJSON})
	if err != nil {
		s.deps.Logger.Error("create controller via brood failed", "cluster", cluster, "namespace", namespace, "name", cr.Name, "error", err)
		writeBroodErrorChecks(w, err, checks)
		return
	}
	if s.deps.ActivityStore != nil && ActorFrom(claims) != "" {
		s.notifyActivity(activity.Event{
			Type:       "controller.created",
			Source:     "user",
			Actor:      ActorFrom(claims),
			Controller: cr.Name,
			Namespace:  namespace,
			Cluster:    cluster,
			Message:    fmt.Sprintf("controller %s/%s created by %s", namespace, cr.Name, ActorFrom(claims)),
		})
	}
	s.writeJSON(w, http.StatusCreated, s.controllerDetail(created, cluster))
}

// handleDeleteController handles DELETE /controllers/{ns}/{name}.
func (s *Server) handleDeleteController(w http.ResponseWriter, r *http.Request, cluster, namespace, name string) {
	ctx := r.Context()

	claims := auth.ClaimsFromContext(r.Context())
	if !s.deps.Authorizer.CanDeleteController(claims, namespace, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	if s.deps.Brood != nil {
		if err := s.deps.Brood.Delete(ctx, cluster, namespace, name); err != nil {
			s.deps.Logger.Error("delete controller failed", "cluster", cluster, "namespace", namespace, "name", name, "error", err)
			writeBroodError(w, err)
			return
		}
	} else {
		if err := crdstore.Delete[v1alpha1.Controller](ctx, s.deps.Store, name, namespace); err != nil {
			s.deps.Logger.Error("delete controller failed", "namespace", namespace, "name", name, "error", err)
			s.writeJSONError(w, k8sErrorToHTTP(err), "failed to delete controller")
			return
		}
	}

	// Emit activity event.
	if s.deps.ActivityStore != nil {
		if claims != nil {
			s.notifyActivity(activity.Event{
				Type:       "controller.deleted",
				Source:     "user",
				Actor:      ActorFrom(claims),
				Controller: name,
				Namespace:  namespace,
				Cluster:    cluster,
				Message:    fmt.Sprintf("controller %s/%s deleted by %s", namespace, name, ActorFrom(claims)),
			})
		}
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleUpdateController handles PATCH /controllers/{ns}/{name}.
// Uses server-side apply (SSA) with field-manager conflict detection.
func (s *Server) handleUpdateController(w http.ResponseWriter, r *http.Request, cluster, namespace, name string) {
	ctx := r.Context()

	// Parse ?force=true for conflict override.
	force := r.URL.Query().Get("force") == "true"

	// Decode the patch up front so the "manage" permission can be scoped to
	// the admin-control surface (power/ingress) only.
	var patch map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// N5: Only the "spec" top-level key is allowed in patches.
	for k := range patch {
		if k != "spec" {
			s.writeJSONError(w, http.StatusBadRequest, "patch may only contain spec")
			return
		}
	}

	claims := auth.ClaimsFromContext(r.Context())

	// "update" authorizes any patch to the Controller. "manage" is narrower: it
	// only authorizes patches limited to spec.powerState / spec.ingressSpec, so
	// an operator with manage-but-not-update cannot rewrite version, resources,
	// bundles, etc. via this generic endpoint.
	if !s.deps.Authorizer.CanUpdateController(claims, namespace, name) {
		if !s.deps.Authorizer.CanManageController(claims, namespace, name) || !patchWithinManageScope(patch) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
	}

	// Route via Brood for all clusters (uniform mutation). The local
	// direct-apply fallback is gone: multicluster-control-plane requires local
	// mutations to transit the operator like every other cluster.
	patchJSON, _ := json.Marshal(patch)
	updated, _, unapplied, err := s.deps.Brood.Update(ctx, cluster, namespace, name, patchJSON, "varroa-ui", force)
	if err != nil {
		s.deps.Logger.Error("patch controller via brood failed", "cluster", cluster, "namespace", namespace, "name", name, "error", err)
		var fe *BroodError
		if errors.As(err, &fe) && len(fe.Conflicts) > 0 {
			writeBroodErrorConflicts(w, err, fe.Conflicts)
		} else {
			writeBroodError(w, err)
		}
		return
	}
	detail := s.controllerDetail(updated, cluster)
	detail.UnappliedRemovals = unapplied
	s.writeJSON(w, http.StatusOK, detail)
}

// manageScopeIngressFields are the ingressSpec subfields the "manage"
// permission may set. annotations/ingressClassName are policy-bearing knobs
// (ingress-controller behavior, cert-manager integration, snippet injection)
// applied verbatim to the live Ingress object, so changing them requires the
// broader "update" permission even though the rest of ingressSpec is
// manage-scoped.
var manageScopeIngressFields = map[string]bool{
	"host":          true,
	"mode":          true,
	"tlsSecretName": true,
}

// patchWithinManageScope reports whether a controller merge-patch only touches
// fields the "manage" permission is allowed to change: spec.powerState and the
// manage-scoped subset of spec.ingressSpec (see manageScopeIngressFields).
// Identity fields (apiVersion/kind) are tolerated since the merge ignores
// them. Any other top-level key (including metadata) or any other spec field
// requires the broader "update" permission.
func patchWithinManageScope(patch map[string]interface{}) bool {
	for k, v := range patch {
		switch k {
		case "apiVersion", "kind":
			// identity fields, ignored by the merge
		case "spec":
			spec, ok := v.(map[string]interface{})
			if !ok {
				return false
			}
			for sk, sv := range spec {
				switch sk {
				case "powerState":
				case "ingressSpec":
					if sv == nil {
						continue
					}
					ingressSpec, ok := sv.(map[string]interface{})
					if !ok {
						return false
					}
					for isk := range ingressSpec {
						if !manageScopeIngressFields[isk] {
							return false
						}
					}
				default:
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

// k8sErrorToHTTP maps Kubernetes API errors to appropriate HTTP status codes.
func k8sErrorToHTTP(err error) int {
	if k8serrors.IsNotFound(err) {
		return http.StatusNotFound
	}
	if k8serrors.IsAlreadyExists(err) || k8serrors.IsConflict(err) {
		return http.StatusConflict
	}
	if k8serrors.IsInvalid(err) || k8serrors.IsBadRequest(err) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.deps.Logger.Error("JSON encode error", "error", err)
	}
}

func (s *Server) writeJSONError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"error": msg})
}

// writeBroodError maps a BroodError to its HTTP status per §2.1 and
// ErrClusterUnreachable to 502.
func writeBroodError(w http.ResponseWriter, err error) {
	writeBroodErrorChecks(w, err, nil)
}

// writeBroodErrorChecks is writeBroodError carrying the target cluster's
// preflight checks (create/preflight paths) so callers see WHICH check
// failed — the contract's "remote preflight failure returns target-cluster
// checks" scenario.
func writeBroodErrorChecks(w http.ResponseWriter, err error, checks []bus.Check) {
	body := func(msg string) map[string]interface{} {
		out := map[string]interface{}{"error": msg}
		if len(checks) > 0 {
			out["checks"] = checks
		}
		return out
	}
	var fe *BroodError
	if errors.As(err, &fe) {
		switch fe.Code {
		case bus.CodeNotFound:
			writeJSON(w, http.StatusNotFound, body(fe.Msg))
		case bus.CodeConflict:
			writeJSON(w, http.StatusConflict, body(fe.Msg))
		case bus.CodeDraining:
			writeJSON(w, http.StatusConflict, body(fe.Msg))
		case bus.CodeInvalid:
			writeJSON(w, http.StatusBadRequest, body(fe.Msg))
		default:
			writeJSON(w, http.StatusInternalServerError, body(fe.Msg))
		}
		return
	}
	var ec *ErrClusterUnreachable
	if errors.As(err, &ec) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "cluster " + ec.Cluster + " unreachable"})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

// writeBroodErrorConflicts is writeBroodError carrying SSA field conflicts
// so the caller sees WHICH fields are in conflict. Mapped to 409 Conflict.
func writeBroodErrorConflicts(w http.ResponseWriter, err error, conflicts []bus.FieldConflict) {
	msg := err.Error()
	body := map[string]interface{}{
		"error":     msg,
		"conflicts": conflicts,
	}
	writeJSON(w, http.StatusConflict, body)
}

// requireLocalCluster checks if the given cluster is the local cluster.
// If not, it writes a 501 response for the given operation and returns false.
// Brood is always wired in production (cmd/bff); the nil check only
// accommodates handler tests that don't construct one.
func (s *Server) requireLocalCluster(w http.ResponseWriter, cluster, op string) bool {
	if s.deps.Brood == nil {
		return true
	}
	if cluster != s.deps.Brood.LocalCluster() {
		s.writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": op + " is not available for remote clusters",
		})
		return false
	}
	return true
}

// itemsEnvelope wraps a list in the uniform {"items": [...]} collection
// envelope (N6), guaranteeing items is never null: a nil slice (or nil
// interface) marshals as [] instead.
func itemsEnvelope(list interface{}) map[string]interface{} {
	if list == nil {
		return map[string]interface{}{"items": []interface{}{}}
	}
	if rv := reflect.ValueOf(list); rv.Kind() == reflect.Slice && rv.IsNil() {
		return map[string]interface{}{"items": []interface{}{}}
	}
	return map[string]interface{}{"items": list}
}

// writeK8sError logs and writes a Kubernetes API error as JSON.
func (s *Server) writeK8sError(w http.ResponseWriter, err error) {
	s.deps.Logger.Error("kubernetes error", "error", err)
	s.writeJSON(w, k8sErrorToHTTP(err), map[string]string{"error": err.Error()})
}

// ---------------------------------------------------------------------------
// Catalog handlers
// ---------------------------------------------------------------------------

// handleCatalogSources handles GET (list) on /catalogsources.
// handleComposedBundles handles GET (list) on /composedbundles.
func (s *Server) observabilityIntentAnnotations(ctx context.Context, cluster string, cr *v1alpha1.Controller) map[string]string {
	// A nil composedBundleRef resolves to the starter bundle rather than to "no
	// bundle", so returning early here would silently omit any observability
	// annotations the starter carries from a zero-config controller's model.
	bundleName, lookupNS := v1alpha1.EffectiveBundleRef(cr, s.deps.OperatorNamespace)
	if bundleName == "" {
		return map[string]string{}
	}
	cb, err := s.observabilityComposedBundle(ctx, cluster, lookupNS, bundleName)
	if err != nil || cb == nil {
		if err != nil && s.deps.Logger != nil {
			s.deps.Logger.Warn("observability bundle lookup failed",
				"cluster", cluster,
				"namespace", lookupNS,
				"controller", cr.Name,
				"bundle", bundleName,
				"error", err)
		}
		return map[string]string{}
	}

	providerValues := []string{}
	capabilityValues := []string{}
	if value := cb.GetAnnotations()[observability.AnnotationProviders]; value != "" {
		providerValues = append(providerValues, value)
	}
	if value := cb.GetAnnotations()[observability.AnnotationCapabilities]; value != "" {
		capabilityValues = append(capabilityValues, value)
	}
	for _, input := range cb.Spec.Inputs {
		if input.ItemRef == nil || input.ItemRef.Name == "" {
			continue
		}
		lookupNS := cb.Namespace
		if input.ItemRef.Namespace != "" {
			lookupNS = input.ItemRef.Namespace
		}
		item, itemErr := s.observabilityCatalogItem(ctx, cluster, lookupNS, input.ItemRef.Name)
		if itemErr != nil || item == nil {
			if itemErr != nil && s.deps.Logger != nil {
				s.deps.Logger.Warn("observability catalog item lookup failed",
					"cluster", cluster,
					"bundleNamespace", cb.Namespace,
					"bundle", cb.Name,
					"item", input.ItemRef.Name,
					"error", itemErr)
			}
			continue
		}
		if value := item.GetAnnotations()[observability.AnnotationProviders]; value != "" {
			providerValues = append(providerValues, value)
		}
		if value := item.GetAnnotations()[observability.AnnotationCapabilities]; value != "" {
			capabilityValues = append(capabilityValues, value)
		}
	}

	result := map[string]string{}
	if len(providerValues) > 0 {
		result[observability.AnnotationProviders] = strings.Join(providerValues, ",")
	}
	if len(capabilityValues) > 0 {
		result[observability.AnnotationCapabilities] = strings.Join(capabilityValues, ",")
	}
	return result
}

// observabilityComposedBundle resolves a ComposedBundle for observability-intent
// derivation. When ConfigBrood is wired it routes through it so a controller on
// a remote (hive) cluster reads its bundle over the bus (the ConfigBrood local
// branch still uses the typed core client). When ConfigBrood is nil
// (legacy/test setups) it falls back to the local typed client.
func (s *Server) observabilityComposedBundle(ctx context.Context, cluster, ns, name string) (*v1alpha1.ComposedBundle, error) {
	if s.deps.ConfigBrood == nil {
		return crdstore.Get[v1alpha1.ComposedBundle](ctx, s.deps.Store, name, ns)
	}
	raw, err := s.deps.ConfigBrood.GetComposedBundle(ctx, cluster, ns, name)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var cb v1alpha1.ComposedBundle
	if err := json.Unmarshal(raw, &cb); err != nil {
		return nil, fmt.Errorf("decode composedbundle: %w", err)
	}
	return &cb, nil
}

// observabilityCatalogItem resolves a CatalogItem for observability-intent
// derivation, mirroring observabilityComposedBundle's ConfigBrood-or-local
// routing so remote-cluster catalog items resolve over the bus.
func (s *Server) observabilityCatalogItem(ctx context.Context, cluster, ns, name string) (*v1alpha1.CatalogItem, error) {
	if s.deps.ConfigBrood == nil {
		return crdstore.Get[v1alpha1.CatalogItem](ctx, s.deps.Store, name, ns)
	}
	raw, err := s.deps.ConfigBrood.GetCatalogItem(ctx, cluster, ns, name)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var item v1alpha1.CatalogItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, fmt.Errorf("decode catalogitem: %w", err)
	}
	return &item, nil
}

// handleUpdateComposedBundle handles PUT /composedbundles/{ns}/{name}.
func (s *Server) handleVarroaRoles(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())

	switch r.Method {
	case http.MethodGet:
		if claims == nil || !s.deps.Authorizer.CanReadRoles(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		roles, err := crdstore.List[v1alpha1.VarroaRole](r.Context(), s.deps.Store, "", "")
		if err != nil {
			s.writeK8sError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, itemsEnvelope(roles))

	case http.MethodPost:
		if !s.deps.Authorizer.CanCreateRole(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		var role v1alpha1.VarroaRole
		if err := json.NewDecoder(r.Body).Decode(&role); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		role.APIVersion = "varroa.dev/v1alpha1"
		role.Kind = "VarroaRole"
		if err := crdstore.Apply[v1alpha1.VarroaRole](r.Context(), s.deps.Store, &role); err != nil {
			s.writeK8sError(w, err)
			return
		}
		if s.deps.ActivityStore != nil && claims != nil {
			s.notifyActivity(activity.Event{
				Type:    "varroarole.created",
				Source:  "user",
				Actor:   ActorFrom(claims),
				Message: fmt.Sprintf("VarroaRole %s created by %s", role.Name, ActorFrom(claims)),
			})
		}
		s.writeJSON(w, http.StatusCreated, role)

	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleVarroaRoleDispatch handles GET/PUT/DELETE on /roles/{name}.
func (s *Server) handleVarroaRoleDispatch(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/roles/") {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(path, "/roles/")
	if name == "" || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}

	claims := auth.ClaimsFromContext(r.Context())

	switch r.Method {
	case http.MethodGet:
		if claims == nil || !s.deps.Authorizer.CanReadRoles(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		role, err := crdstore.Get[v1alpha1.VarroaRole](r.Context(), s.deps.Store, name, "")
		if err != nil {
			s.writeK8sError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, role)

	case http.MethodPut:
		if !s.deps.Authorizer.CanUpdateRole(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		var role v1alpha1.VarroaRole
		if err := json.NewDecoder(r.Body).Decode(&role); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		role.Name = name
		role.APIVersion = "varroa.dev/v1alpha1"
		role.Kind = "VarroaRole"
		if err := crdstore.Apply[v1alpha1.VarroaRole](r.Context(), s.deps.Store, &role); err != nil {
			s.writeK8sError(w, err)
			return
		}
		if s.deps.ActivityStore != nil && claims != nil {
			s.notifyActivity(activity.Event{
				Type:    "varroarole.updated",
				Source:  "user",
				Actor:   ActorFrom(claims),
				Message: fmt.Sprintf("VarroaRole %s updated by %s", role.Name, ActorFrom(claims)),
			})
		}
		s.writeJSON(w, http.StatusOK, role)

	case http.MethodDelete:
		if !s.deps.Authorizer.CanDeleteRole(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		if err := crdstore.Delete[v1alpha1.VarroaRole](r.Context(), s.deps.Store, name, ""); err != nil {
			s.writeK8sError(w, err)
			return
		}
		if s.deps.ActivityStore != nil && claims != nil {
			s.notifyActivity(activity.Event{
				Type:    "varroarole.deleted",
				Source:  "user",
				Actor:   ActorFrom(claims),
				Message: fmt.Sprintf("VarroaRole %s deleted by %s", name, ActorFrom(claims)),
			})
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// VarroaRoleBinding handlers
// ---------------------------------------------------------------------------

// handleVarroaRoleBindings handles GET (list) and POST (create) on /rolebindings.
func (s *Server) handleVarroaRoleBindings(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())

	switch r.Method {
	case http.MethodGet:
		if claims == nil || !s.deps.Authorizer.CanReadRoleBindings(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		bindings, err := crdstore.List[v1alpha1.VarroaRoleBinding](r.Context(), s.deps.Store, "", "")
		if err != nil {
			s.writeK8sError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, itemsEnvelope(bindings))

	case http.MethodPost:
		if !s.deps.Authorizer.CanCreateRoleBinding(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		var binding v1alpha1.VarroaRoleBinding
		if err := json.NewDecoder(r.Body).Decode(&binding); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		binding.APIVersion = "varroa.dev/v1alpha1"
		binding.Kind = "VarroaRoleBinding"
		if err := crdstore.Apply[v1alpha1.VarroaRoleBinding](r.Context(), s.deps.Store, &binding); err != nil {
			s.writeK8sError(w, err)
			return
		}
		if s.deps.ActivityStore != nil && claims != nil {
			s.notifyActivity(activity.Event{
				Type:    "varroarolebinding.created",
				Source:  "user",
				Actor:   ActorFrom(claims),
				Message: fmt.Sprintf("VarroaRoleBinding %s created by %s", binding.Name, ActorFrom(claims)),
			})
		}
		s.writeJSON(w, http.StatusCreated, binding)

	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleVarroaRoleBindingDispatch handles GET/PUT/DELETE on /rolebindings/{name}.
func (s *Server) handleVarroaRoleBindingDispatch(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/rolebindings/") {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(path, "/rolebindings/")
	if name == "" || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}

	claims := auth.ClaimsFromContext(r.Context())

	switch r.Method {
	case http.MethodGet:
		if claims == nil || !s.deps.Authorizer.CanReadRoleBindings(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		binding, err := crdstore.Get[v1alpha1.VarroaRoleBinding](r.Context(), s.deps.Store, name, "")
		if err != nil {
			s.writeK8sError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, binding)

	case http.MethodPut:
		if !s.deps.Authorizer.CanUpdateRoleBinding(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		var binding v1alpha1.VarroaRoleBinding
		if err := json.NewDecoder(r.Body).Decode(&binding); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		binding.Name = name
		binding.APIVersion = "varroa.dev/v1alpha1"
		binding.Kind = "VarroaRoleBinding"
		if err := crdstore.Apply[v1alpha1.VarroaRoleBinding](r.Context(), s.deps.Store, &binding); err != nil {
			s.writeK8sError(w, err)
			return
		}
		if s.deps.ActivityStore != nil && claims != nil {
			s.notifyActivity(activity.Event{
				Type:    "varroarolebinding.updated",
				Source:  "user",
				Actor:   ActorFrom(claims),
				Message: fmt.Sprintf("VarroaRoleBinding %s updated by %s", binding.Name, ActorFrom(claims)),
			})
		}
		s.writeJSON(w, http.StatusOK, binding)

	case http.MethodDelete:
		if !s.deps.Authorizer.CanDeleteRoleBinding(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		if err := crdstore.Delete[v1alpha1.VarroaRoleBinding](r.Context(), s.deps.Store, name, ""); err != nil {
			s.writeK8sError(w, err)
			return
		}
		if s.deps.ActivityStore != nil && claims != nil {
			s.notifyActivity(activity.Event{
				Type:    "varroarolebinding.deleted",
				Source:  "user",
				Actor:   ActorFrom(claims),
				Message: fmt.Sprintf("VarroaRoleBinding %s deleted by %s", name, ActorFrom(claims)),
			})
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// JenkinsRole handlers
// ---------------------------------------------------------------------------

// handleJenkinsRoles handles GET (list) and POST (create) on /jenkinsroles.
func (s *Server) handleMePermissions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	perms := s.deps.Authorizer.EffectivePermissions(claims)
	s.writeJSON(w, http.StatusOK, perms)
}

// handleControllerDiff handles GET /controllers/{ns}/{name}/diff.
func (s *Server) handleControllerDiff(w http.ResponseWriter, r *http.Request, cluster, namespace, name string) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireLocalCluster(w, cluster, "diff") {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadController(claims, namespace, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	ctx := r.Context()
	cr, err := crdstore.Get[v1alpha1.Controller](ctx, s.deps.Store, name, namespace)
	if err != nil {
		s.writeJSONError(w, http.StatusNotFound, "controller not found")
		return
	}

	// Resolve incoming content from the bundle mirroring the operator's
	// resolveBundleForController as closely as the BFF context allows.
	type content struct {
		Jcasc   string `json:"jcasc"`
		Items   string `json:"items"`
		Plugins string `json:"plugins"`
	}
	incoming := content{}

	// A nil composedBundleRef resolves to the built-in starter bundle, exactly as
	// it does in the reconciler. Skipping resolution here would show an empty
	// incoming configuration for a zero-config controller that is in fact running
	// the starter — the diff would claim Varroa intends to apply nothing.
	if s.deps.Client != nil {
		bundleName, bundleNS := v1alpha1.EffectiveBundleRef(cr, s.deps.OperatorNamespace)
		cb, cbErr := crdstore.Get[v1alpha1.ComposedBundle](ctx, s.deps.Store, bundleName, bundleNS)
		if cbErr == nil && cb != nil && cb.Status.ContentRef != "" {
			data, cmErr := s.deps.Client.GetConfigMap(ctx, cb.Status.ContentRef, cb.Namespace)
			if cmErr == nil {
				jcasc := data["jenkins.yaml"]
				items := data["items.yaml"]
				plugins := data["plugins.yaml"]

				vars := make(bundle.Variables)
				if varYAML := data["variables.yaml"]; varYAML != "" {
					for _, line := range strings.Split(varYAML, "\n") {
						line = strings.TrimSpace(line)
						if line == "" || strings.HasPrefix(line, "#") {
							continue
						}
						parts := strings.SplitN(line, ":", 2)
						if len(parts) == 2 {
							vars[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
						}
					}
				}
				// Inject varroa_controller_* vars (subset available to BFF).
				vars["varroa_controller_name"] = name
				vars["varroa_controller_namespace"] = namespace
				vars["varroa_controller_version"] = cr.Spec.Version
				extURL := ""
				pathPref := ""
				if cr.Spec.IngressSpec != nil {
					host := cr.Spec.IngressSpec.Host
					if host != "" {
						if cr.Spec.IngressSpec.RoutingMode() == v1alpha1.RoutingModePath {
							pathPref = v1alpha1.PathPrefix(namespace, name)
							extURL = "https://" + host + pathPref
						} else {
							extURL = "https://" + host
						}
						vars["varroa_controller_external_url"] = extURL
						vars["varroa_controller_path_prefix"] = pathPref
					}
				}

				if jcasc != "" {
					jcasc = bundle.ResolveVars(jcasc, vars)
					// Apply path-mode location URL injection mirroring the operator.
					if pathPref != "" && extURL != "" {
						injected, _, _ := bundle.InjectLocationURL(jcasc, extURL+"/")
						if injected != "" {
							jcasc = injected
						}
					}
				}
				if items != "" {
					items = bundle.ResolveVars(items, vars)
				}

				incoming.Jcasc = jcasc
				incoming.Items = items
				// Build plugin list: parse plugins.yaml as YAML entries, respect
				// PluginSpec override, format as artifactId:version lines.
				var pluginLines []string
				if cr.Spec.PluginSpec != nil && len(cr.Spec.PluginSpec.Entries) > 0 {
					for _, e := range cr.Spec.PluginSpec.Entries {
						line := e.ArtifactId
						if e.Version != "" {
							line += ":" + e.Version
						}
						pluginLines = append(pluginLines, line)
					}
				} else if plugins != "" {
					// Parse plugins.yaml: YAML list of {id, artifactId, version} entries.
					entries := parsePluginYAML(plugins)
					for _, e := range entries {
						line := e.artifact
						if e.version != "" {
							line += ":" + e.version
						}
						pluginLines = append(pluginLines, line)
					}
				}
				incoming.Plugins = strings.Join(pluginLines, "\n")
			}
		}
	}

	// Fetch applied content from the mite.
	var applied *content
	appliedUnavailable := false
	if s.deps.MiteRegistry != nil {
		resp, fetchErr := s.deps.MiteRegistry.FetchLastApplied(ctx, namespace, name)
		if fetchErr != nil {
			appliedUnavailable = true
		} else if resp != nil {
			applied = &content{
				Jcasc:   resp.JcascYaml,
				Items:   resp.ItemsYaml,
				Plugins: resp.PluginsTxt,
			}
		}
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"incoming":           incoming,
		"applied":            applied,
		"appliedUnavailable": appliedUnavailable,
	})
}

// parsePluginYAML parses a plugins.yaml string (YAML list of plugin entries
// with id/artifactId and version fields) into simple artifact:version pairs.
func parsePluginYAML(yamlContent string) []pluginEntry {
	var raw []map[string]interface{}
	if err := yaml.Unmarshal([]byte(yamlContent), &raw); err != nil {
		return nil
	}
	var out []pluginEntry
	for _, e := range raw {
		artifact, _ := e["id"].(string)
		if a, ok := e["artifactId"].(string); ok && a != "" {
			artifact = a
		}
		if artifact == "" {
			continue
		}
		version, _ := e["version"].(string)
		out = append(out, pluginEntry{artifact: artifact, version: version})
	}
	return out
}

type pluginEntry struct {
	artifact string
	version  string
}

// previewRequest is the JSON body for POST /controllers/{ns}/{name}/preview.
type previewRequest struct {
	PodOverrides    *v1alpha1.PodOverrides    `json:"podOverrides"`
	Probes          *v1alpha1.ProbesSpec      `json:"probes"`
	ResourceOverlay *v1alpha1.ResourceOverlay `json:"resourceOverlay"`
	Baseline        string                    `json:"baseline"`
}

func (s *Server) handlePreviewController(w http.ResponseWriter, r *http.Request, cluster, namespace, name string) {
	if !s.requireLocalCluster(w, cluster, "preview") {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	cr, crErr := crdstore.Get[v1alpha1.Controller](r.Context(), s.deps.Store, name, namespace)
	if crErr != nil && !k8serrors.IsNotFound(crErr) {
		s.writeJSONError(w, http.StatusInternalServerError, crErr.Error())
		return
	}
	crExists := crErr == nil && cr != nil

	// Normally previewing requires "update" rights on the (existing) controller.
	// But a controller that doesn't exist yet (e.g. previewed from the create
	// wizard) has nothing live to leak or mutate, so a "create" grant suffices.
	// A transient/non-NotFound lookup error above already returns 500 rather
	// than falling through here, so crExists=false only ever means "confirmed
	// absent".
	authorized := s.deps.Authorizer != nil && s.deps.Authorizer.CanUpdateController(claims, namespace, name)
	if !authorized && !crExists && s.deps.Authorizer != nil {
		authorized = s.deps.Authorizer.CanCreateController(claims, namespace, name)
	}
	if !authorized {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req previewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid body: %s", err))
		return
	}
	if req.Baseline == "" {
		req.Baseline = "live"
	}

	// Validate (patch-parse only): any parse failure → 400.
	if err := validatePreviewPatches(req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("%s", err))
		return
	}

	// Build base resources (live or skeleton).
	stsGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	svcGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}
	ingGVR := schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}

	// Use the controller CR (fetched above for authorization) for Service/Ingress naming.
	var prefix string
	if crExists {
		// PodName = "<prefix>-0", so strip the "-0" suffix.
		prefix = strings.TrimSuffix(controller.PodName(cr, 0), "-0")
	} else {
		// No CR — use the name as StatefulSet name, skip service/ingress naming.
		prefix = name
	}
	svcName := prefix + "-svc"
	ingressName := prefix + "-ingress"

	// Result accumulator.
	result := struct {
		Merged       map[string]string `json:"merged"`
		Diff         map[string]string `json:"diff"`
		Warnings     []warningItem     `json:"warnings"`
		BaselineUsed string            `json:"baselineUsed"`
	}{
		Merged:       make(map[string]string),
		Diff:         make(map[string]string),
		BaselineUsed: "base",
	}

	useLive := req.Baseline != "base"

	// --- StatefulSet ---
	stsResult := s.previewResource(r.Context(), previewResourceArgs{
		gvr:       stsGVR,
		name:      name,
		namespace: namespace,
		useLive:   useLive,
		skeleton:  &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": map[string]interface{}{"name": name, "namespace": namespace}}},
	})

	// StatefulSet gets both podOverrides and raw overlay.
	stsPatch, _ := overlay.CompilePodOverrides(req.PodOverrides, overlay.JenkinsContainerName)
	if len(stsPatch) > 0 {
		var err error
		stsResult.merged, err = overlay.Merge(stsResult.merged, stsPatch, appsv1.StatefulSet{})
		if err != nil {
			stsResult.err = err
		}
	}
	if req.ResourceOverlay != nil && req.ResourceOverlay.StatefulSet != "" {
		var err error
		stsResult.merged, err = overlay.Merge(stsResult.merged, []byte(req.ResourceOverlay.StatefulSet), appsv1.StatefulSet{})
		if err != nil {
			stsResult.err = err
		}
	}

	// Emit the StatefulSet when there is a live base OR the request targets it via
	// podOverrides / a statefulSet overlay (so create-flow previews with no live
	// object still show the override's effect).
	hasStsOverlay := len(stsPatch) > 0 || (req.ResourceOverlay != nil && req.ResourceOverlay.StatefulSet != "")
	if stsResult.err == nil && (stsResult.hasContent || hasStsOverlay) {
		result.Merged["statefulSet"] = mustMarshalYAML(stsResult.merged.Object)
		result.Diff["statefulSet"] = diffYAML(stsResult.baseYAML, result.Merged["statefulSet"])
		if stsResult.usedLive {
			result.BaselineUsed = "live"
		}
	}

	// --- Service ---
	svcResult := s.previewResource(r.Context(), previewResourceArgs{
		gvr:       svcGVR,
		name:      svcName,
		namespace: namespace,
		useLive:   useLive,
		skeleton:  &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "Service", "metadata": map[string]interface{}{"name": svcName, "namespace": namespace}}},
	})
	hasSvcOverlay := req.ResourceOverlay != nil && req.ResourceOverlay.Service != ""
	if svcResult.hasContent || hasSvcOverlay {
		if hasSvcOverlay {
			var err error
			svcResult.merged, err = overlay.Merge(svcResult.merged, []byte(req.ResourceOverlay.Service), corev1.Service{})
			if err != nil {
				svcResult.err = err
			}
		}
		if svcResult.err == nil {
			result.Merged["service"] = mustMarshalYAML(svcResult.merged.Object)
			result.Diff["service"] = diffYAML(svcResult.baseYAML, result.Merged["service"])
			if svcResult.usedLive {
				result.BaselineUsed = "live"
			}
		}
	}

	// --- Ingress ---
	ingResult := s.previewResource(r.Context(), previewResourceArgs{
		gvr:       ingGVR,
		name:      ingressName,
		namespace: namespace,
		useLive:   useLive,
		skeleton:  &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress", "metadata": map[string]interface{}{"name": ingressName, "namespace": namespace}}},
	})
	hasIngOverlay := req.ResourceOverlay != nil && req.ResourceOverlay.Ingress != ""
	if ingResult.hasContent || hasIngOverlay {
		if hasIngOverlay {
			var err error
			ingResult.merged, err = overlay.Merge(ingResult.merged, []byte(req.ResourceOverlay.Ingress), &networkingv1.Ingress{})
			if err != nil {
				ingResult.err = err
			}
		}
		if ingResult.err == nil {
			result.Merged["ingress"] = mustMarshalYAML(ingResult.merged.Object)
			result.Diff["ingress"] = diffYAML(ingResult.baseYAML, result.Merged["ingress"])
			if ingResult.usedLive {
				result.BaselineUsed = "live"
			}
		}
	}

	// Warnings.
	ws, _ := overlay.ScanOverrides(req.PodOverrides, req.ResourceOverlay)
	for _, w := range ws {
		result.Warnings = append(result.Warnings, warningItem{
			Resource: w.Resource,
			Path:     w.Path,
			Message:  w.Message,
		})
	}

	s.writeJSON(w, http.StatusOK, result)
}

type warningItem struct {
	Resource string `json:"resource"`
	Path     string `json:"path"`
	Message  string `json:"message"`
}

type previewResourceArgs struct {
	gvr       schema.GroupVersionResource
	name      string
	namespace string
	useLive   bool
	skeleton  *unstructured.Unstructured
}

type previewResourceResult struct {
	merged     *unstructured.Unstructured
	baseYAML   string
	hasContent bool
	usedLive   bool
	err        error
}

func (s *Server) previewResource(ctx context.Context, args previewResourceArgs) previewResourceResult {
	result := previewResourceResult{
		merged: args.skeleton,
	}

	if args.useLive {
		live, err := s.deps.Client.GetLiveResource(ctx, args.gvr, args.name, args.namespace)
		if err == nil && live != nil {
			result.merged = live
			result.usedLive = true
			result.hasContent = true
		}
	}

	// If we ended up with a skeleton (no live or base requested), mark it as
	// having content only if we want to show it (i.e. when there's an overlay).
	// We set hasContent to true only when we have a live object; otherwise the
	// caller checks this flag + whether there's an overlay.
	result.baseYAML = mustMarshalYAML(result.merged.Object)

	// Deep-copy the base for merging.
	baseJSON, _ := json.Marshal(result.merged.Object)
	var baseCopy map[string]interface{}
	_ = json.Unmarshal(baseJSON, &baseCopy)
	result.merged = &unstructured.Unstructured{Object: baseCopy}

	return result
}

// validatePreviewPatches runs ParsePatch on each non-empty overlay field and
// on the compiled podOverrides patch.
func validatePreviewPatches(req previewRequest) error {
	if req.ResourceOverlay != nil {
		if req.ResourceOverlay.StatefulSet != "" {
			if err := overlay.ParsePatch([]byte(req.ResourceOverlay.StatefulSet), appsv1.StatefulSet{}); err != nil {
				return fmt.Errorf("resourceOverlay.statefulSet: %s", err)
			}
		}
		if req.ResourceOverlay.Service != "" {
			if err := overlay.ParsePatch([]byte(req.ResourceOverlay.Service), corev1.Service{}); err != nil {
				return fmt.Errorf("resourceOverlay.service: %s", err)
			}
		}
		if req.ResourceOverlay.Ingress != "" {
			if err := overlay.ParsePatch([]byte(req.ResourceOverlay.Ingress), &networkingv1.Ingress{}); err != nil {
				return fmt.Errorf("resourceOverlay.ingress: %s", err)
			}
		}
	}
	if req.PodOverrides != nil {
		poPatch, err := overlay.CompilePodOverrides(req.PodOverrides, overlay.JenkinsContainerName)
		if err != nil {
			return fmt.Errorf("compile podOverrides: %s", err)
		}
		if len(poPatch) > 0 {
			if err := overlay.ParsePatch(poPatch, appsv1.StatefulSet{}); err != nil {
				return fmt.Errorf("podOverrides: %s", err)
			}
		}
	}
	return nil
}

// mustMarshalYAML marshals v to YAML, returning an empty string on error.
func mustMarshalYAML(v interface{}) string {
	b, err := yaml.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// diffYAML returns a simple line-based diff of two YAML strings. If both are
// equal, returns "".
func diffYAML(base, merged string) string {
	if base == merged {
		return ""
	}
	// Simple line-based diff: prefix with +/-.
	baseLines := strings.Split(base, "\n")
	mergedLines := strings.Split(merged, "\n")
	// If the last line is empty, drop it for cleaner output.
	if len(baseLines) > 0 && baseLines[len(baseLines)-1] == "" {
		baseLines = baseLines[:len(baseLines)-1]
	}
	if len(mergedLines) > 0 && mergedLines[len(mergedLines)-1] == "" {
		mergedLines = mergedLines[:len(mergedLines)-1]
	}

	var buf strings.Builder
	baseSet := make(map[string]int)
	for _, l := range baseLines {
		baseSet[l]++
	}

	// Mark lines that are only in merged, remove from baseSet when found.
	mergedSet := make(map[string]int)
	for _, l := range mergedLines {
		mergedSet[l]++
	}

	i, j := 0, 0
	for i < len(baseLines) || j < len(mergedLines) {
		if i < len(baseLines) && j < len(mergedLines) && baseLines[i] == mergedLines[j] {
			if baseLines[i] != "" {
				buf.WriteString(" " + baseLines[i] + "\n")
			}
			i++
			j++
		} else if j < len(mergedLines) && (i >= len(baseLines) || baseSet[mergedLines[j]] == 0) {
			mergedSet[mergedLines[j]]--
			buf.WriteString("+" + mergedLines[j] + "\n")
			j++
		} else if i < len(baseLines) && (j >= len(mergedLines) || mergedSet[baseLines[i]] == 0) {
			baseSet[baseLines[i]]--
			buf.WriteString("-" + baseLines[i] + "\n")
			i++
		} else {
			if baseLines[i] != "" {
				buf.WriteString(" " + baseLines[i] + "\n")
			}
			baseSet[baseLines[i]]--
			mergedSet[baseLines[i]]--
			i++
			j++
		}
	}
	return buf.String()
}

// effectiveBundleJSON reports which ComposedBundle a controller is actually
// using, which is not always what its spec names: a nil spec.composedBundleRef
// resolves to the operator-seeded starter bundle.
//
// It is deliberately a separate field from composedBundleRef rather than a
// resolved value written into it. The dashboard PATCHes composedBundleRef to
// attach and detach bundles, so overwriting it with a resolved value would make
// the next save materialize an explicit reference to the starter — turning a
// zero-config controller into a pinned one behind the user's back.
type effectiveBundleJSON struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// BuiltIn is true when the controller resolved to the built-in starter
	// bundle rather than one it names. The UI needs this to say "using the
	// built-in starter" instead of "no bundle attached".
	BuiltIn bool `json:"builtIn"`
}

// effectiveBundleFor builds the read-only effective-bundle view for a controller.
func (s *Server) effectiveBundleFor(cr *v1alpha1.Controller) *effectiveBundleJSON {
	name, ns := v1alpha1.EffectiveBundleRef(cr, s.deps.OperatorNamespace)
	if name == "" {
		return nil
	}
	return &effectiveBundleJSON{
		Name:      name,
		Namespace: ns,
		BuiltIn:   cr.Spec.ComposedBundleRef == nil,
	}
}
