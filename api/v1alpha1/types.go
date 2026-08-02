package v1alpha1

import (
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName is the API group for Varroa resources.
const GroupName = "varroa.dev"

var (
	// SchemeGroupVersion is the group version used to register these objects.
	SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}
	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&Controller{}, &ControllerList{},
		&VarroaRole{}, &VarroaRoleList{},
		&VarroaRoleBinding{}, &VarroaRoleBindingList{},
		&JenkinsRole{}, &JenkinsRoleList{},
		&JenkinsRoleBinding{}, &JenkinsRoleBindingList{},
		&User{}, &UserList{},
		&Group{}, &GroupList{},
		&PodTemplate{}, &PodTemplateList{},
		&BuildMetric{}, &BuildMetricList{},
		&CatalogSource{}, &CatalogSourceList{},
		&CatalogItem{}, &CatalogItemList{},
		&ComposedBundle{}, &ComposedBundleList{},
		&ProvisioningDefaults{}, &ProvisioningDefaultsList{},
		&JenkinsVersionProfile{}, &JenkinsVersionProfileList{},
		&UpdateCenter{}, &UpdateCenterList{},
		&ControllerClass{}, &ControllerClassList{},
		&Team{}, &TeamList{},
		&BroodOperation{}, &BroodOperationList{},
		&BroodSchedule{}, &BroodScheduleList{},
	)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}

// Resource takes an unqualified resource and returns a Group-qualified GroupResource.
func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Controller is the Schema for the controllers API.
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="PHASE",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="ENDPOINT",type="string",JSONPath=".status.endpoint"
// +kubebuilder:printcolumn:name="VERSION",type="string",JSONPath=".spec.version"
// +kubebuilder:printcolumn:name="APPLY",type="boolean",JSONPath=".status.lastApplyResult.succeeded"
// +kubebuilder:printcolumn:name="APPLIED",type="date",JSONPath=".status.lastApplyResult.timestamp"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type Controller struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ControllerSpec   `json:"spec,omitempty"`
	Status ControllerStatus `json:"status,omitempty"`
}

// ControllerSpec defines the desired state of Controller.
type ControllerSpec struct {
	// Target namespace this controller manages.
	Namespace string `json:"namespace"`

	// API endpoint URL for the controller.
	Endpoint string `json:"endpoint,omitempty"`

	// Controller version tag.
	Version string `json:"version,omitempty"`

	// ComposedBundleRef references a ComposedBundle for JCasC configuration.
	ComposedBundleRef *ComposedBundleRef `json:"composedBundleRef,omitempty"`

	// PluginSpec declares the plugin policy and entries.
	PluginSpec *PluginSpec `json:"pluginSpec,omitempty"`

	// BackupSpec defines backup configuration for this controller.
	BackupSpec *BackupSpec `json:"backupSpec,omitempty"`

	// IngressSpec defines ingress configuration.
	IngressSpec *IngressSpec `json:"ingressSpec,omitempty"`

	// MiteSpec defines the mite sidecar configuration.
	MiteSpec *MiteSpec `json:"miteSpec,omitempty"`

	// PowerState controls whether the controller runs. "Running" (default/empty)
	// keeps 1 replica; "Stopped" scales the StatefulSet to 0; "Hibernated" scales
	// the StatefulSet to 0 after automatic hibernation.
	// +kubebuilder:validation:Enum="";Running;Stopped;Hibernated
	PowerState string `json:"powerState,omitempty"`

	// Resources defines CPU/memory resource requests and limits for the Jenkins container.
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// Persistence configures the Jenkins home persistent volume claim.
	// +optional
	Persistence *PersistenceSpec `json:"persistence,omitempty"`

	// ReconciliationPolicy controls how and when this controller is reconciled.
	// If unset, defaults from ProvisioningDefaults or the operator defaults.
	// +optional
	ReconciliationPolicy *ReconciliationPolicy `json:"reconciliationPolicy,omitempty"`

	// RBACSpec maps OIDC groups to built-in Varroa roles for this controller.
	// +optional
	RBACSpec *RBACSpec `json:"rbacSpec,omitempty"`

	// ClassName references a ControllerClass by name, layering its defaults
	// between ProvisioningDefaults and this Controller's own spec. Optional;
	// an unset value skips the class layer entirely. A set-but-nonexistent
	// value fails closed (see ClassResolved condition).
	ClassName string `json:"className,omitempty"`

	// PodOverrides supplies typed, pod-scoped customizations merged onto the Jenkins StatefulSet
	// (Tier 2 of the pod/resource layering chain — see "Layering contract" in
	// docs/config/pod-customization.md). Applied after ControllerClass/typed-spec defaults, before
	// resourceOverlay.
	// +optional
	PodOverrides *PodOverrides `json:"podOverrides,omitempty"`

	// ResourceOverlay supplies raw strategic-merge-patch YAML for provisioned resources. Admin-only.
	// Tier 3 of the pod/resource layering chain (see "Layering contract" in
	// docs/config/pod-customization.md) — the escape hatch, applied last, unsupported-shape
	// territory. See status condition OverlayActive for whether a controller is using it.
	// +optional
	ResourceOverlay *ResourceOverlay `json:"resourceOverlay,omitempty"`

	// Hibernation configures automatic controller hibernation.
	// +optional
	Hibernation *HibernationSpec `json:"hibernation,omitempty"`

	// Probes tunes the Jenkins container health probes.
	// +optional
	Probes *ProbesSpec `json:"probes,omitempty"`
}

// ProbesSpec tunes the Jenkins container health probes. Each sub-probe is optional;
// nil ProbesSpec (or nil sub-probe) renders that probe with built-in defaults.
type ProbesSpec struct {
	// +optional
	Startup *ProbeSpec `json:"startup,omitempty"`
	// +optional
	Readiness *ProbeSpec `json:"readiness,omitempty"`
	// +optional
	Liveness *ProbeSpec `json:"liveness,omitempty"`
}

// ProbeSpec is a curated subset of corev1.Probe timing knobs.
type ProbeSpec struct {
	// Disabled removes this probe entirely. Default false (probe enabled with defaults).
	// +optional
	Disabled bool `json:"disabled,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	InitialDelaySeconds *int32 `json:"initialDelaySeconds,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=300
	PeriodSeconds *int32 `json:"periodSeconds,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=300
	TimeoutSeconds *int32 `json:"timeoutSeconds,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	FailureThreshold *int32 `json:"failureThreshold,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	SuccessThreshold *int32 `json:"successThreshold,omitempty"`
}

// HibernationSpec configures automatic hibernation for a controller.
// +kubebuilder:validation:XValidation:rule="!has(self.gracePeriodMinutes) || self.gracePeriodMinutes >= 5",message="gracePeriodMinutes must be >= 5"
type HibernationSpec struct {
	// Enabled enables automatic hibernation. When enabled, the controller will
	// automatically park (scale to 0) after gracePeriodMinutes of inactivity.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// GracePeriodMinutes is the number of minutes of inactivity before the controller
	// hibernates. Minimum 5. Default 60.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=5
	// +optional
	GracePeriodMinutes int32 `json:"gracePeriodMinutes,omitempty"`

	// ActivityIgnoreRegex is a regular expression matched against request paths to
	// exclude them from activity tracking. If the regex is invalid, a warning is logged
	// and it is treated as unset. Changing this value rolls the controller pod.
	// +optional
	ActivityIgnoreRegex string `json:"activityIgnoreRegex,omitempty"`
}

// PodOverrides supplies typed, pod-scoped customizations merged onto the Jenkins StatefulSet.
// Embedded corev1 types ride along with their strategic-merge directives so list fields merge by
// key rather than being replaced wholesale.
type PodOverrides struct {
	// Env adds or overrides environment variables on the jenkins container (merged by name).
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
	// EnvFrom adds envFrom sources to the jenkins container.
	// +optional
	EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`
	// Volumes adds pod volumes (merged by name).
	// +optional
	Volumes []corev1.Volume `json:"volumes,omitempty"`
	// VolumeMounts adds volume mounts to the jenkins container (merged by name).
	// +optional
	VolumeMounts []corev1.VolumeMount `json:"volumeMounts,omitempty"`
	// PodLabels adds labels to the pod template metadata.
	// +optional
	PodLabels map[string]string `json:"podLabels,omitempty"`
	// PodAnnotations adds annotations to the pod template metadata.
	// +optional
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`
	// Labels adds labels to the StatefulSet metadata.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// Annotations adds annotations to the StatefulSet metadata.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
	// JvmOpts is appended (space-joined) to the baseline JAVA_OPTS value in the builder.
	// +optional
	JvmOpts string `json:"jvmOpts,omitempty"`
	// NodeSelector sets the pod nodeSelector.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Tolerations sets the pod tolerations.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// Affinity sets the pod affinity.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
	// SecurityContext sets the pod-level security context.
	// +optional
	SecurityContext *corev1.PodSecurityContext `json:"securityContext,omitempty"`
}

// ResourceOverlay holds raw strategic-merge-patch YAML strings per provisioned resource. PVC
// customization is done via the StatefulSet overlay's volumeClaimTemplates (no dedicated pvc key).
type ResourceOverlay struct {
	// StatefulSet is a strategic-merge-patch YAML applied to the Jenkins StatefulSet.
	// +optional
	StatefulSet string `json:"statefulSet,omitempty"`
	// Service is a strategic-merge-patch YAML applied to the controller Service.
	// +optional
	Service string `json:"service,omitempty"`
	// Ingress is a strategic-merge-patch YAML applied to the controller Ingress.
	// +optional
	Ingress string `json:"ingress,omitempty"`
}

// ReconciliationPolicy defines the reconciliation behavior for a controller.
// +kubebuilder:validation:XValidation:rule="!has(self.interval) || duration(self.interval) >= duration('10s')",message="interval must be at least 10s"
type ReconciliationPolicy struct {
	// Mode is the reconciliation mode. "automatic" (default) pushes desired state
	// immediately on config drift. "manual" requires user approval via the API/UI.
	// +kubebuilder:validation:Enum=automatic;idle;manual
	// +optional
	Mode ReconciliationMode `json:"mode,omitempty"`

	// MaxDeferSeconds bounds how long `idle` mode defers an apply while builds run
	// before applying anyway. 0 ⇒ default (1800). Only consulted when Mode == idle.
	// +optional
	MaxDeferSeconds int `json:"maxDeferSeconds,omitempty"`

	// DrainTimeoutSeconds bounds the quietDown build-drain before a restart-class change.
	// 0 ⇒ disabled (restart immediately, today's behavior). Default seeded to 900.
	// +optional
	DrainTimeoutSeconds int `json:"drainTimeoutSeconds,omitempty"`

	// Interval is the duration between steady-state reconciliation ticks in Connected phase.
	// Format: Go duration string (e.g., "30s", "5m", "1h").
	// Minimum: 10s. If unset, defaults from ProvisioningDefaults or 30s.
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// RolloutWave orders progressive bundle rollout. A controller applies a new bundle version only
	// after every Connected sibling sharing the bundle with a LOWER RolloutWave has applied that same
	// version successfully. 0 (default/unset) => no earlier waves => applies immediately (today's behavior).
	// +kubebuilder:validation:Minimum=0
	// +optional
	RolloutWave int `json:"rolloutWave,omitempty"`
}

// ReconciliationMode is the reconciliation mode for a controller.
type ReconciliationMode string

const (
	// ReconciliationModeAutomatic pushes desired state immediately on config drift.
	ReconciliationModeAutomatic ReconciliationMode = "automatic"
	// ReconciliationModeIdle defers desired state pushes while builds are running.
	ReconciliationModeIdle ReconciliationMode = "idle"
	// ReconciliationModeManual requires user approval before pushing desired state changes.
	ReconciliationModeManual ReconciliationMode = "manual"
)

// RBACSpec maps OIDC groups to built-in Varroa roles for a controller.
type RBACSpec struct {
	Groups []RBACGroupBinding `json:"groups,omitempty"`
}

// RBACGroupBinding assigns a single OIDC group to a built-in role.
type RBACGroupBinding struct {
	// Name is the OIDC group claim value.
	Name string `json:"name"`
	// Role is a built-in role short name.
	// +kubebuilder:validation:Enum=admin;operator;developer;viewer
	Role string `json:"role"`
}

// PluginSpec defines the plugin policy and plugin entries.
type PluginSpec struct {
	// Policy for plugin management (e.g. "pinned").
	Policy string `json:"policy,omitempty"`
	// Entries is the list of plugins with pinned versions.
	Entries []PluginEntry `json:"entries,omitempty"`
}

// PluginEntry defines a single plugin with artifact ID and version.
type PluginEntry struct {
	// ArtifactId is the Jenkins plugin artifact ID.
	ArtifactId string `json:"artifactId"`
	// Version is the pinned plugin version.
	Version string `json:"version"`
}

// BackupSpec defines backup configuration.
type BackupSpec struct {
	// Enabled indicates whether backups are enabled.
	Enabled bool `json:"enabled,omitempty"`
	// Schedule is a cron expression for the backup schedule.
	Schedule string `json:"schedule,omitempty"`
	// RetentionDays is the number of days to retain backups.
	RetentionDays int `json:"retentionDays,omitempty"`
}

// IngressSpec defines ingress configuration.
// +kubebuilder:validation:XValidation:rule="(has(oldSelf.mode) ? (oldSelf.mode == 'path' ? 'path' : 'subdomain') : 'subdomain') == (has(self.mode) ? (self.mode == 'path' ? 'path' : 'subdomain') : 'subdomain')",message="ingressSpec.mode is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.annotations) || self.annotations.all(k, k.matches(r'^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$'))",message="ingressSpec.annotations keys must be valid Kubernetes annotation names (optional DNS-subdomain prefix + '/' + alphanumeric name)"
type IngressSpec struct {
	// Host is the ingress hostname.
	Host string `json:"host,omitempty"`
	// TLSSecretName is the name of the TLS secret.
	TLSSecretName string `json:"tlsSecretName,omitempty"`
	// Mode selects how the controller is exposed: "subdomain" (default) gives the
	// controller its own host; "path" serves it at /jenkins/<ns>/<name> on the
	// shared host. Immutable after create.
	// +kubebuilder:validation:Enum="";subdomain;path
	Mode string `json:"mode,omitempty"`
	// Annotations adds/overrides ingress annotations for this controller only.
	// Merged over the cluster-wide ProvisioningDefaults.spec.ingressAnnotations;
	// on key conflict, this controller's value wins.
	// +optional
	// +kubebuilder:validation:MaxProperties=20
	Annotations map[string]string `json:"annotations,omitempty"`
	// IngressClassName overrides the cluster-wide ProvisioningDefaults.spec.ingressClassName
	// for this controller only. Empty means use the cluster default.
	// +optional
	IngressClassName string `json:"ingressClassName,omitempty"`
}

// PersistenceSpec configures the Jenkins home persistent volume claim.
// Applied at StatefulSet creation only — volumeClaimTemplates are immutable,
// so edits to an existing controller take effect after teardown/recreate.
type PersistenceSpec struct {
	// Size is the persistent volume storage request (e.g. "20Gi").
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|Ei|[numkMGTPE])?$`
	Size string `json:"size,omitempty"`
	// StorageClass is the Kubernetes StorageClass name for the persistent volume.
	StorageClass string `json:"storageClass,omitempty"`
}

// MiteSpec defines the mite sidecar configuration.
type MiteSpec struct {
	// Resources defines CPU/memory resource requests and limits for the mite sidecar container.
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Image overrides the mite sidecar container image for this Controller. Takes precedence
	// over the resolved ControllerClass's spec.mite.image and the operator-wide default
	// (VARROA_MITE_IMAGE / baked-in fallback), but is itself overridden by an explicit
	// resourceOverlay.statefulSet image on the mite container (the pod/resource layering
	// chain's Tier-3 escape hatch, per docs/config/pod-customization.md — distinct from this
	// field's own position in the image/pull-policy precedence table below).
	// +optional
	Image string `json:"image,omitempty"`

	// ImagePullPolicy overrides the mite sidecar image pull policy for this Controller. Same
	// precedence as Image. An unset value falls back to the resolved ControllerClass's
	// spec.mite.imagePullPolicy when set, otherwise the k8s-mirroring default computed from the
	// resolved image (":latest"/untagged -> Always; otherwise IfNotPresent).
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
}

// ControllerPhase represents the lifecycle phase of a controller.
type ControllerPhase string

const (
	// ControllerPhasePending is the initial phase before reconciliation starts.
	ControllerPhasePending ControllerPhase = "Pending"
	// ControllerPhaseProvisioning indicates the controller is being provisioned.
	ControllerPhaseProvisioning ControllerPhase = "Provisioning"
	// ControllerPhaseRunning indicates the controller is operational.
	ControllerPhaseRunning ControllerPhase = "Running"
	// ControllerPhaseConnected indicates the mite sidecar is connected.
	ControllerPhaseConnected ControllerPhase = "Connected"
	// ControllerPhaseStopped indicates the controller is powered off (spec.powerState=Stopped).
	ControllerPhaseStopped ControllerPhase = "Stopped"
	// ControllerPhaseHibernated indicates the controller is hibernated (spec.powerState=Hibernated).
	ControllerPhaseHibernated ControllerPhase = "Hibernated"
	// ControllerPhaseFailed indicates provisioning or operation has failed.
	ControllerPhaseFailed ControllerPhase = "Failed"
)

// ControllerStatus defines the observed state of Controller.
type ControllerStatus struct {
	// Phase is the current lifecycle phase of the controller.
	Phase ControllerPhase `json:"phase,omitempty"`

	// Endpoint is the resolved endpoint for the running controller.
	Endpoint string `json:"endpoint,omitempty"`

	// Conditions represent the latest available observations of the controller's state.
	Conditions []ControllerCondition `json:"conditions,omitempty"`

	// JenkinsRef references the generated Jenkins CR.
	JenkinsRef string `json:"jenkinsRef,omitempty"`

	// ProvisioningStartedAt is the timestamp when provisioning began.
	ProvisioningStartedAt *metav1.Time `json:"provisioningStartedAt,omitempty"`

	// ConfigHash is the hash of the JCasC configuration.
	ConfigHash string `json:"configHash,omitempty"`

	// RBACHash is the hash of the RBAC configuration.
	RBACHash string `json:"rbacHash,omitempty"`

	// DesiredStateHash is the combined hash of the desired state.
	DesiredStateHash string `json:"desiredStateHash,omitempty"`

	// MiteStatus holds the mite sidecar connection status.
	MiteStatus *MiteStatus `json:"miteStatus,omitempty"`

	// FirstConnectedAt records when the mite first connected and the initial
	// desired-state handshake completed. Used to distinguish fresh controllers
	// from reconnecting ones.
	// +optional
	FirstConnectedAt *metav1.Time `json:"firstConnectedAt,omitempty"`

	// PendingRestart indicates that config drift was detected in manual mode
	// and is awaiting user approval.
	// +optional
	PendingRestart *PendingRestart `json:"pendingRestart,omitempty"`

	// PendingPluginRoll, when non-nil, means managed plugins changed in manual/idle mode
	// and a pod roll is awaiting approval.
	// +optional
	PendingPluginRoll *PendingPluginRoll `json:"pendingPluginRoll,omitempty"`

	// ApprovedPluginRollChecksum is a one-shot approval of the target checksum for a
	// pending plugin roll. Set by ApproveRestart, consumed and cleared on next reconcile.
	// +optional
	ApprovedPluginRollChecksum string `json:"approvedPluginRollChecksum,omitempty"`

	// PendingItemDeletions lists managed items whose deletion is deferred
	// because a build is running. Requires explicit operator approval.
	// +optional
	PendingItemDeletions []PendingDeletion `json:"pendingItemDeletions,omitempty"`

	// RestartDrain tracks drain-and-restart backoff when a SAFE_RESTART
	// drain times out. Cleared on success or force-restart.
	// +optional
	RestartDrain *RestartDrainStatus `json:"restartDrain,omitempty"`

	// LastImperativeResult records the most recent imperative command result
	// (e.g. SAFE_RESTART) for brood operation completion predicates.
	// Last-one-wins; sufficient because commands to one controller serialize
	// through the mite.
	// +optional
	LastImperativeResult *ImperativeResult `json:"lastImperativeResult,omitempty"`

	// LiveDrift, when set with Detected=true, indicates live config diverged from what Varroa applied.
	// +optional
	LiveDrift *LiveDriftStatus `json:"liveDrift,omitempty"`

	// LastReconciledAt records the last time a full reconciliation completed.
	// +optional
	LastReconciledAt *metav1.Time `json:"lastReconciledAt,omitempty"`
	// LastDesiredPushAt records the last time a desired state was pushed to the mite.
	// Used by the convergence short-circuit to avoid pushing every tick while still
	// periodically correcting drift (pushes at least every 5 min).
	// +optional
	LastDesiredPushAt *metav1.Time `json:"lastDesiredPushAt,omitempty"`
	// LastApplyResult is the most recent desired-state apply outcome.
	// +optional
	LastApplyResult *ApplyResult `json:"lastApplyResult,omitempty"`
	// ApplyHistory is a newest-first ring buffer of the last 10 apply results.
	// +optional
	ApplyHistory []ApplyResult `json:"applyHistory,omitempty"`

	// AppliedBundleHash is the ComposedBundle.status.ResolvedHash of the last SUCCESSFUL apply.
	// It is the cross-sibling-comparable gate key (DesiredStateHash is per-controller and is NOT comparable).
	// +optional
	AppliedBundleHash string `json:"appliedBundleHash,omitempty"`

	// Rollout, when non-nil, reports this controller's wave-rollout gate state (pointer-struct
	// convention, cf. RestartDrain *RestartDrainStatus). nil => not blocked.
	// +optional
	Rollout *RolloutStatus `json:"rollout,omitempty"`

	// OverlayWarnings lists warn-but-allow guardrail hits from the last applied overlay.
	// +optional
	OverlayWarnings []OverlayWarning `json:"overlayWarnings,omitempty"`

	// LastActivityAt is the most recent activity timestamp across all sources
	// (HTTP activity, Jenkins events, mite connection). Only persisted when it
	// advances by more than 60 seconds.
	// +optional
	LastActivityAt *metav1.Time `json:"lastActivityAt,omitempty"`

	// WakeToken is a 128-bit crypto/rand hex token used by the BFF wake endpoint
	// to authenticate wake requests. Set when hibernation is enabled or the
	// controller is in Hibernated phase and the token is empty.
	// +optional
	WakeToken string `json:"wakeToken,omitempty"`

	// LastReconcileError is the message of the most recent reconcile-blocking
	// error, or empty if the controller is not currently blocked.
	// +optional
	LastReconcileError string `json:"lastReconcileError,omitempty"`

	// LastReconcileErrorAt is when LastReconcileError was last set. Cleared
	// (nil) together with LastReconcileError once the controller successfully
	// reconciles past the blocking condition.
	// +optional
	LastReconcileErrorAt *metav1.Time `json:"lastReconcileErrorAt,omitempty"`

	// PluginInventory is the bounded plugin inventory summary written by the
	// Connected-phase classifier. It carries the hash, collection/observation
	// times, source, freshness and degradation flags, installed total, per-class
	// counts, capped drift and version-drift lists, and the list-capped flag.
	// The full inventory lives in the read model (invc/ KV key).
	// +optional
	PluginInventory *PluginInventoryStatus `json:"pluginInventory,omitempty"`
}

// OverlayWarning records a warn-but-allow guardrail hit where an overlay or podOverrides edit
// touched an operator-managed path. It never blocks reconciliation.
type OverlayWarning struct {
	// Resource is the overlaid resource kind, e.g. "statefulSet", "service", "ingress".
	Resource string `json:"resource"`
	// Path is the JSONPath-ish location of the protected edit.
	Path string `json:"path"`
	// Message is the human-readable warning.
	Message string `json:"message"`
}

// PendingPluginRoll represents an approved-but-not-yet-applied managed plugin pod roll.
type PendingPluginRoll struct {
	// TargetChecksum is the desired plugins.txt checksum awaiting approval.
	TargetChecksum string `json:"targetChecksum"`
	// Since is when the pending roll was first raised.
	Since metav1.Time `json:"since"`
	// Changes is a human-readable display diff (+id:ver / -id:ver).
	// +optional
	Changes []string `json:"changes,omitempty"`
}

// PendingRestart represents a pending configuration change awaiting approval.
type PendingRestart struct {
	// DetectedAt is when the config drift was first detected.
	DetectedAt metav1.Time `json:"detectedAt"`

	// DesiredStateHash is the pending desired state hash.
	DesiredStateHash string `json:"desiredStateHash"`

	// Changes lists which config sections changed (e.g., "plugins", "config", "rbac", "items").
	// +optional
	Changes []string `json:"changes,omitempty"`
}

// ApplySectionResult is the outcome of one section of a desired-state apply.
type ApplySectionResult struct {
	// Name is the apply section: one of "config", "rbac", "plugins", "items".
	Name string `json:"name"`
	// OK is true if this section applied successfully.
	OK bool `json:"ok"`
	// Error is the failure message when OK is false; truncated to 1024 bytes.
	// +optional
	Error string `json:"error,omitempty"`
}

// ApplyResult is the outcome of a single desired-state apply attempt.
type ApplyResult struct {
	// Hash is the desired-state hash that was attempted (present even on
	// partial failure; this is the attempted hash, not a converged hash).
	Hash string `json:"hash,omitempty"`
	// Timestamp is when the operator recorded this result (operator clock at drain).
	Timestamp metav1.Time `json:"timestamp"`
	// Succeeded is the logical AND of all four section OK values.
	Succeeded bool `json:"succeeded"`
	// Sections is the per-section breakdown, always four entries in the
	// fixed order: config, rbac, plugins, items.
	Sections []ApplySectionResult `json:"sections"`
}

// PendingDeletion records a managed item whose deletion was deferred because a
// build is running.
type PendingDeletion struct {
	Path       string      `json:"path"`
	Reason     string      `json:"reason,omitempty"`
	DetectedAt metav1.Time `json:"detectedAt,omitempty"`
}

// RestartDrainStatus tracks the backoff state for a drain-timeout restart deferral.
type RestartDrainStatus struct {
	// DesiredStateHash is the hash that triggered the restart attempt.
	DesiredStateHash string `json:"desiredStateHash,omitempty"`
	// CommandID is the outstanding SAFE_RESTART command_id for correlation.
	CommandID string `json:"commandID,omitempty"`
	// AttemptCount is the number of drain attempts made so far.
	AttemptCount int `json:"attemptCount,omitempty"`
	// NextRetryAt is the time after which the operator may re-issue the restart.
	NextRetryAt *metav1.Time `json:"nextRetryAt,omitempty"`
	// LastReason describes why the last attempt did not succeed.
	LastReason string `json:"lastReason,omitempty"`
}

// ImperativeResult records the outcome of an imperative command (e.g. SAFE_RESTART).
// Shape-aligned with RestartDrainStatus.CommandID for correlation.
type ImperativeResult struct {
	// CommandID is the imperative command identifier for correlation.
	CommandID string `json:"commandID,omitempty"`
	// Success indicates whether the command completed successfully.
	Success bool `json:"success"`
	// Error is the error message when Success is false.
	// +optional
	Error string `json:"error,omitempty"`
	// FinishedAt is when the result was recorded.
	// +optional
	FinishedAt metav1.Time `json:"finishedAt,omitempty"`
}

// RolloutStatus reports a controller held by the wave gate.
type RolloutStatus struct {
	// TargetBundleHash is the ComposedBundle.ResolvedHash the controller is waiting to apply.
	TargetBundleHash string `json:"targetBundleHash,omitempty"`
	// Blocked is true while the wave gate holds this controller on its last-good config.
	Blocked bool `json:"blocked"`
	// Paused is true when the ComposedBundle is annotated varroa.dev/rollout-paused, blocking
	// all not-on-target controllers (including wave 0) from advancing.
	Paused bool `json:"paused,omitempty"`
	// WaitingOn lists the earlier-wave Connected siblings not yet healthy on TargetBundleHash,
	// as "namespace/name", sorted, capped at 10 entries.
	WaitingOn []string `json:"waitingOn,omitempty"`
	// BlockedSince is when the gate first blocked this controller for the current target.
	BlockedSince *metav1.Time `json:"blockedSince,omitempty"`
}

// LiveDriftStatus is the operator-surfaced live-drift signal from the mite fingerprint.
type LiveDriftStatus struct {
	// Detected is true when the live fingerprint differs from the post-apply baseline.
	Detected bool `json:"detected"`
	// LiveConfigHash is the managed-scope fingerprint hash of the latest live export.
	LiveConfigHash string `json:"liveConfigHash,omitempty"`
	// DetectedAt is when drift was first detected; cleared when in-sync.
	DetectedAt *metav1.Time `json:"detectedAt,omitempty"`
	// LastCheckedAt is when the fingerprint was last computed.
	LastCheckedAt *metav1.Time `json:"lastCheckedAt,omitempty"`
}

// reason consts for DeletionPending condition.
const (
	ReasonItemDeletionPending = "ItemDeletionPending"
	ReasonNoDeletionPending   = "NoDeletionPending"
)

const (
	// ReasonBlockedByWave is set when the wave rollout gate holds a controller.
	ReasonBlockedByWave = "BlockedByWave"
	// ReasonWaveCleared is set when the wave rollout gate clears and the controller proceeds.
	ReasonWaveCleared = "WaveCleared"
	// ReasonRolloutPaused is set when the ComposedBundle-level rollout pause holds a controller.
	ReasonRolloutPaused = "RolloutPaused"

	// ReasonPluginRollPending is set when the managed plugin set changed in manual/idle mode.
	ReasonPluginRollPending = "PluginRollPending"
	// ReasonVersionRollStarted is set when a gate-allowed version roll transitions the controller to Provisioning.
	ReasonVersionRollStarted = "VersionRollStarted"
	// ReasonVersionConverged is set when the stamped applied jenkins image equals the effective desired image.
	ReasonVersionConverged = "VersionConverged"
	// ReasonMiteSpecRollStarted is set when a mite sidecar image, resources,
	// or imagePullPolicy delta transitions the controller to Provisioning.
	ReasonMiteSpecRollStarted = "MiteSpecRollStarted"
	// ReasonMiteSpecConverged is set when the mite sidecar's applied image,
	// resources, and imagePullPolicy all equal their effective desired values.
	ReasonMiteSpecConverged = "MiteSpecConverged"
	// ReasonCoreOlderThanPluginBaseline is set when core(spec.version) < plugin-set baseline (the #185 crash class).
	ReasonCoreOlderThanPluginBaseline = "CoreOlderThanPluginBaseline"
	// ReasonUnparseableVersion is set when spec.version is unparseable and no profile vouches for it.
	ReasonUnparseableVersion = "UnparseableVersion"
	// ReasonCoreCompatible is set (with VersionUpgradeBlocked=False) when the requested core is compatible.
	ReasonCoreCompatible = "CoreCompatible"
	// ReasonPluginRollApproved is set when the operator clears a pending roll after an approval roll.
	ReasonPluginRollApproved = "PluginRollApproved"
	// ReasonPluginRollFailed is set when the plugins-init container fails to sync managed plugins.
	ReasonPluginRollFailed = "PluginRollFailed"
	// ReasonPluginConflict is set when a requested plugin version conflicts with the profile lock set.
	ReasonPluginConflict = "PluginConflict"
	// ReasonPluginInstallRequired is set when the managed plugin set diverged from
	// the baked set in manual/idle mode and the roll is not yet approved (it will
	// not converge until the user approves the plugin-roll or sets mode: automatic).
	ReasonPluginInstallRequired = "PluginInstallRequired"
	// ReasonPluginsInstalled is set when no managed-plugin action is required: the
	// set matches the baked set, or mode is automatic (drift self-heals via roll),
	// or the roll is already approved.
	ReasonPluginsInstalled = "PluginsInstalled"
	// ReasonJCascApplyFailed is set when the last apply result's config section failed.
	ReasonJCascApplyFailed = "JCascApplyFailed"
	// ReasonConfigApplied is set when the last apply result indicates the config
	// section succeeded (or no apply has been recorded yet).
	ReasonConfigApplied = "ConfigApplied"
	// ReasonTargetNamespaceMissing is the reason set when the target namespace does
	// not exist (BFF preflight only).
	ReasonTargetNamespaceMissing = "TargetNamespaceMissing"
	// ReasonTargetNamespaceUnmanaged is the reason set when the namespace exists
	// but is outside managedNamespaces in scoped mode, so operator/BFF RBAC does
	// not reach it.
	ReasonTargetNamespaceUnmanaged = "TargetNamespaceUnmanaged"
	// ReasonTargetNamespaceReady is the healthy-path clear of a prior
	// TargetNamespaceUnmanaged Degraded.
	ReasonTargetNamespaceReady = "TargetNamespaceReady"
	// ReasonHibernated is set when the controller is hibernated (scaled to 0).
	ReasonHibernated = "Hibernated"
	// ReasonCronTriggersSkipped is set when hibernation skips TimerTrigger jobs.
	ReasonCronTriggersSkipped = "CronTriggersSkipped"
	// ReasonWakeInitiated is set when a wake request is processed.
	ReasonWakeInitiated = "WakeInitiated"
	// ReasonClassNotFound is set when spec.className is set but the
	// referenced ControllerClass does not exist or is unreadable.
	ReasonClassNotFound = "ClassNotFound"
	// ReasonClassResolved is set when spec.className resolves to a
	// ControllerClass successfully.
	ReasonClassResolved = "ClassResolved"
	// ReasonNoClassConfigured is set when spec.className is unset,
	// indicating no class layer is being applied.
	ReasonNoClassConfigured = "NoClassConfigured"
	// ReasonResourceOverlaySet is set when spec.resourceOverlay has one or
	// more of statefulSet/service/ingress populated — the Tier-3 escape hatch
	// is in use.
	ReasonResourceOverlaySet = "ResourceOverlaySet"
	// ReasonNoResourceOverlay is set when spec.resourceOverlay is nil or
	// every sub-field is empty — no Tier-3 overlay is active.
	ReasonNoResourceOverlay = "NoResourceOverlay"
	// ReasonMiteImageStale is set when the running mite image differs from
	// effectiveDesiredMiteImage(cr).
	ReasonMiteImageStale = "MiteImageStale"
	// ReasonMiteImageCurrent is set when the running mite image matches
	// effectiveDesiredMiteImage(cr).
	ReasonMiteImageCurrent = "MiteImageCurrent"
)

const (
	// ReasonReconcileBlocked is the parent/namespace anchor for the
	// ReconcileBlocked reason family. markReconcileBlocked always sets
	// Reason to one of the 19 site-specific sub-reasons below, never
	// this bare parent string — it exists as a namespacing/doc anchor.
	ReasonReconcileBlocked = "ReconcileBlocked"
	// ReasonReconcileBlockedBundleUnreadable is set when the effective ComposedBundle is not found or unreadable.
	// There is deliberately no "BundleRefMissing" reason: an unset spec.composedBundleRef
	// resolves to the built-in starter bundle, so a missing bundle is always this case.
	ReasonReconcileBlockedBundleUnreadable = "BundleUnreadable"
	// ReasonReconcileBlockedClassResolutionFailed is set when spec.className does not resolve (ClassNotFound).
	ReasonReconcileBlockedClassResolutionFailed = "ClassResolutionFailed"
	// ReasonReconcileBlockedServiceReconcileFailed is set when the Service reconcile fails.
	ReasonReconcileBlockedServiceReconcileFailed = "ServiceReconcileFailed"
	// ReasonReconcileBlockedAgentRBACFailed is set when agent ServiceAccount creation or agent RBAC provisioning fails.
	ReasonReconcileBlockedAgentRBACFailed = "AgentRBACFailed"
	// ReasonReconcileBlockedBootstrapTokenFailed is set when the bootstrap token reconcile fails.
	ReasonReconcileBlockedBootstrapTokenFailed = "BootstrapTokenFailed"
	// ReasonReconcileBlockedPluginsConfigMapFailed is set when the plugins ConfigMap write fails.
	ReasonReconcileBlockedPluginsConfigMapFailed = "PluginsConfigMapFailed"
	// ReasonReconcileBlockedInitConfigMapFailed is set when the init ConfigMap write fails.
	ReasonReconcileBlockedInitConfigMapFailed = "InitConfigMapFailed"
	// ReasonReconcileBlockedCascConfigMapFailed is set when the CASC ConfigMap write fails.
	ReasonReconcileBlockedCascConfigMapFailed = "CascConfigMapFailed"
	// ReasonReconcileBlockedStatefulSetCreateFailed is set when the StatefulSet create/update fails.
	ReasonReconcileBlockedStatefulSetCreateFailed = "StatefulSetCreateFailed"
	// ReasonReconcileBlockedIngressCreateFailed is set when the Ingress reconcile fails.
	ReasonReconcileBlockedIngressCreateFailed = "IngressCreateFailed"
	// ReasonReconcileBlockedMiteConnectTimeout is set when the mite connection times out.
	ReasonReconcileBlockedMiteConnectTimeout = "MiteConnectTimeout"
	// ReasonReconcileBlockedWaveGateCheckFailed is set when the wave-gate check fails.
	ReasonReconcileBlockedWaveGateCheckFailed = "WaveGateCheckFailed"
	// ReasonReconcileBlockedDesiredStatePushFailed is set when the desired-state push fails.
	ReasonReconcileBlockedDesiredStatePushFailed = "DesiredStatePushFailed"
	// ReasonReconcileBlockedHibernateTransitionFailed is set when a hibernate/power-state transition fails.
	ReasonReconcileBlockedHibernateTransitionFailed = "HibernateTransitionFailed"
	// ReasonReconcileBlockedFinalizeFailed is set when finalize fails during deletion.
	ReasonReconcileBlockedFinalizeFailed = "FinalizeFailed"
	// ReasonReconcileBlockedScaleDownFailed is set when ScaleStatefulSet fails on Stopped or Hibernated.
	ReasonReconcileBlockedScaleDownFailed = "ScaleDownFailed"
	// ReasonReconcileBlockedUnknownPhase is set when the controller's phase is unrecognized.
	ReasonReconcileBlockedUnknownPhase = "UnknownPhase"
	// ReasonReconcileBlockedPluginConflict is set when a plugin lock version conflict
	// is detected against the resolved profile core set during provisioning.
	ReasonReconcileBlockedPluginConflict = "PluginConflict"
)

// MiteStatus holds the mite sidecar connection and version information.
type MiteStatus struct {
	// Connected is true when the mite gRPC command stream is active.
	Connected bool `json:"connected"`
	// Version is the mite binary version.
	Version string `json:"version,omitempty"`
	// LastSeen is the timestamp of the last heartbeat from the mite.
	LastSeen *metav1.Time `json:"lastSeen,omitempty"`
	// CertExpiry is the expiry time of the mite's client certificate.
	CertExpiry *metav1.Time `json:"certExpiry,omitempty"`
	// JenkinsVersion is the Jenkins version reported by the mite.
	JenkinsVersion string `json:"jenkinsVersion,omitempty"`
	// JenkinsHealth is the health status reported by the mite (healthy, starting, unhealthy, unreachable).
	JenkinsHealth string `json:"jenkinsHealth,omitempty"`
	// LastHealthCheck is the timestamp of the last Jenkins health probe.
	LastHealthCheck *metav1.Time `json:"lastHealthCheck,omitempty"`
	// Image is the mite sidecar image actually running for this controller,
	// as observed by the operator (Pod container image, else StatefulSet
	// computed-images stamp / live template). Populated independent of
	// Connected: a controller whose mite has never connected (e.g.
	// hibernated since creation) still gets Image from the StatefulSet
	// fallback tier.
	Image string `json:"image,omitempty"`
}

// ControllerConditionType is the type of a controller condition.
type ControllerConditionType string

const (
	// ConditionBundleResolved indicates the JCasC bundle was resolved successfully.
	ConditionBundleResolved ControllerConditionType = "BundleResolved"
	// ConditionBundleFailed indicates bundle resolution failed.
	ConditionBundleFailed ControllerConditionType = "BundleFailed"
	// ConditionProvisioning indicates the controller is provisioning.
	ConditionProvisioning ControllerConditionType = "Provisioning"
	// ConditionReady indicates the controller is ready.
	ConditionReady ControllerConditionType = "Ready"
	// ConditionFailed indicates the controller has failed.
	ConditionFailed ControllerConditionType = "Failed"
	// ConditionDegraded indicates the controller runs but with a configuration problem.
	ConditionDegraded ControllerConditionType = "Degraded"
	// ConditionRBACLockoutRisk indicates that the last RBAC generation would leave
	// no human administrator; the operator skipped the RBAC push to preserve last-good
	// authorization.
	ConditionRBACLockoutRisk ControllerConditionType = "RBACLockoutRisk"
	// ConditionPluginLockMissing indicates that the Controller requested a Jenkins
	// core version with no corresponding lockfile entry; the baseline set was used
	// as a fallback and core plugins may not match the requested core.
	ConditionPluginLockMissing ControllerConditionType = "PluginLockMissing"
	// ConditionDeletionPending indicates one or more managed items are pending deletion
	// because a build is running and await operator approval.
	ConditionDeletionPending ControllerConditionType = "DeletionPending"
	// ConditionApplyDeferred indicates a config apply is deferred because builds are running.
	ConditionApplyDeferred ControllerConditionType = "ApplyDeferred"
	// ConditionRestartDeferred indicates a restart drain timed out and is pending retry.
	ConditionRestartDeferred ControllerConditionType = "RestartDeferred"
	// ConditionLiveDrift indicates the live JCasC config diverged from what Varroa applied.
	ConditionLiveDrift ControllerConditionType = "LiveDrift"
	// ConditionRolloutBlocked indicates the controller is held by the wave rollout gate and
	// is not applying a new bundle version until earlier waves are healthy.
	ConditionRolloutBlocked ControllerConditionType = "RolloutBlocked"
	// ConditionRolloutPaused indicates the controller is held by a bundle-level rollout pause
	// (ComposedBundle annotation varroa.dev/rollout-paused=true). Pause wins over override and
	// freezes all waves including wave 0.
	ConditionRolloutPaused ControllerConditionType = "RolloutPaused"
	// ConditionPluginRollPending indicates the managed plugin set changed in
	// manual/idle mode and a pod roll is awaiting approval.
	ConditionPluginRollPending ControllerConditionType = "PluginRollPending"
	// ConditionPluginRollFailed indicates the plugins-init container failed to
	// sync the managed plugin set during a pod roll.
	ConditionPluginRollFailed ControllerConditionType = "PluginRollFailed"
	// ConditionPluginConflict indicates the requested plugin set conflicts with
	// the resolved JenkinsVersionProfile/core lock set.
	ConditionPluginConflict ControllerConditionType = "PluginConflict"
	// ConditionVersionUpgradeBlocked indicates the requested Jenkins core is
	// incompatible with the plugin set that would bake (guard-version-upgrade-path).
	ConditionVersionUpgradeBlocked ControllerConditionType = "VersionUpgradeBlocked"
	// ConditionPluginInstallRequired indicates that the managed plugin set diverged
	// from the baked set in manual/idle mode without an approved roll — the drift
	// will not self-heal until the plugin-roll is approved (or mode is automatic).
	ConditionPluginInstallRequired ControllerConditionType = "PluginInstallRequired"
	// ConditionConfigApplyFailed indicates a config-section apply failure (e.g. a
	// JCasC preflight rejection) was recorded in the last apply result.
	ConditionConfigApplyFailed ControllerConditionType = "ConfigApplyFailed"
	// ConditionVersionRollPending indicates a version/image roll is detected,
	// held by the version-roll gate, or in flight (cleared on convergence).
	ConditionVersionRollPending ControllerConditionType = "VersionRollPending"
	// ConditionMiteSpecRollPending indicates a mite sidecar roll (image,
	// resources, or imagePullPolicy) is detected or in flight (cleared on
	// convergence). Unlike ConditionVersionRollPending there is no compat gate
	// to hold on.
	ConditionMiteSpecRollPending ControllerConditionType = "MiteSpecRollPending"
	// ConditionMiteImageStale indicates the controller's actually-running mite
	// image differs from the operator-desired mite image. Detection only —
	// the operator takes no remediation action based on this condition.
	ConditionMiteImageStale ControllerConditionType = "MiteImageStale"
	// ConditionHibernationCronTriggersSkipped indicates the controller has
	// TimerTrigger jobs and hibernation is active — cron triggers are skipped
	// while the controller is parked.
	ConditionHibernationCronTriggersSkipped ControllerConditionType = "HibernationCronTriggersSkipped"
	// ConditionClassResolved reports whether spec.className (when set)
	// resolved to an existing ControllerClass. True/NoClassConfigured when
	// className is unset; True/ClassResolved when found; False/ClassNotFound
	// when set but missing or unreadable — fails closed, blocks
	// handleProvisioning and reconcileContainerSpecRoll (not reconcileIngress).
	ConditionClassResolved ControllerConditionType = "ClassResolved"
	// ConditionOverlayActive is True when spec.resourceOverlay sets any of
	// statefulSet/service/ingress — the controller is using the Tier-3 escape hatch.
	ConditionOverlayActive ControllerConditionType = "OverlayActive"
	// ConditionUpdateCenterFallback indicates the update center is configured but
	// unreachable in online mode; the init container falls back to tool defaults.
	// Non-blocking: provisioning proceeds.
	ConditionUpdateCenterFallback ControllerConditionType = "UpdateCenterFallback"
	// ConditionWaitingForUpdateCenter indicates the update center is configured but
	// not Ready in air-gap mode; provisioning is blocked until the UC is available.
	ConditionWaitingForUpdateCenter ControllerConditionType = "WaitingForUpdateCenter"
	// ConditionReconcileBlocked indicates the reconcile loop is blocked by an
	// unresolved error (a transient API-server hiccup, a missing resource, etc.).
	// It is set True with one of 19 sub-reasons by markReconcileBlocked at a
	// specific blocking call site and cleared to False once a subsequent reconcile
	// pass succeeds.
	ConditionReconcileBlocked ControllerConditionType = "ReconcileBlocked"
	// ConditionPluginInventoryDrift indicates that the controller has unmanaged
	// plugins installed and the inventory is fresh, complete, and non-degraded.
	ConditionPluginInventoryDrift ControllerConditionType = "PluginInventoryDrift"
	// ConditionNoExternalURL indicates the controller resolved no hostname, so
	// no Ingress was created and the UI is reachable only through the in-cluster
	// Service (kubectl port-forward).
	//
	// It is informational, never blocking: a controller with no host still
	// provisions, connects, and builds. It exists because the alternative —
	// creating no Ingress and saying nothing — is indistinguishable from a
	// broken ingress controller.
	ConditionNoExternalURL ControllerConditionType = "NoExternalURL"
)

// ControllerCondition contains details about the current condition of a controller.
type ControllerCondition struct {
	// Type of the condition.
	Type ControllerConditionType `json:"type"`
	// Status of the condition: True, False, or Unknown.
	Status metav1.ConditionStatus `json:"status"`
	// LastTransitionTime is the last time the condition transitioned.
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
	// Reason is a machine-readable reason for the condition.
	Reason string `json:"reason,omitempty"`
	// Message is a human-readable message.
	Message string `json:"message,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ControllerList contains a list of Controller objects.
type ControllerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Controller `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// User is the Schema for the users API.
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type User struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UserSpec   `json:"spec,omitempty"`
	Status UserStatus `json:"status,omitempty"`
}

// UserSpec defines the desired state of User.
type UserSpec struct {
	// Email is the email address of the user.
	Email string `json:"email,omitempty"`
	// DisplayName is a human-readable display name.
	DisplayName string `json:"displayName,omitempty"`
	// Password is a write-only field set by admins. The UserReconciler
	// hashes this value into status.credentials.passwordHash and clears it.
	Password string `json:"password,omitempty"`
}

// UserPreferences holds user-customizable UI settings.
type UserPreferences struct {
	Theme          string `json:"theme,omitempty"`
	Accent         string `json:"accent,omitempty"`
	DefaultLanding string `json:"defaultLanding,omitempty"`
}

// UserCredentials holds the hashed password for local auth users.
type UserCredentials struct {
	PasswordHash string       `json:"passwordHash,omitempty"`
	LastChanged  *metav1.Time `json:"lastChanged,omitempty"`
}

// UserStatus defines the observed state of User.
type UserStatus struct {
	// LastLogin is the timestamp of the user's most recent login.
	LastLogin *metav1.Time `json:"lastLogin,omitempty"`
	// ActiveControllers lists controller names this user has active sessions on.
	ActiveControllers []string `json:"activeControllers,omitempty"`
	// Preferences stores user-customizable UI settings.
	Preferences *UserPreferences `json:"preferences,omitempty"`
	// Credentials holds the hashed password for local auth users.
	// Only set when auth mode is "local".
	Credentials *UserCredentials `json:"credentials,omitempty"`
	// ObservedGroups captures the groups from OIDC claims at login time,
	// enabling read-only group-by views in OIDC mode.
	ObservedGroups []string `json:"observedGroups,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// UserList contains a list of User objects.
type UserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []User `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status

// Group is a cluster-scoped group of users for RBAC and local auth.
type Group struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GroupSpec   `json:"spec,omitempty"`
	Status GroupStatus `json:"status,omitempty"`
}

// GroupSpec defines the desired state of Group.
type GroupSpec struct {
	// DisplayName is a human-readable display name.
	DisplayName string `json:"displayName,omitempty"`
	// Members is the list of usernames in this group.
	Members []string `json:"members,omitempty"`
}

// GroupStatus defines the observed state of Group.
type GroupStatus struct{}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// GroupList contains a list of Group objects.
type GroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Group `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="NAMESPACES",type="string",JSONPath=".spec.namespaces"
// +kubebuilder:printcolumn:name="ROLE",type="string",JSONPath=".spec.roleRef"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"

// Team is a cluster-scoped group of users with namespace-scoped role bindings.
// The operator composes it into a Group + VarroaRoleBinding.
type Team struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TeamSpec   `json:"spec,omitempty"`
	Status            TeamStatus `json:"status,omitempty"`
}

// TeamSpec defines the desired state of a Team.
type TeamSpec struct {
	// DisplayName is a human-readable display name.
	DisplayName string `json:"displayName,omitempty"`
	// Members is the list of local usernames in this team (→ owned local Group).
	Members []string `json:"members,omitempty"`
	// Subjects are pass-through IdP-group/user subjects for the binding.
	Subjects []SubjectRef `json:"subjects,omitempty"`
	// Namespaces is the required list of Kubernetes namespaces this team
	// has RBAC in. Must have at least one entry.
	Namespaces []string `json:"namespaces"`
	// RoleRef is the VarroaRole to bind. Defaults to "developer" when empty.
	RoleRef string `json:"roleRef,omitempty"`
	// ProvisionNamespaces, when true, creates the team namespaces (via the
	// tenancy helper) if they do not already exist.
	ProvisionNamespaces bool `json:"provisionNamespaces,omitempty"`
}

// TeamStatus defines the observed state of a Team.
type TeamStatus struct {
	ObservedGeneration int64                `json:"observedGeneration,omitempty"`
	GroupRef           string               `json:"groupRef,omitempty"`
	BindingRef         string               `json:"bindingRef,omitempty"`
	NamespaceStates    []TeamNamespaceState `json:"namespaceStates,omitempty"`
	Conditions         []TeamCondition      `json:"conditions,omitempty"`
}

// TeamNamespaceState reports the state of a single namespace for a Team.
type TeamNamespaceState struct {
	Name  string `json:"name"`
	State string `json:"state"` // one of the TeamNamespaceState* consts
}

// TeamCondition is a status condition for a Team.
type TeamCondition struct {
	Type               string                 `json:"type"`
	Status             metav1.ConditionStatus `json:"status"`
	LastTransitionTime metav1.Time            `json:"lastTransitionTime,omitempty"`
	Reason             string                 `json:"reason,omitempty"`
	Message            string                 `json:"message,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// TeamList contains a list of Team objects.
type TeamList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Team `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PodTemplate is the Schema for the podtemplates API.
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type PodTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PodTemplateSpec `json:"spec,omitempty"`
}

// PodTemplateSpec defines the desired state of PodTemplate.
type PodTemplateSpec struct {
	// Containers is the list of containers that run as part of this pod template.
	Containers []ContainerSpec `json:"containers"`

	// RestartPolicy is the pod restart policy.
	// Valid values: Always, OnFailure, Never.
	RestartPolicy string `json:"restartPolicy,omitempty"`

	// NodeSelector is a map of node labels for pod assignment.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Volumes is the list of volumes to attach.
	Volumes []corev1.Volume `json:"volumes,omitempty"`

	// Tolerations is the list of pod tolerations.
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Resources defines resource requests and limits.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ContainerSpec defines a container within a pod template.
type ContainerSpec struct {
	// Name is the container name.
	Name string `json:"name"`
	// Image is the container image.
	Image string `json:"image"`
	// Command is the entrypoint array.
	Command []string `json:"command,omitempty"`
	// Args are the arguments to the entrypoint.
	Args []string `json:"args,omitempty"`
	// Env is the list of environment variables.
	Env []corev1.EnvVar `json:"env,omitempty"`
	// Resources defines resource requests and limits.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PodTemplateList contains a list of PodTemplate objects.
type PodTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodTemplate `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// BuildMetric is the Schema for the buildmetrics API.
// +kubebuilder:printcolumn:name="BUILD STATUS",type="string",JSONPath=".spec.buildStatus"
// +kubebuilder:printcolumn:name="DURATION",type="integer",JSONPath=".spec.durationSeconds"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type BuildMetric struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec BuildMetricSpec `json:"spec,omitempty"`
}

// BuildMetricSpec defines the desired state of BuildMetric.
type BuildMetricSpec struct {
	// BuildId is the identifier of the build this metric belongs to.
	BuildId string `json:"buildId"`
	// BuildStatus is the outcome of the build.
	// Valid values: Success, Failure, Aborted, Running.
	BuildStatus string `json:"buildStatus,omitempty"`
	// DurationSeconds is the wall-clock duration of the build in seconds.
	DurationSeconds int `json:"durationSeconds,omitempty"`
	// JobName is the Jenkins job name that produced this build.
	JobName string `json:"jobName,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// BuildMetricList contains a list of BuildMetric objects.
type BuildMetricList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BuildMetric `json:"items"`
}

// TemplateCatalogCondition is a generic status condition for catalogs and composed bundles.
type TemplateCatalogCondition struct {
	Type               string                 `json:"type"`
	Status             metav1.ConditionStatus `json:"status"`
	LastTransitionTime metav1.Time            `json:"lastTransitionTime,omitempty"`
	Reason             string                 `json:"reason,omitempty"`
	Message            string                 `json:"message,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// CatalogSource is the Schema for the catalogsources API.
//
// The object-level rule below binds the zero-source-field shape to the reserved
// name varroa-update-center (the update-center-backed variant). CEL at the root
// schema can read metadata.name and metadata.generateName but never
// metadata.namespace, so "reserved name only in the operator namespace" is a
// runtime guard in CatalogReconciler, not a validation guard.
// The has(self.spec) guards are load-bearing: spec is optional in the schema, so
// an object created with no spec block at all reaches this rule and a bare
// has(self.spec.repoURL) raises "no such key: spec" instead of the message below.
// +kubebuilder:validation:XValidation:rule="(has(self.metadata.name) && self.metadata.name == 'varroa-update-center') ? (!has(self.spec) || (!has(self.spec.repoURL) && !has(self.spec.ociRef))) : (has(self.spec) && (has(self.spec.repoURL) != has(self.spec.ociRef)))",message="the reserved source name 'varroa-update-center' requires neither repoURL nor ociRef; every other CatalogSource requires exactly one"
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="REPOSITORY",type="string",JSONPath=".spec.repoURL"
// +kubebuilder:printcolumn:name="ITEMS",type="integer",JSONPath=".status.itemCount"
// +kubebuilder:printcolumn:name="SYNC",type="string",JSONPath=".status.phase"
type CatalogSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CatalogSourceSpec   `json:"spec,omitempty"`
	Status            CatalogSourceStatus `json:"status,omitempty"`
}

// CatalogSourceSpec defines the desired state of CatalogSource.
// +kubebuilder:validation:XValidation:rule="!has(self.repoURL) || self.repoURL.startsWith('https://') || self.repoURL.startsWith('ssh://') || self.repoURL.startsWith('git@')",message="repoURL must start with https://, ssh://, or git@ (transport helpers like ext::/file:///fd:: are rejected)"
// +kubebuilder:validation:XValidation:rule="(has(self.repoURL)?1:0)+(has(self.ociRef)?1:0)<=1",message="at most one of repoURL or ociRef may be set"
type CatalogSourceSpec struct {
	RepoURL             string `json:"repoURL,omitempty"`
	OCIRef              string `json:"ociRef,omitempty"`
	Revision            string `json:"revision,omitempty"`
	Path                string `json:"path,omitempty"`                // catalog root; default "."
	SyncIntervalSeconds int    `json:"syncIntervalSeconds,omitempty"` // min 30, default 300
	SecretRef           string `json:"secretRef,omitempty"`           // git creds Secret (same ns)
	Trusted             bool   `json:"trusted,omitempty"`
}

// CatalogSyncPhase represents the sync phase of a CatalogSource.
type CatalogSyncPhase string

const (
	// CatalogSyncPending is the initial state before the first sync.
	CatalogSyncPending CatalogSyncPhase = "Pending"
	// CatalogSyncSyncing indicates a sync is in progress.
	CatalogSyncSyncing CatalogSyncPhase = "Syncing"
	// CatalogSyncReady indicates the source was synced successfully.
	CatalogSyncReady CatalogSyncPhase = "Ready"
	// CatalogSyncError indicates the last sync failed.
	CatalogSyncError CatalogSyncPhase = "Error"
)

// CatalogSourceStatus defines the observed state of CatalogSource.
type CatalogSourceStatus struct {
	Phase            CatalogSyncPhase           `json:"phase,omitempty"`
	ObservedRevision string                     `json:"observedRevision,omitempty"`
	LastSyncTime     *metav1.Time               `json:"lastSyncTime,omitempty"`
	ItemCount        int                        `json:"itemCount,omitempty"`
	Message          string                     `json:"message,omitempty"`
	Conditions       []TemplateCatalogCondition `json:"conditions,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// CatalogSourceList contains a list of CatalogSource objects.
type CatalogSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CatalogSource `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// CatalogItem is the Schema for the catalogitems API (operator-owned; users read only).
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="TYPE",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="SOURCE",type="string",JSONPath=".spec.sourceRef"
type CatalogItem struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CatalogItemSpec   `json:"spec,omitempty"`
	Status            CatalogItemStatus `json:"status,omitempty"`
}

// CatalogItemType is the kind of catalog item.
// +kubebuilder:validation:Enum=podtemplate;plugin;item;jcasc;rbac;pipeline-template;groovy
type CatalogItemType string

const (
	// CatalogItemPodTemplate is a pod template (JCasC kubernetes cloud templates).
	CatalogItemPodTemplate CatalogItemType = "podtemplate"
	// CatalogItemPlugin is a Jenkins plugin entry (plugins.yaml).
	CatalogItemPlugin CatalogItemType = "plugin"
	// CatalogItemItem is a Jenkins job/item definition (items.yaml).
	CatalogItemItem CatalogItemType = "item"
	// CatalogItemJCasC is a JCasC configuration fragment (jenkins.yaml).
	CatalogItemJCasC CatalogItemType = "jcasc"
	// CatalogItemRBAC is an RBAC role definition (rbac.yaml).
	CatalogItemRBAC CatalogItemType = "rbac"
	// CatalogItemPipelineTemplate is a reusable pipeline/multibranch job template
	// with typed parameters. Content uses the same item-YAML schema as
	// CatalogItemItem — this is a discoverability tag, not a new content format.
	CatalogItemPipelineTemplate CatalogItemType = "pipeline-template"
	// CatalogItemGroovy is an execution-only Groovy script for brood-operation
	// executeGroovy dispatch. Not usable as a ComposedBundle input.
	CatalogItemGroovy CatalogItemType = "groovy"
)

// CatalogItemSpec defines the desired state of CatalogItem.
type CatalogItemSpec struct {
	SourceRef   string            `json:"sourceRef"`
	Type        CatalogItemType   `json:"type"`
	DisplayName string            `json:"displayName,omitempty"`
	Description string            `json:"description,omitempty"`
	Path        string            `json:"path"`
	Version     string            `json:"version,omitempty"`
	Tags        []string          `json:"tags,omitempty"`
	Variables   []CatalogVariable `json:"variables,omitempty"`
	Requires    []string          `json:"requires,omitempty"`
}

// CatalogVariable declares a user-supplied variable for a catalog item.
type CatalogVariable struct {
	Name        string `json:"name"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	// Type is the parameter type: string|number|boolean|credentials. Omitting the field
	// means "string" — every pre-existing CatalogItem with untyped variables validates
	// unchanged. Note: the Enum marker below does not list "" — a client that sends an
	// explicit `type: ""` (as opposed to omitting the key entirely) would be rejected by
	// CRD schema validation before ValidateCatalogItem ever runs. Nothing in this codebase
	// writes an explicit empty string (Go zero value + `omitempty` always omits the key on
	// marshal), so this is a theoretical corner case, not a behavior change for any real
	// caller.
	// +kubebuilder:validation:Enum=string;number;boolean;credentials
	Type string `json:"type,omitempty"`
	// AllowedValues populates a choice/dropdown widget. Only valid when Type is "string" or
	// "number" (empty/unset); rejected by ValidateCatalogItem when set with boolean/credentials.
	AllowedValues []string `json:"allowedValues,omitempty"`
}

// CatalogItemStatus defines the observed state of CatalogItem.
type CatalogItemStatus struct {
	Content          string `json:"content,omitempty"`     // verbatim item YAML at sync time
	ContentHash      string `json:"contentHash,omitempty"` // sha256(Content)
	ObservedRevision string `json:"observedRevision,omitempty"`
	Valid            bool   `json:"valid"`
	Message          string `json:"message,omitempty"`

	// Closure is the resolver's own record of what it pinned, for items derived
	// from the update-center store. Content is a flat plugins.yaml fragment and
	// can express none of direct-vs-transitive, provenance, or the minimum in
	// force, so the detail view reads them from here rather than re-deriving
	// them (it has neither the store annotations nor the solver).
	// +optional
	Closure []CatalogItemClosureEntry `json:"closure,omitempty"`

	// Compat carries one advisory verdict per JenkinsVersionProfile. Verdicts
	// never set Valid=false and never block selection or provisioning.
	// +optional
	Compat []CatalogItemCompat `json:"compat,omitempty"`

	// Conditions carries CompatWarning for derived items.
	// +optional
	Conditions []TemplateCatalogCondition `json:"conditions,omitempty"`
}

// CatalogItemCompat is one profile's advisory compatibility verdict for a
// catalog item.
type CatalogItemCompat struct {
	// Profile names the JenkinsVersionProfile this verdict was computed against.
	Profile string `json:"profile"`
	// JenkinsVersion is the profile's effective deployed core version — its
	// spec.resolveVersion when set, else spec.version.
	// +optional
	JenkinsVersion string `json:"jenkinsVersion,omitempty"`
	// +kubebuilder:validation:Enum=compatible;core-too-old;dep-below-minimum;lock-too-old;unknown
	Verdict string `json:"verdict"`
	// +optional
	Message string `json:"message,omitempty"`
}

// CatalogItemClosureEntry is one pinned member of a derived item's dependency
// closure.
type CatalogItemClosureEntry struct {
	ArtifactID string `json:"artifactId"`
	Version    string `json:"version"`
	// Direct reports whether the root declared this dependency itself, as
	// opposed to it being pulled in transitively.
	// +optional
	Direct bool `json:"direct,omitempty"`
	// Provenance is "store" or "lock" — where the selected version came from.
	// +optional
	Provenance string `json:"provenance,omitempty"`
	// Minimum is the effective minimum in force, when one was declared.
	// +optional
	Minimum string `json:"minimum,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// CatalogItemList contains a list of CatalogItem objects.
type CatalogItemList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CatalogItem `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ComposedBundle is the Schema for the composedbundles API (user-authored).
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="ITEMS",type="integer",JSONPath=".status.itemCount"
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.phase"
type ComposedBundle struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ComposedBundleSpec   `json:"spec,omitempty"`
	Status            ComposedBundleStatus `json:"status,omitempty"`
}

// ComposedBundleSpec defines the desired state of ComposedBundle.
type ComposedBundleSpec struct {
	DisplayName        string            `json:"displayName,omitempty"`
	Description        string            `json:"description,omitempty"`
	Inputs             []ComposedInput   `json:"inputs"`                       // order = jcasc merge order
	Variables          map[string]string `json:"variables,omitempty"`          // composition-wide
	JcascMergeStrategy string            `json:"jcascMergeStrategy,omitempty"` // errorOnConflict(default)|override
}

// ComposedInput is a single input to a ComposedBundle. Exactly one of ItemRef, GitSource, or
// OciSource must be set.
// +kubebuilder:validation:XValidation:rule="(has(self.itemRef)?1:0)+(has(self.gitSource)?1:0)+(has(self.ociSource)?1:0)==1",message="exactly one of itemRef, gitSource, or ociSource must be set"
type ComposedInput struct {
	// ItemRef references a CatalogItem by name.
	// +optional
	ItemRef *ComposedItemRef `json:"itemRef,omitempty"`
	// GitSource references a git bundle repository.
	// +optional
	GitSource *GitBundleSource `json:"gitSource,omitempty"`
	// OCISource references an OCI artifact.
	// +optional
	OCISource *OCIBundleSource `json:"ociSource,omitempty"`
}

// GitBundleSource defines a git bundle repository input.
// +kubebuilder:validation:XValidation:rule="self.repoURL.startsWith('https://') || self.repoURL.startsWith('ssh://') || self.repoURL.startsWith('git@')",message="repoURL must use https://, ssh://, or scp-like git@host:path; transport helpers (ext::, file://) are not allowed"
type GitBundleSource struct {
	// Repository URL for the bundle.
	RepoURL string `json:"repoURL"`
	// Path within the repository to the bundle.
	Path string `json:"path"`
	// Revision (branch, tag, or commit SHA).
	Revision string `json:"revision,omitempty"`
	// SecretRef names a Secret in the same namespace for git auth.
	// +optional
	SecretRef string `json:"secretRef,omitempty"`
}

// OCIBundleSource defines an OCI artifact repository input.
type OCIBundleSource struct {
	// Ref is the OCI artifact reference.
	Ref string `json:"ref"`
	// SecretRef names a Secret in the same namespace for pull credentials.
	// +optional
	SecretRef string `json:"secretRef,omitempty"`
	// Path is an optional sub-path within the artifact.
	// +optional
	Path string `json:"path,omitempty"`
}

// ComposedItemRef is a reference to a CatalogItem within a ComposedBundle.
type ComposedItemRef struct {
	Name string `json:"name"`
	// Namespace of the CatalogItem. Empty = resolve in the bundle's own namespace
	// with operator-namespace fallback; set = exact lookup in that namespace only.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Namespace         string            `json:"namespace,omitempty"`
	PinnedContentHash string            `json:"pinnedContentHash,omitempty"` // empty = track latest
	Variables         map[string]string `json:"variables,omitempty"`         // highest precedence
}

// ComposedBundleRef references a ComposedBundle from a Controller.
type ComposedBundleRef struct {
	Name string `json:"name"`
	// Namespace of the ComposedBundle. Empty = the controller's own namespace.
	Namespace string `json:"namespace,omitempty"`
}

// ComposedBundlePhase represents the status phase of a ComposedBundle.
type ComposedBundlePhase string

const (
	// ComposedBundlePending indicates the bundle has not yet been composed.
	ComposedBundlePending ComposedBundlePhase = "Pending"
	// ComposedBundleReady indicates all items resolved and the bundle is valid.
	ComposedBundleReady ComposedBundlePhase = "Ready"
	// ComposedBundleDrifted indicates pinned items have changed upstream.
	ComposedBundleDrifted ComposedBundlePhase = "Drifted"
	// ComposedBundleInvalid indicates one or more referenced items are missing.
	ComposedBundleInvalid ComposedBundlePhase = "Invalid"
)

// ComposedBundleStatus defines the observed state of ComposedBundle.
type ComposedBundleStatus struct {
	Phase        ComposedBundlePhase        `json:"phase,omitempty"`
	ItemCount    int                        `json:"itemCount,omitempty"`
	ResolvedHash string                     `json:"resolvedHash,omitempty"` // sha256 over merged bundle (unresolved)
	MissingItems []string                   `json:"missingItems,omitempty"`
	DriftedItems []string                   `json:"driftedItems,omitempty"`
	Message      string                     `json:"message,omitempty"`
	Conditions   []TemplateCatalogCondition `json:"conditions,omitempty"`

	// ContentRef names the ConfigMap or Secret holding the materialized,
	// unresolved bundle content. Owned by this ComposedBundle.
	// +optional
	ContentRef string `json:"contentRef,omitempty"`

	// ObservedRevisions records the resolved SHA for each git input.
	// Key is the input index (e.g. "0", "1") or the item ref name.
	// +optional
	ObservedRevisions map[string]string `json:"observedRevisions,omitempty"`

	// ObservedGeneration is the metadata.generation last reconciled into status.
	// Used to force a recompose when spec.inputs changes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Errors holds materialize-time validation errors.
	// +optional
	Errors []string `json:"errors,omitempty"`

	// Warnings holds materialize-time validation warnings.
	// +optional
	Warnings []string `json:"warnings,omitempty"`

	// InputSummary mirrors spec.inputs one-to-one, in order, for list rendering.
	// +optional
	InputSummary []InputSummaryEntry `json:"inputSummary,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ComposedBundleList contains a list of ComposedBundle objects.
type ComposedBundleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ComposedBundle `json:"items"`
}

// InputSummaryEntry mirrors a single input in a ComposedBundle spec for list rendering.
type InputSummaryEntry struct {
	// Kind is "itemRef", "gitSource", or "ociSource".
	Kind string `json:"kind"`
	// Type is the referenced CatalogItem's spec.type verbatim
	// ("jcasc"|"plugin"|"podtemplate"|"rbac"|"item"|"pipeline-template") for itemRef, or
	// "git" for gitSource.
	Type string `json:"type"`
	// Namespace the itemRef resolved in (explicit, bundle, or operator namespace);
	// empty when unresolved or for gitSource entries.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:scope=Cluster

// ProvisioningDefaults is the Schema for the provisioningdefaults API (cluster-scoped).
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="STORAGECLASS",type="string",JSONPath=".spec.storageClass"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type ProvisioningDefaults struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProvisioningDefaultsSpec   `json:"spec,omitempty"`
	Status ProvisioningDefaultsStatus `json:"status,omitempty"`
}

// ProvisioningDefaultsSpec defines the desired state of ProvisioningDefaults.
type ProvisioningDefaultsSpec struct {
	// RootDomain is the root domain for auto-derived ingress hosts.
	RootDomain         string            `json:"rootDomain,omitempty"`
	IngressAnnotations map[string]string `json:"ingressAnnotations,omitempty"`
	DefaultCPU         string            `json:"defaultCPU,omitempty"`
	DefaultMemory      string            `json:"defaultMemory,omitempty"`
	DefaultStorage     string            `json:"defaultStorage,omitempty"`
	DefaultPlugins     []PluginEntry     `json:"defaultPlugins,omitempty"`
	DefaultVersion     string            `json:"defaultVersion,omitempty"`
	StorageClass       string            `json:"storageClass,omitempty"`
	StorageSizeGB      int               `json:"storageSizeGB,omitempty"`
	// IngressClassName is the ingress class name for controller Ingresses.
	IngressClassName string `json:"ingressClassName,omitempty"`
	// ImagePullSecrets is a list of image pull secret names to attach to StatefulSet pods.
	ImagePullSecrets []string `json:"imagePullSecrets,omitempty"`
	// ProvisioningTimeoutSec is the max seconds to wait for provisioning.
	ProvisioningTimeoutSec int `json:"provisioningTimeoutSec,omitempty"`
	// CommandDeadlineSec bounds each mite command (desired-state apply). 0 = default (1200s/20m).
	CommandDeadlineSec int `json:"commandDeadlineSec,omitempty"`
	// LiveFingerprintIntervalSec is the mite's live-drift fingerprint cadence, seconds.
	// 0/unset ⇒ default 600 (10m). A negative value disables fingerprinting brood-wide.
	// +optional
	LiveFingerprintIntervalSec int `json:"liveFingerprintIntervalSec,omitempty"`
	// DefaultReconciliationPolicy is the default reconciliation policy for
	// controllers that don't specify their own.
	// +optional
	DefaultReconciliationPolicy *ReconciliationPolicy `json:"defaultReconciliationPolicy,omitempty"`
	// DefaultNamespace is the namespace the creation wizard preselects. Empty means "varroa".
	DefaultNamespace string `json:"defaultNamespace,omitempty"`
	// Namespaces is an optional list of additional provisionable namespaces.
	Namespaces []string `json:"namespaces,omitempty"`
	// SizePresets are the S/M/L resource cards offered by the wizard.
	SizePresets []SizePreset `json:"sizePresets,omitempty"`
	// PluginUpdateCenterURL is the Jenkins update-center metadata URL for plugin
	// resolution (maps to JENKINS_UC). When non-empty, plugins-init uses this mirror.
	// +optional
	PluginUpdateCenterURL string `json:"pluginUpdateCenterURL,omitempty"`
	// PluginUpdateCenterDownloadURL is the Jenkins update-center binary download URL
	// (maps to JENKINS_UC_DOWNLOAD_URL). When non-empty, plugins-init uses this mirror.
	// +optional
	PluginUpdateCenterDownloadURL string `json:"pluginUpdateCenterDownloadURL,omitempty"`
	// BroodPolicy constrains which brood operation verbs may run in this cluster.
	// Nil means every verb is allowed — the singleton is optional and its absence
	// must not disable functionality.
	// +optional
	BroodPolicy *BroodPolicy `json:"broodPolicy,omitempty"`
}

// BroodPolicy constrains brood operations cluster-wide. It is enforced by the
// operator in the BroodOperation reconciler, not by the BFF: kubectl and GitOps
// create BroodOperation objects directly and never traverse the HTTP API, so an
// API-only check would be advisory. The BFF performs the same check purely so
// the UI can refuse before creating an object that would immediately fail.
type BroodPolicy struct {
	// ExecuteGroovy governs the executeGroovy verb, which runs arbitrary Groovy
	// on a controller's script console with Administer rights.
	// +optional
	ExecuteGroovy *ExecuteGroovyPolicy `json:"executeGroovy,omitempty"`
}

// ExecuteGroovyPolicy governs the executeGroovy brood verb.
type ExecuteGroovyPolicy struct {
	// Enabled turns the verb on or off cluster-wide. Nil means enabled: the verb
	// is CloudBees-parity functionality, and a policy object that only some
	// clusters have must not change behaviour by its absence.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// AllowedNamespaces restricts which namespaces may host an executeGroovy
	// BroodOperation. It is matched against the BroodOperation's OWN namespace,
	// not its targets': the operation is the thing being authorized, and a single
	// operation may fan out to targets in many namespaces.
	//
	// Empty means every namespace, when Enabled is not false. A non-empty list
	// with Enabled explicitly false is still disabled — Enabled is the outer gate.
	// +optional
	AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`
}

// ExecuteGroovyAllowed reports whether an executeGroovy BroodOperation in
// opNamespace is permitted, and why not when it is not.
//
// A nil receiver, a nil ExecuteGroovy, and a nil Enabled all mean allowed. This
// is the single decision point: the operator enforces with it and the BFF
// pre-checks with it, so the two can never drift into different answers.
func (p *BroodPolicy) ExecuteGroovyAllowed(opNamespace string) (bool, string) {
	if p == nil || p.ExecuteGroovy == nil {
		return true, ""
	}
	eg := p.ExecuteGroovy
	if eg.Enabled != nil && !*eg.Enabled {
		return false, "executeGroovy is disabled by ProvisioningDefaults.broodPolicy"
	}
	if len(eg.AllowedNamespaces) == 0 {
		return true, ""
	}
	for _, ns := range eg.AllowedNamespaces {
		if ns == opNamespace {
			return true, ""
		}
	}
	return false, fmt.Sprintf(
		"executeGroovy is not permitted in namespace %s; ProvisioningDefaults.broodPolicy allows %s",
		opNamespace, strings.Join(eg.AllowedNamespaces, ", "))
}

// ProvisioningDefaultsStatus defines the observed state.
type ProvisioningDefaultsStatus struct {
	Conditions []ProvisioningDefaultsCondition `json:"conditions,omitempty"`
}

// ProvisioningDefaultsCondition is a status condition for ProvisioningDefaults.
type ProvisioningDefaultsCondition struct {
	Type               string                 `json:"type"`
	Status             metav1.ConditionStatus `json:"status"`
	LastTransitionTime metav1.Time            `json:"lastTransitionTime,omitempty"`
	Reason             string                 `json:"reason,omitempty"`
	Message            string                 `json:"message,omitempty"`
}

// SizePreset defines a named resource size template for the wizard.
type SizePreset struct {
	Name    string `json:"name"`
	CPU     string `json:"cpu"`
	Memory  string `json:"memory"`
	Storage string `json:"storage"`
}

// ProvisioningDefaultsList contains a list of ProvisioningDefaults objects.
type ProvisioningDefaultsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProvisioningDefaults `json:"items"`
}

// VarroaRole is the Schema for the varroaroles API.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="BUILTIN",type="string",JSONPath=".metadata.labels.varroa\\.dev/builtin"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type VarroaRole struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              VarroaRoleSpec `json:"spec,omitempty"`
}

// VarroaRoleSpec defines the desired state of VarroaRole.
type VarroaRoleSpec struct {
	// APIRules defines the BFF API authorization rules for this role.
	// +optional
	APIRules []APIRule `json:"apiRules,omitempty"`
	// JenkinsPermissions defines the Jenkins data-plane permissions for this role.
	// Deprecated: use JenkinsRoleRef to reference a JenkinsRole CR instead.
	// +optional
	JenkinsPermissions []string `json:"jenkinsPermissions,omitempty"`
	// JenkinsRoleRef links this API role to a data-plane JenkinsRole.
	// The referenced JenkinsRole MUST be RoleType=Global.
	// +optional
	JenkinsRoleRef string `json:"jenkinsRoleRef,omitempty"`
}

// APIRule defines a single API authorization rule with resources and verbs.
type APIRule struct {
	Resources []string `json:"resources"`
	Verbs     []string `json:"verbs"`
}

// VarroaRoleList contains a list of VarroaRole objects.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type VarroaRoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VarroaRole `json:"items"`
}

// VarroaRoleBinding is the Schema for the varroarolebindings API.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="ROLE",type="string",JSONPath=".spec.roleRef"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type VarroaRoleBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              VarroaRoleBindingSpec `json:"spec,omitempty"`
}

// VarroaRoleBindingSpec defines the desired state of VarroaRoleBinding.
type VarroaRoleBindingSpec struct {
	Subjects []SubjectRef            `json:"subjects"`
	RoleRef  string                  `json:"roleRef"`
	Scope    *VarroaRoleBindingScope `json:"scope,omitempty"`
}

// SubjectRef references a subject by kind and name.
type SubjectRef struct {
	// Kind is the subject kind: "Group" or "User".
	Kind string `json:"kind"`
	// Name is the OIDC claim value (group name or user subject).
	Name string `json:"name"`
}

// VarroaRoleBindingScope defines the scope where a binding applies.
type VarroaRoleBindingScope struct {
	// Namespaces is an optional allow-list of Kubernetes namespaces.
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`
	// ControllerSelector is an optional label selector over Controller CR labels.
	// +optional
	ControllerSelector *metav1.LabelSelector `json:"controllerSelector,omitempty"`
}

// VarroaRoleBindingList contains a list of VarroaRoleBinding objects.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type VarroaRoleBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VarroaRoleBinding `json:"items"`
}

// JenkinsRole is a pure Jenkins permission set (data plane).
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:scope=Cluster
type JenkinsRole struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              JenkinsRoleSpec `json:"spec,omitempty"`
}

// JenkinsRoleSpec defines the desired state of JenkinsRole.
type JenkinsRoleSpec struct {
	// RoleType is "Global", "Item", or "Agent"; default "Global".
	// +optional
	RoleType string `json:"roleType,omitempty"`
	// Permissions is the list of Jenkins permission IDs.
	Permissions []string `json:"permissions"`
	// Description is a human-readable description of the role.
	// +optional
	Description string `json:"description,omitempty"`
}

// JenkinsRoleList contains a list of JenkinsRole objects.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type JenkinsRoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []JenkinsRole `json:"items"`
}

// JenkinsRoleBinding binds subjects to a JenkinsRole with controller
// and in-Jenkins scope.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:scope=Cluster
type JenkinsRoleBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              JenkinsRoleBindingSpec `json:"spec,omitempty"`
}

// JenkinsRoleBindingSpec defines the desired state of JenkinsRoleBinding.
type JenkinsRoleBindingSpec struct {
	// Subjects references the subjects (users/groups) this binding grants to.
	Subjects []SubjectRef `json:"subjects"`
	// RoleRef is the name of the JenkinsRole to bind.
	RoleRef string `json:"roleRef"`
	// ControllerScope restricts which controllers this binding applies to.
	// +optional
	ControllerScope *VarroaRoleBindingScope `json:"controllerScope,omitempty"`
	// JenkinsScope defines the in-Jenkins scope. nil means Global.
	// +optional
	JenkinsScope *JenkinsScope `json:"jenkinsScope,omitempty"`
}

// JenkinsScope defines the in-Jenkins scope for a binding.
type JenkinsScope struct {
	// Type is "Global", "Folder", or "Pattern".
	Type string `json:"type"`
	// Folder is the folder path, e.g. "team-a/project-x". Only for Type=Folder.
	// +optional
	Folder string `json:"folder,omitempty"`
	// Propagate is "None", "Children", or "Subtree". Only for Type=Folder.
	// +optional
	Propagate string `json:"propagate,omitempty"`
	// Pattern is the raw regex pattern. Only for Type=Pattern.
	// +optional
	Pattern string `json:"pattern,omitempty"`
}

// JenkinsRoleBindingList contains a list of JenkinsRoleBinding objects.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type JenkinsRoleBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []JenkinsRoleBinding `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=cc

// ControllerClass is the Schema for the controllerclasses API. A ControllerClass
// layers cluster-level defaults between ProvisioningDefaults and each Controller's
// own spec, selected via spec.className.
type ControllerClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ControllerClassSpec   `json:"spec,omitempty"`
	Status            ControllerClassStatus `json:"status,omitempty"`
}

// ControllerClassSpec defines the desired state of ControllerClass. Fields that
// map to a corev1 concept (Resources, Affinity, SecurityContext, Tolerations) use
// the literal corev1 types — never Varroa wrappers.
type ControllerClassSpec struct {
	// NodeSelector constrains which nodes the controller pod can run on.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Tolerations allow the controller pod to tolerate node taints.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// Affinity constrains pod scheduling.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
	// SecurityContext sets the pod-level security context.
	// +optional
	SecurityContext *corev1.PodSecurityContext `json:"securityContext,omitempty"`
	// PodLabels are merged onto the controller pod template metadata.
	// +optional
	PodLabels map[string]string `json:"podLabels,omitempty"`
	// PodAnnotations are merged onto the controller pod template metadata.
	// +optional
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`
	// IngressClassName is the default ingress class for controllers in this class.
	// +optional
	IngressClassName string `json:"ingressClassName,omitempty"`
	// IngressAnnotations are merged onto the controller ingress annotations.
	// +optional
	IngressAnnotations map[string]string `json:"ingressAnnotations,omitempty"`
	// Resources defines CPU/memory resource requests and limits for the Jenkins container.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// Persistence configures the Jenkins home persistent volume claim defaults.
	// +optional
	Persistence *PersistenceSpec `json:"persistence,omitempty"`
	// Probes tunes the Jenkins container health probes.
	// +optional
	Probes *ProbesSpec `json:"probes,omitempty"`
	// Mite defines the mite sidecar configuration for controllers in this class.
	// +optional
	Mite *ClassMiteSpec `json:"mite,omitempty"`
	// ImagePullSecrets are image-pull secret names for controllers in this class.
	// +optional
	ImagePullSecrets []string `json:"imagePullSecrets,omitempty"`
	// JvmOpts are JVM options prepended to JAVA_OPTS for controllers in this class.
	// +optional
	JvmOpts string `json:"jvmOpts,omitempty"`
}

// ClassMiteSpec defines the mite sidecar configuration for a ControllerClass.
type ClassMiteSpec struct {
	// Image is the container image for the mite sidecar.
	// +optional
	Image string `json:"image,omitempty"`
	// ImagePullPolicy overrides the pull policy for the mite sidecar image.
	// +optional
	ImagePullPolicy string `json:"imagePullPolicy,omitempty"`
}

// ControllerClassStatus is the observed state of a ControllerClass.
type ControllerClassStatus struct {
	// Conditions describe the current conditions of this ControllerClass.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// ControllerClassList contains a list of ControllerClass objects.
type ControllerClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ControllerClass `json:"items"`
}

// JenkinsVersionProfile pins a plugin set and a JCasC overlay to a Jenkins
// version or LTS line, and supplies the wizard version catalog metadata.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
type JenkinsVersionProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              JenkinsVersionProfileSpec   `json:"spec"`
	Status            JenkinsVersionProfileStatus `json:"status,omitempty"`
}

// JenkinsVersionProfileSpec is the desired state of a JenkinsVersionProfile.
type JenkinsVersionProfileSpec struct {
	// Version is the Jenkins version this profile pins. A 3-segment value
	// (e.g. "2.479.3") is an exact pin; a 2-segment value (e.g. "2.479") is an
	// LTS line matching any 2.479.x. Required.
	Version string `json:"version"`
	// +kubebuilder:validation:Enum=lts;weekly
	Channel     string `json:"channel"`
	Recommended bool   `json:"recommended,omitempty"`
	// EOL is an informational end-of-life date string (e.g. "2026-09-30").
	EOL string `json:"eol,omitempty"`
	// ResolveVersion is the exact 3-segment patch this profile's plugin set was
	// resolved against (LTS line profiles only). The operator deploys the
	// Jenkins core at this version rather than the bare line in Version, so the
	// running core is never older than the core the pinned plugins require
	// (see AggregatePluginPrerequisitesNotMetException / #185). Empty for
	// weekly/exact-pin profiles, where Version is already deployable as-is.
	ResolveVersion string `json:"resolveVersion,omitempty"`

	// PluginSetRef names a ConfigMap (in the operator namespace) holding the
	// fully-resolved pinned plugin set under key "plugins.yaml" (lockSet YAML:
	// `core:` seeds + `plugins:` [{artifactId, version}]). The operator copies
	// it into an owned ConfigMap (status.contentRef). Resolves Open Q1.
	PluginSetRef *ConfigMapRef `json:"pluginSetRef,omitempty"`

	// JCasC is a version-specific JCasC overlay merged on top of the
	// controller's composed bundle at provisioning time. Optional.
	JCasC *VersionJCasC `json:"jcasc,omitempty"`
}

// ConfigMapRef references a ConfigMap by name.
type ConfigMapRef struct {
	Name string `json:"name"`
}

// VersionJCasC is a version-specific JCasC overlay applied on top of a controller's composed bundle.
type VersionJCasC struct {
	// Content is the JCasC overlay YAML (strategic/ordered merge onto the
	// composed bundle's jenkins.yaml).
	Content string `json:"content,omitempty"`
	// RequiredPlugins lists artifactIds the overlay's config depends on. The
	// reconciler warns (non-blocking) when any is absent from the pinned set.
	// Resolves Open Q3 (no UC-metadata key→plugin map needed).
	RequiredPlugins []string `json:"requiredPlugins,omitempty"`
}

// JenkinsVersionProfileStatus is the observed state of a JenkinsVersionProfile.
type JenkinsVersionProfileStatus struct {
	// ContentRef is the operator-owned ConfigMap holding the materialized
	// plugin set actually served to provisioning.
	ContentRef  string                           `json:"contentRef,omitempty"`
	PluginCount int                              `json:"pluginCount,omitempty"`
	Conditions  []JenkinsVersionProfileCondition `json:"conditions,omitempty"`
}

// JenkinsVersionProfileCondition is a status condition for JenkinsVersionProfile.
type JenkinsVersionProfileCondition struct {
	Type               string                 `json:"type"`
	Status             metav1.ConditionStatus `json:"status"`
	LastTransitionTime metav1.Time            `json:"lastTransitionTime,omitempty"`
	Reason             string                 `json:"reason,omitempty"`
	Message            string                 `json:"message,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// JenkinsVersionProfileList contains a list of JenkinsVersionProfile objects.
type JenkinsVersionProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []JenkinsVersionProfile `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="PHASE",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="PLUGINS",type="integer",JSONPath=".status.pluginCount"
// +kubebuilder:printcolumn:name="STORAGE",type="string",JSONPath=".spec.storage.type"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"

// UpdateCenter is the Schema for the updatecenters API. It manages a
// cluster-scoped plugin update center with OCI or local storage.
type UpdateCenter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              UpdateCenterSpec   `json:"spec"`
	Status            UpdateCenterStatus `json:"status,omitempty"`
}

// UpdateCenterSpec defines the desired state of an UpdateCenter.
type UpdateCenterSpec struct {
	// Storage configures the update center's backing store. Required.
	Storage UpdateCenterStorage `json:"storage"`
	// PullThrough configures pull-through proxy behavior. Optional.
	// +optional
	PullThrough UpdateCenterPullThrough `json:"pullThrough,omitempty"`
	// Seed configures the initial plugin seed. Optional.
	// +optional
	Seed UpdateCenterSeed `json:"seed,omitempty"`
}

// UpdateCenterStorageType is the type of storage backend.
// +kubebuilder:validation:Enum=local;oci
type UpdateCenterStorageType string

// UpdateCenterStorage configures the update center's backing store.
// +kubebuilder:validation:XValidation:rule="self.type != 'oci' || has(self.oci)",message="oci storage requires spec.storage.oci"
// +kubebuilder:validation:XValidation:rule="self.type != 'local' || has(self.local)",message="local storage requires spec.storage.local"
// +kubebuilder:validation:XValidation:rule="!(has(self.oci) && has(self.local))",message="exactly one of spec.storage.oci or spec.storage.local may be set"
type UpdateCenterStorage struct {
	// Type is the storage backend type. Required.
	Type UpdateCenterStorageType `json:"type"`
	// OCI configures OCI-compatible registry storage. Required when type=oci.
	// +optional
	OCI *UpdateCenterOCIStorage `json:"oci,omitempty"`
	// Local configures local PVC storage. Required when type=local.
	// +optional
	Local *UpdateCenterLocalStorage `json:"local,omitempty"`
}

// UpdateCenterOCIStorage configures OCI-compatible registry storage.
type UpdateCenterOCIStorage struct {
	// Ref is the OCI image reference for the update center content.
	Ref string `json:"ref"`
	// ExistingSecret is the name of an existing secret for registry auth.
	// +optional
	ExistingSecret string `json:"existingSecret,omitempty"`
	// Insecure allows plain HTTP connections to the registry.
	// +optional
	Insecure bool `json:"insecure,omitempty"`
}

// UpdateCenterLocalStorage configures local PVC storage.
type UpdateCenterLocalStorage struct {
	// StorageClassName is the storage class for the PVC. Optional.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
	// Size is the requested persistent volume size (resource.Quantity string, default "10Gi").
	// +optional
	Size string `json:"size,omitempty"`
}

// UpdateCenterPullThrough configures pull-through proxy behavior.
type UpdateCenterPullThrough struct {
	// Enabled enables pull-through caching from an upstream update center.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// UpstreamURL is the upstream update center URL (default https://updates.jenkins.io).
	// +optional
	UpstreamURL string `json:"upstreamURL,omitempty"`
	// DownloadURL is the upstream download URL (default https://updates.jenkins.io/download).
	// +optional
	DownloadURL string `json:"downloadURL,omitempty"`
}

// UpdateCenterSeed configures the initial plugin seed.
type UpdateCenterSeed struct {
	// Refs is the list of plugin references to seed.
	// +optional
	Refs []string `json:"refs,omitempty"`
}

// UpdateCenterPhase is the phase of an UpdateCenter.
// +kubebuilder:validation:Enum=Pending;Ready;Degraded;Error
type UpdateCenterPhase string

// UpdateCenterStatus defines the observed state of an UpdateCenter.
type UpdateCenterStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase UpdateCenterPhase `json:"phase,omitempty"`
	// Conditions describe the current conditions of this UpdateCenter.
	// +optional
	Conditions []UpdateCenterCondition `json:"conditions,omitempty"`
	// PluginCount is the number of plugins stored in this update center.
	// +optional
	PluginCount int `json:"pluginCount,omitempty"`
	// StoreBytes is the total bytes stored.
	// +optional
	StoreBytes int64 `json:"storeBytes,omitempty"`
	// Gaps lists plugins required by profiles but missing from the store.
	// +optional
	Gaps []UpdateCenterGap `json:"gaps,omitempty"`
	// LastSyncTime is the last sync time.
	// +optional
	LastSyncTime metav1.Time `json:"lastSyncTime,omitempty"`
	// SeedImportedDigests lists digests of successfully imported seed plugins.
	// +optional
	SeedImportedDigests []string `json:"seedImportedDigests,omitempty"`
	// ResolvedMetadataSources lists the update-center metadata endpoints pull-through
	// consults for sha256 resolution: the weekly source first, then any operator-derived
	// LTS-line (dynamic-stable) sources. Read-only, operator-managed, bounded.
	// +optional
	// +listType=atomic
	ResolvedMetadataSources []string `json:"resolvedMetadataSources,omitempty"`
}

// UpdateCenterCondition is a status condition for an UpdateCenter.
type UpdateCenterCondition struct {
	// Type is the condition type.
	Type string `json:"type"`
	// Status is the condition status.
	Status metav1.ConditionStatus `json:"status"`
	// LastTransitionTime is the last time the condition changed.
	// +optional
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
	// Reason is a machine-readable reason for the condition.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message is a human-readable message for the condition.
	// +optional
	Message string `json:"message,omitempty"`
}

// UpdateCenterGap identifies a plugin missing from the store.
type UpdateCenterGap struct {
	// Plugin is the plugin artifact ID.
	Plugin string `json:"plugin"`
	// Version is the required plugin version.
	Version string `json:"version"`
	// RequiredBy identifies the profile or bundle that requires this plugin.
	RequiredBy string `json:"requiredBy"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// UpdateCenterList contains a list of UpdateCenter objects.
type UpdateCenterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UpdateCenter `json:"items"`
}

// BroodVerb is the action verb for a brood operation.
// +kubebuilder:validation:Enum=restart;reprovision;reconcile;stop;start;executeGroovy
type BroodVerb string

// Brood operation verbs.
const (
	BroodVerbRestart       BroodVerb = "restart"
	BroodVerbReprovision   BroodVerb = "reprovision"
	BroodVerbReconcile     BroodVerb = "reconcile"
	BroodVerbStop          BroodVerb = "stop"
	BroodVerbStart         BroodVerb = "start"
	BroodVerbExecuteGroovy BroodVerb = "executeGroovy"
)

// BroodOrder controls the dispatch order of targets.
// +kubebuilder:validation:Enum=rolloutWave;name
type BroodOrder string

// Brood dispatch orders.
const (
	BroodOrderRolloutWave BroodOrder = "rolloutWave"
	BroodOrderName        BroodOrder = "name"
)

// BroodFailurePolicy controls the behavior when a target fails.
// +kubebuilder:validation:Enum=FailFast;FailTidy;FailAtEnd
type BroodFailurePolicy string

// Brood failure policies.
const (
	BroodFailurePolicyFailFast  BroodFailurePolicy = "FailFast"
	BroodFailurePolicyFailTidy  BroodFailurePolicy = "FailTidy"
	BroodFailurePolicyFailAtEnd BroodFailurePolicy = "FailAtEnd"
)

// BroodOperationPhase is the phase of a brood operation.
// +kubebuilder:validation:Enum=Pending;Running;Suspended;Succeeded;Failed;Canceled
type BroodOperationPhase string

// Brood operation phases.
const (
	BroodOperationPhasePending   BroodOperationPhase = "Pending"
	BroodOperationPhaseRunning   BroodOperationPhase = "Running"
	BroodOperationPhaseSuspended BroodOperationPhase = "Suspended"
	BroodOperationPhaseSucceeded BroodOperationPhase = "Succeeded"
	BroodOperationPhaseFailed    BroodOperationPhase = "Failed"
	BroodOperationPhaseCanceled  BroodOperationPhase = "Canceled"
)

// BroodTargetState is the state of a single target within a brood operation.
// +kubebuilder:validation:Enum=Pending;Dispatched;Succeeded;Failed;Skipped
type BroodTargetState string

// Brood target states.
const (
	BroodTargetStatePending    BroodTargetState = "Pending"
	BroodTargetStateDispatched BroodTargetState = "Dispatched"
	BroodTargetStateSucceeded  BroodTargetState = "Succeeded"
	BroodTargetStateFailed     BroodTargetState = "Failed"
	BroodTargetStateSkipped    BroodTargetState = "Skipped"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="VERB",type="string",JSONPath=".spec.action.verb"
// +kubebuilder:printcolumn:name="PHASE",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="TOTAL",type="integer",JSONPath=".status.summary.total"
// +kubebuilder:printcolumn:name="SUCCEEDED",type="integer",JSONPath=".status.summary.succeeded"
// +kubebuilder:printcolumn:name="FAILED",type="integer",JSONPath=".status.summary.failed"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"

// BroodOperation is the Schema for the broodoperations API.
type BroodOperation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BroodOperationSpec   `json:"spec,omitempty"`
	Status BroodOperationStatus `json:"status,omitempty"`
}

// BroodOperationSpec defines the desired state of a BroodOperation.
// +kubebuilder:validation:XValidation:rule="self.action == oldSelf.action",message="spec.action is immutable"
// +kubebuilder:validation:XValidation:rule="self.targets == oldSelf.targets",message="spec.targets is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.execution) == has(oldSelf.execution)",message="spec.execution is immutable"
type BroodOperationSpec struct {
	// Action is the operation to perform. Required. Immutable.
	Action BroodAction `json:"action"`
	// Targets defines which controllers to target. Required. Immutable.
	Targets BroodTargets `json:"targets"`
	// Execution controls how the operation is executed. Optional, defaulted. Immutable.
	// NOTE: must stay a pointer — a value struct is never dropped by omitempty,
	// so full-object updates would serialize execution:{} and trip the
	// has(self.execution) == has(oldSelf.execution) CEL rule on CRs created
	// without an execution block.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.execution is immutable"
	// +optional
	Execution *BroodExecution `json:"execution,omitempty"`
	// Suspend pauses a running operation. This is the only mutable spec field.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
	// TTLSecondsAfterFinished bounds the lifetime of the CR after reaching a terminal phase. Default 604800.
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
}

// BroodAction defines the action to perform on each target.
// +kubebuilder:validation:XValidation:rule="self.verb == 'executeGroovy' ? has(self.groovy) : !has(self.groovy)",message="groovy is required iff verb is executeGroovy"
type BroodAction struct {
	// Verb is the operation to perform.
	// +kubebuilder:validation:Enum=restart;reprovision;reconcile;stop;start;executeGroovy
	Verb BroodVerb `json:"verb"`
	// Groovy carries the script for verb=executeGroovy. Required iff Verb ==
	// executeGroovy; must be absent for every other verb.
	// +optional
	Groovy *BroodGroovyAction `json:"groovy,omitempty"`
}

// BroodGroovyAction selects the script to run for verb=executeGroovy. Exactly
// one of Script or ItemRef must be set.
// +kubebuilder:validation:XValidation:rule="(has(self.script) ? 1 : 0) + (has(self.itemRef) ? 1 : 0) == 1",message="exactly one of script or itemRef must be set"
type BroodGroovyAction struct {
	// Script is an inline Groovy script.
	// +optional
	Script string `json:"script,omitempty"`
	// ItemRef resolves a CatalogItem of type groovy, using the same
	// local-first/operator-namespace-fallback resolution mechanics as
	// ComposedBundle itemRef inputs.
	// +optional
	ItemRef *ComposedItemRef `json:"itemRef,omitempty"`
}

// BroodTargets defines which controllers to target.
// +kubebuilder:validation:XValidation:rule="has(self.names) != has(self.selector)",message="exactly one of names or selector"
// +kubebuilder:validation:XValidation:rule="!has(self.names) || size(self.names) > 0",message="names must be non-empty when present"
// +kubebuilder:validation:XValidation:rule="!has(self.namespaces) || size(self.namespaces) > 0",message="namespaces must be non-empty when present"
// +kubebuilder:validation:XValidation:rule="!has(self.namespaces) || has(self.selector)",message="namespaces requires selector"
type BroodTargets struct {
	// Names is a list of target controller names. Exactly one of names or selector must be set.
	// In the team namespace, bare names; in the operator namespace, "ns/name" format.
	// +optional
	Names []string `json:"names,omitempty"`
	// Selector is a label selector for targeting controllers. Exactly one of names or selector must be set.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
	// Namespaces restricts the selector to specific namespaces. Operator-ns selector mode only.
	// Use ["all"] to target all namespaces.
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`
	// Filters further narrows the target set by controller properties.
	// +optional
	Filters *BroodTargetFilters `json:"filters,omitempty"`
}

// BroodTargetFilters narrows the target set by controller properties. Each filter is optional; all are ANDed.
type BroodTargetFilters struct {
	// Phase filters by controller status.phase equality.
	// +optional
	// +kubebuilder:validation:Enum=Pending;Provisioning;Running;Connected;Stopped;Failed
	Phase *ControllerPhase `json:"phase,omitempty"`
	// Version filters by controller spec.version exact match.
	// +optional
	Version *string `json:"version,omitempty"`
	// Bundle filters by controller spec.composedBundleRef.name exact match.
	// +optional
	Bundle *string `json:"bundle,omitempty"`
}

// BroodExecution controls execution parameters.
type BroodExecution struct {
	// MaxParallel is the maximum number of targets to dispatch concurrently. Default 1, minimum 1.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxParallel *int32 `json:"maxParallel,omitempty"`
	// Order controls target dispatch order. Default rolloutWave.
	// +optional
	// +kubebuilder:validation:Enum=rolloutWave;name
	Order BroodOrder `json:"order,omitempty"`
	// FailurePolicy controls behavior on target failure. Default FailTidy.
	// +optional
	// +kubebuilder:validation:Enum=FailFast;FailTidy;FailAtEnd
	FailurePolicy BroodFailurePolicy `json:"failurePolicy,omitempty"`
}

// BroodOperationStatus defines the observed state of a BroodOperation.
type BroodOperationStatus struct {
	// Phase is the current operation phase.
	// +optional
	// +kubebuilder:validation:Enum=Pending;Running;Suspended;Succeeded;Failed;Canceled
	Phase BroodOperationPhase `json:"phase,omitempty"`
	// StartedBy is the user who started the operation. Stamped by the BFF at create.
	// +optional
	StartedBy string `json:"startedBy,omitempty"`
	// StartedAt is when the operation moved from Pending to Running.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// FinishedAt is when the operation reached a terminal phase.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// Reason provides terminal-phase detail (e.g. TenancyViolation).
	// +optional
	Reason string `json:"reason,omitempty"`
	// Targets is the snapshot of target states, ordered as dispatched.
	// +optional
	Targets []BroodTargetStatus `json:"targets,omitempty"`
	// ScriptSnapshotRef names the owner-referenced ConfigMap holding the
	// once-resolved, byte-identical script content for an executeGroovy
	// operation whose script came from action.groovy.itemRef. Empty for
	// inline scripts and for every other verb.
	// +optional
	ScriptSnapshotRef string `json:"scriptSnapshotRef,omitempty"`
	// Summary is the aggregate counts.
	Summary BroodSummary `json:"summary,omitempty"`
	// ObservedGeneration is the metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// BroodTargetStatus is the state of a single target within a brood operation.
type BroodTargetStatus struct {
	// Namespace is the target controller's namespace.
	Namespace string `json:"namespace"`
	// Name is the target controller's name.
	Name string `json:"name"`
	// Wave is the controller's rolloutWave at resolution.
	Wave int32 `json:"wave"`
	// State is the current state of this target.
	// +kubebuilder:validation:Enum=Pending;Dispatched;Succeeded;Failed;Skipped
	State BroodTargetState `json:"state"`
	// Reason explains why the target was Skipped or Failed.
	// +optional
	Reason string `json:"reason,omitempty"`
	// DispatchedAt is when the target was dispatched.
	// +optional
	DispatchedAt *metav1.Time `json:"dispatchedAt,omitempty"`
	// FinishedAt is when the target reached a terminal state.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// Output holds verb-specific dispatch output for verbs that produce a
	// response body worth surfacing (currently executeGroovy only), truncated
	// to 4096 bytes on a UTF-8-safe boundary. Empty for verbs that don't
	// produce one.
	// +optional
	Output string `json:"output,omitempty"`
}

// BroodSummary aggregates target counts for a brood operation.
type BroodSummary struct {
	// Total is the total number of resolved targets.
	Total int `json:"total"`
	// Succeeded is the number of succeeded targets.
	Succeeded int `json:"succeeded"`
	// Failed is the number of failed targets.
	Failed int `json:"failed"`
	// Skipped is the number of skipped targets.
	Skipped int `json:"skipped"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// BroodOperationList contains a list of BroodOperation objects.
type BroodOperationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BroodOperation `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status

// BroodSchedule is the Schema for the broodschedules API.
type BroodSchedule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BroodScheduleSpec   `json:"spec"`
	Status BroodScheduleStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// BroodScheduleList contains a list of BroodSchedule objects.
type BroodScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BroodSchedule `json:"items"`
}

// BroodScheduleSpec defines the desired state of a BroodSchedule.
type BroodScheduleSpec struct {
	// Schedule is the cron expression for the schedule. Required.
	Schedule string `json:"schedule"`
	// Suspend suspends the schedule. Default false.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
	// ConcurrencyPolicy governs how to treat overlapping executions.
	// When waitForCompletion is true, this governs the underlying brood run's
	// overlap; when false, it only guards trigger-Job overlap, not run overlap.
	// +kubebuilder:default=Forbid
	// +optional
	ConcurrencyPolicy batchv1.ConcurrencyPolicy `json:"concurrencyPolicy,omitempty"`
	// StartingDeadlineSeconds is the deadline in seconds for starting the Job if it
	// misses its scheduled time. Default nil (no deadline).
	// +optional
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`
	// SuccessfulJobsHistoryLimit is the number of successful finished Jobs to retain.
	// +kubebuilder:default=3
	// +optional
	SuccessfulJobsHistoryLimit *int32 `json:"successfulJobsHistoryLimit,omitempty"`
	// FailedJobsHistoryLimit is the number of failed finished Jobs to retain.
	// +kubebuilder:default=1
	// +optional
	FailedJobsHistoryLimit *int32 `json:"failedJobsHistoryLimit,omitempty"`
	// WaitForCompletion determines whether the CronJob should wait for the brood
	// operation run to complete before considering the Job successful.
	// +kubebuilder:default=true
	WaitForCompletion bool `json:"waitForCompletion"`
	// Template is the template for the brood operation to execute.
	Template BroodScheduleTemplate `json:"template"`
}

// BroodScheduleTemplate defines the template for a scheduled brood operation.
type BroodScheduleTemplate struct {
	// Targets defines which controllers to target. Required.
	Targets BroodTargets `json:"targets"`
	// Action is the operation to perform. Required.
	Action BroodAction `json:"action"`
	// Clusters is the list of clusters to target.
	// +optional
	Clusters []string `json:"clusters,omitempty"`
	// Execution controls how the operation is executed. Optional.
	// +optional
	Execution *BroodExecution `json:"execution,omitempty"`
	// TTLSecondsAfterFinished bounds the lifetime of the operation after completion.
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
}

// BroodScheduleStatus defines the observed state of a BroodSchedule.
type BroodScheduleStatus struct {
	// LastScheduleTime is the last time the CronJob was scheduled.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`
	// LastSuccessfulTime is the last time the CronJob completed successfully.
	// +optional
	LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`
	// Active is a list of references to active Jobs.
	// +optional
	Active []corev1.ObjectReference `json:"active,omitempty"`
	// Reason provides detail about any schedule-level issues (e.g. tenancy violation).
	// +optional
	Reason string `json:"reason,omitempty"`
}

// PluginInventoryStatus is the bounded plugin inventory summary written to
// Controller.status by the Connected-phase plugin classifier.
type PluginInventoryStatus struct {
	// Hash is the installed_plugins_hash from the heartbeat the inventory was
	// classified against.
	Hash string `json:"hash"`

	// CollectedAt is when the inventory was collected by the mite.
	CollectedAt *metav1.Time `json:"collectedAt,omitempty"`

	// ObservedAt is when this classification was computed by the operator.
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`

	// Source is either "jenkins-api" or "filesystem".
	Source string `json:"source"`

	// Stale is true when the inventory can no longer be classified (mite
	// disconnected, collection failed, or read model lost).
	// omitempty deliberately NOT used: false must clear a prior true under
	// merge-patch (RFC 7386 — absent key means "leave unchanged").
	Stale bool `json:"stale"`

	// Degraded is true when the inventory came from the filesystem fallback
	// (flags unobservable, detached/bundled unavailable).
	// omitempty deliberately NOT used: false must clear a prior true.
	Degraded bool `json:"degraded"`

	// BootstrapApproximate is true when the bootstrap closure could not be
	// confirmed against an observed graph.
	// omitempty deliberately NOT used: false must clear a prior true.
	BootstrapApproximate bool `json:"bootstrapApproximate"`

	// OptionalEdgesDropped is true when optional dependency edges were dropped
	// due to payload budget constraints.
	// omitempty deliberately NOT used: false must clear a prior true.
	OptionalEdgesDropped bool `json:"optionalEdgesDropped"`

	// Truncated is true when the inventory was truncated at the mite side.
	// omitempty deliberately NOT used: false must clear a prior true.
	Truncated bool `json:"truncated"`

	// Total is the installed plugin count.
	Total int `json:"total"`

	// Counts maps provenance class label to installed plugin count. Absent
	// keys mean not determinable, never zero.
	Counts map[string]int `json:"counts,omitempty"`

	// Drift lists plugins in class 6 (unmanaged) and class 5 (optional-dependency),
	// capped at MaxItems. Class 6 before class 5, then by name.
	// +kubebuilder:validation:MaxItems=50
	Drift []PluginInventoryDriftEntry `json:"drift,omitempty"`

	// VersionDrift lists declared plugins whose installed version differs from
	// the declared version (ahead/behind/missing), capped at MaxItems.
	// +kubebuilder:validation:MaxItems=50
	VersionDrift []PluginInventoryDriftEntry `json:"versionDrift,omitempty"`

	// DriftTruncated is true when either drift or versionDrift was capped.
	// omitempty deliberately NOT used: false must clear a prior true.
	DriftTruncated bool `json:"driftTruncated"`

	// PendingCollect records an in-flight COLLECT_PLUGIN_INVENTORY command.
	// Emitted as explicit null when clearing (merge-patch omitempty trap).
	// omitempty deliberately NOT used: nil must marshal as explicit null so
	// the merge-patch removes the key rather than leaving it unchanged.
	//
	// +optional and +nullable are load-bearing and must stay with that choice:
	// without omitempty, controller-gen marks the field required and
	// non-nullable, and the API server then REJECTS the very explicit null this
	// field exists to send — wedging every status patch for any controller
	// whose collect is not in flight.
	// +optional
	// +nullable
	PendingCollect *PendingCollect `json:"pendingCollect"`
}

// PluginInventoryDriftEntry is one row of the drift or version-drift list.
type PluginInventoryDriftEntry struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Class   string `json:"class,omitempty"`   // provenance class label (R19)
	Verdict string `json:"verdict,omitempty"` // ahead/behind/missing (version drift only)
	Message string `json:"message,omitempty"` // human-readable annotation
}

// PendingCollect tracks an in-flight COLLECT_PLUGIN_INVENTORY command.
type PendingCollect struct {
	CommandID string      `json:"commandId"`
	IssuedAt  metav1.Time `json:"issuedAt"`
}

// PluginInventoryDrift condition false reasons.
const (
	// ReasonNoDrift indicates no unmanaged plugins were found in a healthy inventory.
	ReasonNoDrift = "NoDrift"
	// ReasonDegraded indicates the inventory source is degraded, so drift cannot
	// be confirmed.
	ReasonDegraded = "Degraded"
	// ReasonIndeterminate indicates the inventory is indeterminate (e.g. edges
	// dropped), so drift cannot be confirmed.
	ReasonIndeterminate = "Indeterminate"
	// ReasonStale indicates the inventory is stale (mite disconnected, collection
	// failed, or read model lost).
	ReasonStale = "Stale"
)
