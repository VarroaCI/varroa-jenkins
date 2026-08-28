package controller

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/overlay"
)

// pluginRollDecision is the outcome of the gated plugins-checksum evaluation
// (design §3): whether the desired plugin set rolls onto the StatefulSet this
// tick, and which status bookkeeping the handler must apply.
type pluginRollDecision struct {
	// Bump means the StatefulSet is stamped with the desired checksum.
	Bump bool
	// STSChecksum is the checksum to stamp on the StatefulSet: the desired
	// one when bumping, otherwise the currently-applied one.
	STSChecksum string
	// ClearApproved means Status.ApprovedPluginRollChecksum must be reset —
	// either it went stale (approval for a different checksum) or it was
	// consumed by this bump.
	ClearApproved bool
	// RaisePending means the roll is deferred: PendingPluginRoll bookkeeping
	// and PluginRollPending=True must be applied.
	RaisePending bool
	// ResolvePending means desired == applied: PendingPluginRoll clears and
	// PluginRollPending=False. RaisePending and ResolvePending are mutually
	// exclusive; both false leaves pending state untouched (e.g. a
	// force-reprovision bump onto a different checksum).
	ResolvePending bool
}

// ucGateOutcome is what the update-center gate decided for this tick.
type ucGateOutcome int

const (
	// ucGateClear — UC is Ready or not configured at all: any stale
	// UpdateCenterFallback/WaitingForUpdateCenter conditions clear.
	ucGateClear ucGateOutcome = iota
	// ucGateNoop — UC is configured but unusable, yet explicit
	// ProvisioningDefaults override both URL tiers, so nothing fell through
	// to the UC: conditions stay untouched (§5.2).
	ucGateNoop
	// ucGateFallbackOnline — a URL fell through to an unusable UC and
	// pull-through is enabled: provisioning continues with the fallback
	// condition raised.
	ucGateFallbackOnline
	// ucGateBlockAirgap — a URL fell through to an unusable UC in air-gap
	// mode: provisioning blocks.
	ucGateBlockAirgap
)

// ucGateResult carries the resolved plugin update-center URLs (3-tier
// precedence: explicit defaults > in-cluster UC > upstream) and the gate
// outcome.
type ucGateResult struct {
	URL, DownloadURL string
	Outcome          ucGateOutcome
}

// resolveUpdateCenterGate resolves the plugin update-center URLs and decides
// the §5.2 fallback semantics. Pure: uc is nil when the UpdateCenter CR is
// absent; ucBaseURL is empty when the in-cluster UC component is disabled.
func resolveUpdateCenterGate(defaults *v1alpha1.ProvisioningDefaults, uc *v1alpha1.UpdateCenter, ucBaseURL string) ucGateResult {
	url, downloadURL := resolvePluginUpdateCenterURLs(defaults, uc, ucBaseURL)
	res := ucGateResult{URL: url, DownloadURL: downloadURL, Outcome: ucGateClear}

	ucReady := uc != nil && ucConditionTrue(uc.Status.Conditions, "Ready") && ucBaseURL != ""
	if ucBaseURL == "" || ucReady {
		return res
	}
	// UC configured but not usable — only act when a field actually fell
	// through to the (empty) UC tier.
	urlNeedsUC := defaults == nil || defaults.Spec.PluginUpdateCenterURL == ""
	dlNeedsUC := defaults == nil || defaults.Spec.PluginUpdateCenterDownloadURL == ""
	if !urlNeedsUC && !dlNeedsUC {
		res.Outcome = ucGateNoop
		return res
	}
	if uc != nil && uc.Spec.PullThrough.Enabled {
		res.Outcome = ucGateFallbackOnline
	} else {
		res.Outcome = ucGateBlockAirgap
	}
	return res
}

// computePluginRollGate decides whether the desired plugins checksum rolls
// onto the StatefulSet. Pure: callers pass the raw applied checksum (from the
// live StatefulSet), the approval recorded on status, the effective
// reconciliation mode, and whether the force-reprovision annotation is set.
func computePluginRollGate(desired, applied, approved string, mode v1alpha1.ReconciliationMode, forceReprovision bool) pluginRollDecision {
	var d pluginRollDecision

	// ForceReprovision bypasses the gate (first-case match below).
	if forceReprovision {
		applied = ""
	}

	// Clear stale approval — if the desired set changed, the old approval is
	// no longer valid for a different checksum.
	if approved != "" && approved != desired {
		d.ClearApproved = true
		approved = ""
	}

	switch {
	case applied == "":
		d.Bump = true
	case desired == applied:
		d.Bump = true
	case desired == approved:
		d.Bump = true
	case mode == v1alpha1.ReconciliationModeAutomatic:
		d.Bump = true
	}
	if d.Bump && desired == approved && approved != "" {
		// Approval consumed by this bump.
		d.ClearApproved = true
	}

	d.STSChecksum = applied
	if d.Bump {
		d.STSChecksum = desired
	}

	if !d.Bump && desired != applied {
		d.RaisePending = true
	} else if desired == applied {
		d.ResolvePending = true
	}
	return d
}

// stsBuildInputs carries the handler-computed names and gate results the
// desired-state builder stamps onto the StatefulSet.
type stsBuildInputs struct {
	Name             string
	BootstrapSecret  string
	InitConfigMap    string
	CascConfigMap    string
	PluginsConfigMap string
	PluginsChecksum  string
	Policy           v1alpha1.ReconciliationPolicy
}

// buildStatefulSetSpec derives the desired StatefulSet from the CR and its
// fallback tiers (cr → class → defaults). It performs NO I/O: cr, class,
// defaults, and profile are read-only inputs, and the Reconciler fields
// consulted are static config (Resolver getters, endpoints, cluster name,
// key material). Precedence rules are table-tested in desiredstate_test.go.
func (r *Reconciler) buildStatefulSetSpec(cr *v1alpha1.Controller, class *v1alpha1.ControllerClass, defaults *v1alpha1.ProvisioningDefaults, profile *v1alpha1.JenkinsVersionProfile, in stsBuildInputs) StatefulSetSpec {
	jenkinsImage := jenkinsImageForVersion(cr.Spec.Version, profile)
	miteImage := r.effectiveDesiredMiteImage(cr)
	miteImagePullPolicy := r.effectiveDesiredMiteImagePullPolicy(cr)
	miteResources := r.effectiveDesiredResources(cr, overlay.MiteContainerName)
	jenkinsResources := r.effectiveDesiredResources(cr, overlay.JenkinsContainerName)

	// Determine the source of each container's resources for the annotation stamp.
	resourcesSource := map[string]string{
		overlay.JenkinsContainerName: "none",
		overlay.MiteContainerName:    "none",
	}
	if jenkinsResources != nil {
		resourcesSource[overlay.JenkinsContainerName] = "spec"
	}
	if miteResources != nil {
		resourcesSource[overlay.MiteContainerName] = "spec"
	}
	// Class fallback: cr.Spec.Resources → class.Spec.Resources → nil (no request).
	if jenkinsResources == nil && class != nil && class.Spec.Resources != nil {
		jenkinsResources = class.Spec.Resources
		resourcesSource[overlay.JenkinsContainerName] = "class"
	}

	storageSize := "20Gi"
	storageClass := ""
	var imagePullSecrets []string
	if cr.Spec.Persistence != nil {
		if cr.Spec.Persistence.Size != "" {
			storageSize = cr.Spec.Persistence.Size
		}
		if cr.Spec.Persistence.StorageClass != "" {
			storageClass = cr.Spec.Persistence.StorageClass
		}
	}
	// Class fallback for persistence (whole-struct override: if cr.Spec.Persistence
	// is non-nil, class is not consulted at all; otherwise class supplies defaults).
	if cr.Spec.Persistence == nil && class != nil && class.Spec.Persistence != nil {
		if storageSize == "20Gi" && class.Spec.Persistence.Size != "" {
			storageSize = class.Spec.Persistence.Size
		}
		if storageClass == "" && class.Spec.Persistence.StorageClass != "" {
			storageClass = class.Spec.Persistence.StorageClass
		}
	}
	if defaults != nil && storageClass == "" && defaults.Spec.StorageClass != "" {
		storageClass = defaults.Spec.StorageClass
	}
	// Image pull secrets: class entries first, then ProvisioningDefaults, deduplicated by name.
	if class != nil {
		imagePullSecrets = class.Spec.ImagePullSecrets
	}
	if defaults != nil {
		seen := make(map[string]bool, len(imagePullSecrets)+len(defaults.Spec.ImagePullSecrets))
		for _, s := range imagePullSecrets {
			seen[s] = true
		}
		for _, s := range defaults.Spec.ImagePullSecrets {
			if !seen[s] {
				imagePullSecrets = append(imagePullSecrets, s)
				seen[s] = true
			}
		}
	}

	// Compute path prefix for path-mode routing (also fed to the StatefulSet).
	pathPrefix := ""
	if cr.Spec.IngressSpec != nil && cr.Spec.IngressSpec.RoutingMode() == v1alpha1.RoutingModePath {
		pathPrefix = v1alpha1.PathPrefix(cr.Namespace, cr.Name)
	}

	// Map power state and hibernation to StatefulSet replicas.
	replicas := int32(1)
	if cr.Spec.PowerState == "Stopped" || cr.Status.Hibernated {
		replicas = 0
	}

	// Probes fallback: cr.Spec.Probes → class.Spec.Probes → nil (operator defaults).
	probes := cr.Spec.Probes
	if probes == nil && class != nil && class.Spec.Probes != nil {
		probes = class.Spec.Probes
	}

	return StatefulSetSpec{
		Name:                      in.Name,
		Namespace:                 cr.Namespace,
		ControllerName:            cr.Name,
		JenkinsImage:              jenkinsImage,
		MiteImage:                 miteImage,
		BootstrapSecret:           in.BootstrapSecret,
		InitConfigMap:             in.InitConfigMap,
		CascConfigMap:             in.CascConfigMap,
		PluginsConfigMap:          in.PluginsConfigMap,
		PluginsChecksum:           in.PluginsChecksum,
		TerminationGracePeriodSec: int64(in.Policy.DrainTimeoutSeconds) + restartHeadroomSec,
		DrainTimeoutSec:           int64(in.Policy.DrainTimeoutSeconds),
		StorageSize:               storageSize,
		StorageClass:              storageClass,
		Resources:                 jenkinsResources,
		VarroaEndpoint:            r.varroaEndpoint,
		ImagePullSecrets:          imagePullSecrets,
		ServiceAccountName:        in.Name + "-agent",
		OIDCIssuer:                r.Resolver.OIDCIssuer(),
		VarroaLoginURL:            r.Resolver.LoginURL(r.varroaRedirectURL),
		OIDCUserClaim:             r.Resolver.OIDCUserClaim(),
		OIDCGroupClaim:            r.Resolver.OIDCGroupClaim(),
		MitePubKeyPEM:             r.mitePubKeyPEM(),
		MitePubKeyKID:             r.mitePubKeyKID(),
		MiteImagePullPolicy:       miteImagePullPolicy,
		MiteResources:             miteResources,
		ApikeyVerifyURL:           r.apikeyVerifyURL(),
		CAPEM:                     r.caPEM,
		PathPrefix:                pathPrefix,
		VarroaBaseURL:             varroaBaseURL(r.varroaRedirectURL),
		ClusterName:               r.clusterName,
		Replicas:                  &replicas,
		HibernationIgnoreRegex:    hibernationIgnoreRegex(cr.Spec.Hibernation),
		PodOverrides:              mergeClassPodDefaults(class, cr.Spec.PodOverrides),
		Probes:                    probes,
		ResourceOverlay:           cr.Spec.ResourceOverlay,
		ResourcesSource:           resourcesSource,
	}
}

// miteObservation is everything handleConnected reads from the transport
// about one mite, gathered before derivation so the health logic stays pure.
type miteObservation struct {
	TransportConnected bool
	Version            string
	LastHeartbeat      time.Time
	CertExpiry         time.Time
	// Health is the transport health state ("healthy"/"unhealthy"/"unreachable"/...).
	Health   string
	Snapshot *mitev1.StateSnapshot
}

// miteHealthResult is what deriveMiteHealth decided: effective connectivity
// (after the stale-heartbeat override), the MiteStatus fields to persist, an
// optional LiveDrift replacement, and the conditions to apply, in order.
type miteHealthResult struct {
	Connected bool
	// StaleOverride means the transport said connected but the heartbeat aged
	// past staleMiteThreshold — the handler logs the downgrade.
	StaleOverride bool
	MiteStatus    v1alpha1.MiteStatus
	// LiveDrift is nil when cr.Status.LiveDrift must stay unchanged (no
	// snapshot, or a snapshot with neither drift nor a live config hash).
	LiveDrift  *v1alpha1.LiveDriftStatus
	Conditions []v1alpha1.ControllerCondition
}

// deriveMiteHealth turns a transport observation into desired status. Pure:
// prevDrift is the CR's current LiveDrift (DetectedAt preservation) and now
// is the caller's clock.
func deriveMiteHealth(obs miteObservation, prevDrift *v1alpha1.LiveDriftStatus, now metav1.Time) miteHealthResult {
	res := miteHealthResult{Connected: obs.TransportConnected}

	// If the transport says connected but the last heartbeat is stale, the
	// mite is gone. Use lastHeartbeat from Info (the transport source of
	// truth), not cr.Status.MiteStatus.LastSeen (the persisted CR field
	// which may lag behind after an operator restart).
	if res.Connected && now.Sub(obs.LastHeartbeat) > staleMiteThreshold {
		res.Connected = false
		res.StaleOverride = true
	}
	if !res.Connected {
		return res
	}

	res.MiteStatus = v1alpha1.MiteStatus{
		Connected:  true,
		Version:    obs.Version,
		LastSeen:   &metav1.Time{Time: obs.LastHeartbeat},
		CertExpiry: &metav1.Time{Time: obs.CertExpiry},
	}

	if snapshot := obs.Snapshot; snapshot != nil {
		res.MiteStatus.JenkinsVersion = snapshot.JenkinsVersion
		res.MiteStatus.JenkinsHealth = snapshot.JenkinsHealth
		res.MiteStatus.LastHealthCheck = &metav1.Time{Time: obs.LastHeartbeat}

		// ApplyDeferred condition: surface idle-defer state from the snapshot.
		// Independent from RestartDeferred (set/cleared in the result drain).
		if snapshot.ApplyDeferred {
			res.Conditions = append(res.Conditions, v1alpha1.ControllerCondition{
				Type:    v1alpha1.ConditionApplyDeferred,
				Status:  metav1.ConditionTrue,
				Reason:  "BuildsRunning",
				Message: snapshot.DeferReason,
			})
		} else {
			res.Conditions = append(res.Conditions, v1alpha1.ControllerCondition{
				Type:   v1alpha1.ConditionApplyDeferred,
				Status: metav1.ConditionFalse,
				Reason: "ApplyNotDeferred",
			})
		}

		// LiveDrift condition: surface live-drift detection from the snapshot.
		// Detection only — never gates convergence or Ready.
		if snapshot.LiveDrift {
			detectedAt := &now
			if prevDrift != nil && prevDrift.Detected && prevDrift.DetectedAt != nil {
				detectedAt = prevDrift.DetectedAt
			}
			res.LiveDrift = &v1alpha1.LiveDriftStatus{
				Detected:       true,
				LiveConfigHash: snapshot.LiveConfigHash,
				DetectedAt:     detectedAt,
				LastCheckedAt:  &now,
			}
			res.Conditions = append(res.Conditions, v1alpha1.ControllerCondition{
				Type:    v1alpha1.ConditionLiveDrift,
				Status:  metav1.ConditionTrue,
				Reason:  "ExternalMutation",
				Message: "Jenkins config was changed outside Varroa",
			})
		} else if snapshot.LiveConfigHash != "" {
			res.LiveDrift = &v1alpha1.LiveDriftStatus{
				Detected:       false,
				LiveConfigHash: snapshot.LiveConfigHash,
				LastCheckedAt:  &now,
			}
			res.Conditions = append(res.Conditions, v1alpha1.ControllerCondition{
				Type:   v1alpha1.ConditionLiveDrift,
				Status: metav1.ConditionFalse,
				Reason: "InSync",
			})
		}
	}

	// Ready condition based on Jenkins health.
	readyStatus := metav1.ConditionTrue
	readyReason := "JenkinsHealthy"
	if obs.Health == "unhealthy" || obs.Health == "unreachable" {
		readyStatus = metav1.ConditionFalse
		readyReason = "JenkinsUnhealthy"
	}
	res.Conditions = append(res.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionReady,
		Status:  readyStatus,
		Reason:  readyReason,
		Message: "jenkins health: " + obs.Health,
	})
	return res
}
