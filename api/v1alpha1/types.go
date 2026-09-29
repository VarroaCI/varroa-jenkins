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
		&CatalogSource{}, &CatalogSourceList{},
		&CatalogItem{}, &CatalogItemList{},
		&ComposedBundle{}, &ComposedBundleList{},
		&ProvisioningDefaults{}, &ProvisioningDefaultsList{},
		&JenkinsVersionProfile{}, &JenkinsVersionProfileList{},
		&UpdateCenter{}, &UpdateCenterList{},
		&ControllerClass{}, &ControllerClassList{},
		&Team{}, &TeamList{},
		&Runner{}, &RunnerList{},
		&CellPool{}, &CellPoolList{},
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
	// keeps 1 replica; "Stopped" scales the StatefulSet to 0. Hibernation is
	// reported in status.hibernated, not requested here.
	// +kubebuilder:validation:Enum="";Running;Stopped
	PowerState string `json:"powerState,omitempty"`

	// Headless blocks the Jenkins web UI for every non-admin caller. A cell
	// is always headless. Set this to true to headless a one-tenant
	// controller too. Read this field only through v1alpha1.IsHeadless.
	// +optional
	Headless bool `json:"headless,omitempty"`

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

	// Cell configures this Controller as a cell: a disposable Jenkins JVM
	// with no durable state that hosts many tenants as top-level folders.
	// Enabled is immutable after creation, because a live StatefulSet never
	// changes its storage shape. MaxTenants stays mutable: it changes
	// placement capacity, never the storage shape.
	// +kubebuilder:validation:XValidation:rule="self.enabled == oldSelf.enabled",message="cell.enabled is immutable"
	// +optional
	Cell *CellSpec `json:"cell,omitempty"`
}

// CellSpec configures a Controller to run as a cell.
type CellSpec struct {
	// Enabled marks this Controller as a cell.
	Enabled bool `json:"enabled,omitempty"`
	// MaxTenants caps the number of tenants a placement pass may bind to
	// this cell.
	MaxTenants int32 `json:"maxTenants,omitempty"`
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
	// ControllerPhaseHibernated indicates the controller is hibernated (status.hibernated=true).
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

	// LastReconciledAt records the last time a reconcile pass completed without
	// error for this controller, in any phase.
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

	// Hibernated is true when the operator has parked this controller (scaled its
	// StatefulSet to 0) for automatic or manual hibernation. It is written only by
	// the status compare-and-swap helper SetHibernated, never by the
	// end-of-reconcile status patch.
	// +optional
	Hibernated bool `json:"hibernated,omitempty"`

	// HibernatedAt is when the controller was last hibernated. It is cleared when
	// the controller wakes, and like Hibernated is written only by SetHibernated.
	// +optional
	HibernatedAt *metav1.Time `json:"hibernatedAt,omitempty"`

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

	// Cell reports the placement and health of this Controller when it runs
	// as a cell. The placement pass writes only Tenants; the controller
	// reconciler writes the other fields. A merge patch that names only one
	// side leaves the other untouched.
	// +optional
	Cell *CellStatus `json:"cell,omitempty"`
}

// CellStatus reports the placement and health of a Controller running as a
// cell.
type CellStatus struct {
	// Tenants lists the Team names the placement pass has bound to this
	// cell.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Tenants []string `json:"tenants,omitempty"`

	// Warm is true while this cell is a free member of the warm pool: it
	// carries no tenant.
	// +optional
	Warm bool `json:"warm,omitempty"`

	// Evacuate is true when the roll gate has given up on this cell; every
	// tenant must move to another cell.
	// +optional
	Evacuate bool `json:"evacuate,omitempty"`

	// UnreadySince is when CellReady first went False for a reason other
	// than the three configuration gates. Cleared, together with RolledAt
	// and Evacuate, on recovery.
	// +optional
	UnreadySince *metav1.Time `json:"unreadySince,omitempty"`

	// RolledAt is when the operator last deleted this cell's pod to recover
	// from an unready state. Compared against UnreadySince so a tick does
	// not delete the pod again on every pass.
	// +optional
	RolledAt *metav1.Time `json:"rolledAt,omitempty"`

	// AppliedTenants is the tenant set of the desired state whose apply
	// last succeeded. A tenant absent here has no folder building on this
	// cell.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	AppliedTenants []string `json:"appliedTenants,omitempty"`

	// TenantWarnings lists non-blocking findings the controller reconciler
	// records about a tenant during cell item assembly or the hibernation
	// gate. Rebuilt each tick.
	// +kubebuilder:validation:MaxItems=50
	// +optional
	TenantWarnings []TenantWarning `json:"tenantWarnings,omitempty"`

	// Instance is the running Jenkins pod's metadata.uid. Empty when no
	// pod runs. Written by the controller reconciler on every tick.
	// +optional
	Instance string `json:"instance,omitempty"`

	// PreviousInstance is the pod uid Instance held before its last
	// change. A controller with no cell role reports it as a dead
	// instance in its own tenant.sync announcement, so the recorder's
	// interrupted-run sweep can confirm a replaced pod is gone.
	// +optional
	PreviousInstance string `json:"previousInstance,omitempty"`

	// PreviousInstanceLastSeen is the last mite heartbeat recorded while
	// PreviousInstance was the running pod.
	// +optional
	PreviousInstanceLastSeen *metav1.Time `json:"previousInstanceLastSeen,omitempty"`

	// Rehydration holds one build-record rehydration entry per tenant on
	// this cell. The controller reconciler is its only writer.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Rehydration []CellRehydrationEntry `json:"rehydration,omitempty"`

	// PendingTenants holds one placement claim per tenant the placement
	// pass has chosen this cell for and has not yet added to Tenants. The
	// placement pass is its only writer. The pool drain skips a cell that
	// carries a live claim.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	PendingTenants []CellPendingTenant `json:"pendingTenants,omitempty"`
}

// CellPendingTenant is one placement claim on a cell. The placement pass
// writes it before it writes the Team fence, so a pool drain that re-read
// the cell earlier cannot delete the cell the tenant is about to use.
type CellPendingTenant struct {
	// Tenant is the Team name this claim belongs to.
	Tenant string `json:"tenant"`

	// ClaimedAt is when the placement pass wrote the claim. A claim older
	// than the claim window holds no cell, so a placement pass that stops
	// between the claim and the tenant patch cannot pin a cell.
	ClaimedAt metav1.Time `json:"claimedAt"`
}

// CellRehydrationEntry tracks one tenant's build-record rehydration bundle
// on a cell.
type CellRehydrationEntry struct {
	// Tenant is the Team name this entry belongs to.
	Tenant string `json:"tenant"`
	// CommandID correlates this entry with the desired-state command that
	// carried its bundle. Persisting it survives a controller restart,
	// which an in-memory map does not.
	// +optional
	CommandID string `json:"commandId,omitempty"`
	// BundleID identifies the rehydration bundle this entry was built
	// from.
	// +optional
	BundleID string `json:"bundleId,omitempty"`
	// Generation is the placement generation this entry was built for.
	Generation int64 `json:"generation,omitempty"`
	// CellInstance is the cell pod UID this entry was built for.
	CellInstance string `json:"cellInstance,omitempty"`
	// Records is the number of thin build records the bundle carried.
	Records int32 `json:"records,omitempty"`
	// Truncated is true when the bundle dropped changelogs or older
	// records to fit the byte budget.
	Truncated bool `json:"truncated,omitempty"`
	// UnseededJobs counts the jobs no bundle has covered yet.
	UnseededJobs int32 `json:"unseededJobs,omitempty"`
	// Applied is true once the mite reports this bundle applied with no
	// error and no deferred job, on the generation and cell instance this
	// entry names.
	Applied bool `json:"applied,omitempty"`
	// Deferred lists the job paths the mite could not seed, because the
	// job did not exist yet.
	// +kubebuilder:validation:MaxItems=50
	// +optional
	Deferred []string `json:"deferred,omitempty"`
	// Error is the whole-bundle rehydration failure the mite reported, or
	// empty.
	// +optional
	Error string `json:"error,omitempty"`
	// At is when the controller reconciler last wrote this entry.
	// +optional
	At *metav1.Time `json:"at,omitempty"`
}

// TenantWarning records one non-blocking finding about a tenant on a cell.
type TenantWarning struct {
	// Tenant is the Team name the warning is about.
	Tenant string `json:"tenant"`
	// Reason names the warning class.
	Reason string `json:"reason"`
	// Message is a human-readable description of the finding.
	Message string `json:"message"`
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
	// ReasonCoreOlderThanPluginBaseline is set when core(spec.version) < plugin-set baseline (a crash-inducing mismatch).
	ReasonCoreOlderThanPluginBaseline = "CoreOlderThanPluginBaseline"
	// ReasonUnparseableVersion is set when spec.version is unparseable and no profile vouches for it.
	ReasonUnparseableVersion = "UnparseableVersion"
	// ReasonCoreCompatible is set (with VersionUpgradeBlocked=False) when the requested core is compatible.
	ReasonCoreCompatible = "CoreCompatible"
	// ReasonPluginRollApproved is set when the operator clears a pending roll after an approval roll.
	ReasonPluginRollApproved = "PluginRollApproved"
	// ReasonPluginRollFailed is set when the plugins-init container fails to sync managed plugins.
	ReasonPluginRollFailed = "PluginRollFailed"
	// ReasonJenkinsBootFailed is set when the Jenkins container fails to reach Running.
	ReasonJenkinsBootFailed = "JenkinsBootFailed"
	// ReasonPluginConflict is set when a requested plugin version conflicts with the profile lock set.
	ReasonPluginConflict = "PluginConflict"
	// ReasonPluginPinConflict is set when a bundle-pinned plugin version conflicts with the
	// resolved plugin set, independently of ReasonPluginConflict.
	ReasonPluginPinConflict = "PluginPinConflict"
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
	// ReasonReconcileBlockedArtifactSecretFailed is set when the artifact Secret lookup or projection fails.
	ReasonReconcileBlockedArtifactSecretFailed = "ArtifactSecretFailed"
	// ReasonReconcileBlockedCellSecretsFailed is set when a pool cell's own
	// pool Secret is absent, empty, incomplete, or unreadable.
	ReasonReconcileBlockedCellSecretsFailed = "CellSecretsFailed"
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
	// ConditionJenkinsBootFailed is True while the Jenkins container itself is
	// crash-looping or cannot be pulled; the message carries the exit code and
	// restart count so a JCasC boot error is visible without reading pod status.
	// It stays meaningful once the controller is Connected: the mite is a
	// sibling container and keeps its stream while kubelet restarts only the
	// Jenkins container beside it, so a Connected controller never clears this
	// condition on connectivity alone. The mite's cached health verdict can
	// hold it False, but only a live pod inspection can turn it back off once
	// it is True.
	ConditionJenkinsBootFailed ControllerConditionType = "JenkinsBootFailed"
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
	// ConditionMiteStreamDegraded indicates the gateway's bus→gRPC bridge for
	// this controller's mite is broken: a JetStream KV watch or subscription
	// failed and is being retried with backoff. The mite still holds a healthy
	// gRPC stream and the controller stays Connected, but operator-published
	// desired state (JCasC, RBAC, items) is NOT reaching it. Detection only —
	// the gateway self-heals by retrying; this condition exists so the failure
	// is not silent.
	ConditionMiteStreamDegraded ControllerConditionType = "MiteStreamDegraded"
	// ConditionMiteConnected reports the mite command stream of a controller
	// that was Connected. The operator writes it only after the first stream
	// loss. False/StreamLost holds the controller in Running with the pod
	// unchanged. True/StreamRestored follows a reconnect.
	ConditionMiteConnected ControllerConditionType = "MiteConnected"
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
	// ConditionPluginPinConflict indicates a bundle-pinned plugin version
	// conflicts with the controller's resolved plugin set. It is a distinct
	// slot from ConditionPluginConflict: advisory only, never blocking, and
	// never participates in markReconcileBlocked.
	ConditionPluginPinConflict ControllerConditionType = "PluginPinConflict"
	// ConditionUpgradePending indicates a profile-driven version roll is held
	// because ProvisioningDefaults.Spec.UpgradePolicy is "manual". It is set by
	// upgradePolicyVersionRollGate and cleared by reconcileVersionRoll once the
	// controller's applied image converges with the desired image, regardless of
	// what unblocked it.
	ConditionUpgradePending ControllerConditionType = "UpgradePending"
	// ConditionCellReady indicates a cell controller (spec.cell.enabled) can
	// accept a tenant. See the CellReady reason constants below.
	ConditionCellReady ControllerConditionType = "CellReady"
	// ConditionWarmPool indicates whether a cell is a free member of the
	// warm pool: it is Ready and carries no tenant.
	ConditionWarmPool ControllerConditionType = "WarmPool"
)

// MiteConnected reasons.
const (
	// ReasonStreamLost is set when the mite stream of a Connected controller
	// stays lost past the grace period.
	ReasonStreamLost = "StreamLost"
	// ReasonStreamRestored is set when the mite reconnects after a
	// StreamLost.
	ReasonStreamRestored = "StreamRestored"
)

// CellReady reasons.
const (
	// ReasonRecordStoreUnsupported is set when recordStore.enabled is false.
	ReasonRecordStoreUnsupported = "RecordStoreUnsupported"
	// ReasonBlobTierUnsupported is set when recordStore.blobs.mode is not s3.
	ReasonBlobTierUnsupported = "BlobTierUnsupported"
	// ReasonArtifactStorageLocal is set when the ArtifactStorage condition
	// is not External.
	ReasonArtifactStorageLocal = "ArtifactStorageLocal"
	// ReasonNotProvisioned is set while the phase is below Connected.
	ReasonNotProvisioned = "NotProvisioned"
	// ReasonMiteUnreachable is set while the phase is Running and the mite
	// stream of a controller that was Connected is lost.
	ReasonMiteUnreachable = "MiteUnreachable"
	// ReasonConnected is set when the phase is Connected.
	ReasonConnected = "Connected"
	// ReasonCellRolled is set when the operator deleted the cell pod to
	// recover from an unready state. The original fault reason moves to
	// Condition.Message.
	ReasonCellRolled = "CellRolled"
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
// +kubebuilder:validation:XValidation:rule="!has(self.spec) || !has(self.spec.pipelines) || (size(self.metadata.name) <= 47 && !self.metadata.name.contains('.'))",message="a pipelines tenant name is at most 47 characters and contains no dot, because it names the agent Service"

// Team is a cluster-scoped group of users with namespace-scoped role bindings.
// The operator composes it into a Group + VarroaRoleBinding.
type Team struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TeamSpec   `json:"spec,omitempty"`
	Status            TeamStatus `json:"status,omitempty"`
}

// TeamSpec defines the desired state of a Team.
// +kubebuilder:validation:XValidation:rule="!has(self.pipelines) || size(self.pipelines.profile) > 0",message="pipelines.profile must be non-empty when pipelines is set"
// +kubebuilder:validation:XValidation:rule="!has(self.pipelines) || size(self.pipelines.agentNamespace) > 0",message="pipelines.agentNamespace must be non-empty when pipelines is set"
// +kubebuilder:validation:XValidation:rule="!has(self.pipelines) || self.pipelines.agentNamespace in self.namespaces",message="pipelines.agentNamespace must be one of namespaces"
// +kubebuilder:validation:XValidation:rule="!has(self.pipelines) || !has(self.pipelines.composedBundleRef) || (has(self.pipelines.composedBundleRef.namespace) && size(self.pipelines.composedBundleRef.namespace) > 0)",message="pipelines.composedBundleRef.namespace must be non-empty when composedBundleRef is set"
// +kubebuilder:validation:XValidation:rule="!has(self.pipelines) || !has(self.pipelines.composedBundleRef) || !has(self.pipelines.composedBundleRef.namespace) || self.pipelines.composedBundleRef.namespace in self.namespaces",message="pipelines.composedBundleRef.namespace must be one of namespaces"
// +kubebuilder:validation:XValidation:rule="!has(self.pipelines) || !has(self.pipelines.rehydrateDeadline) || (duration(self.pipelines.rehydrateDeadline) >= duration('1m') && duration(self.pipelines.rehydrateDeadline) <= duration('24h'))",message="pipelines.rehydrateDeadline must be between 1m and 24h"
// +kubebuilder:validation:XValidation:rule="!has(self.pipelines) || !has(self.pipelines.cronGrace) || (duration(self.pipelines.cronGrace) >= duration('1m') && duration(self.pipelines.cronGrace) <= duration('60m'))",message="pipelines.cronGrace must be between 1m and 60m"
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

	// Pipelines, when set, makes this Team a pipelines tenant: a placement
	// reconciler binds it to exactly one cell. A Team is cluster-scoped, so
	// no namespace default exists for AgentNamespace or ComposedBundleRef.
	// +optional
	Pipelines *TeamPipelinesSpec `json:"pipelines,omitempty"`
}

// TeamPipelinesSpec declares a Team's pipelines tenancy.
type TeamPipelinesSpec struct {
	// Profile names the JenkinsVersionProfile the placement pass resolves a
	// cell against.
	Profile string `json:"profile,omitempty"`
	// CellPool names the CellPool the placement pass selects a cell from.
	// An empty value resolves to the profile's default pool: the pool whose
	// name equals Profile. A tenant that needs a cell of its own names a
	// pool whose maxTenantsPerCell is 1.
	// +optional
	CellPool string `json:"cellPool,omitempty"`
	// AgentNamespace is the Kubernetes namespace this tenant's agents run
	// in. Must be one of spec.namespaces.
	AgentNamespace string `json:"agentNamespace,omitempty"`
	// ComposedBundleRef names the ComposedBundle this tenant's items come
	// from. Only its items.yaml is used. When set, its namespace must be
	// non-empty and one of spec.namespaces.
	// +optional
	ComposedBundleRef *ComposedBundleRef `json:"composedBundleRef,omitempty"`

	// RehydrateBuilds sets how many of a job's newest completed builds the
	// operator seeds when this tenant's cell changes. The newest
	// successful build is always seeded in addition. Default 10.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	RehydrateBuilds *int32 `json:"rehydrateBuilds,omitempty"`

	// RehydrateDeadline bounds how long the operator keeps retrying a
	// deferred rehydration seed before it stops and reports
	// RehydrationFailed. Default 30m, range 1m to 24h.
	// +kubebuilder:default="30m"
	// +optional
	RehydrateDeadline *metav1.Duration `json:"rehydrateDeadline,omitempty"`

	// InterruptedRetry selects whether an interrupted run retries on its
	// own. "manual" (default) leaves the retry to a user. "automatic"
	// retries once.
	// +kubebuilder:validation:Enum=manual;automatic
	// +kubebuilder:default=manual
	// +optional
	InterruptedRetry string `json:"interruptedRetry,omitempty"`

	// Concurrency selects this tenant's job concurrency policy. "allow"
	// (default) runs builds side by side. "cancelInProgress" cancels the
	// running build of the same job path when a new one starts.
	// +kubebuilder:validation:Enum=allow;cancelInProgress
	// +kubebuilder:default=allow
	// +optional
	Concurrency string `json:"concurrency,omitempty"`

	// CronBackstop turns the backstop cron scheduler on for this tenant.
	// Nil means on.
	// +kubebuilder:default=true
	// +optional
	CronBackstop *bool `json:"cronBackstop,omitempty"`

	// CronCatchUp selects how many missed cron occurrences the backstop
	// fires on wake. "one" (default) fires the newest missed occurrence.
	// "none" fires nothing.
	// +kubebuilder:validation:Enum=one;none
	// +kubebuilder:default=one
	// +optional
	CronCatchUp string `json:"cronCatchUp,omitempty"`

	// CronGrace bounds how long the backstop waits past a missed cron
	// occurrence before it fires. Default 5m, range 1m to 60m.
	// +kubebuilder:default="5m"
	// +optional
	CronGrace *metav1.Duration `json:"cronGrace,omitempty"`

	// Exports declares which of this tenant's jobs and locks it grants to
	// other tenants, and who may use each grant. An absent list exports
	// nothing.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Exports []TeamExport `json:"exports,omitempty"`

	// Members sets the number of active slots this tenant runs. Each slot
	// holds one cell. A job runs only on its home slot's holder. This field
	// is not spec.members, which lists local usernames.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16
	// +optional
	Members *int32 `json:"members,omitempty"`

	// Standby sets the number of warm members this tenant keeps. A standby
	// holds the folder and no slot. It takes a slot whose holder dies.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=8
	// +optional
	Standby *int32 `json:"standby,omitempty"`
}

// TeamExport is one grant a pipelines tenant makes to other tenants: a
// folder-relative job glob, or a list of prefix-relative lock-name globs,
// plus the Team names allowed to use it. Exactly one of Job or Locks must
// be set.
// +kubebuilder:validation:XValidation:rule="(has(self.job)?1:0)+(has(self.locks)?1:0)==1",message="exactly one of job or locks must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.job) || size(self.job) > 0",message="job must be non-empty when set"
// +kubebuilder:validation:XValidation:rule="!has(self.job) || !self.job.startsWith('/')",message="job must not start with /"
// +kubebuilder:validation:XValidation:rule="!has(self.locks) || self.locks.all(l, size(l) > 0)",message="a locks entry must be non-empty"
// +kubebuilder:validation:XValidation:rule="self.callers.all(c, size(c) > 0)",message="a callers entry must be non-empty"
type TeamExport struct {
	// Job is a folder-relative job glob this tenant exports. Exactly one of
	// Job or Locks must be set.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Job string `json:"job,omitempty"`

	// Locks lists globs over the bare names of this tenant's own locks.
	// Exactly one of Job or Locks must be set.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=512
	// +optional
	Locks []string `json:"locks,omitempty"`

	// Callers lists the Team names allowed to use this export. At least
	// one entry is required.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=253
	Callers []string `json:"callers,omitempty"`
}

// TeamStatus defines the observed state of a Team.
type TeamStatus struct {
	ObservedGeneration int64                `json:"observedGeneration,omitempty"`
	GroupRef           string               `json:"groupRef,omitempty"`
	BindingRef         string               `json:"bindingRef,omitempty"`
	NamespaceStates    []TeamNamespaceState `json:"namespaceStates,omitempty"`
	Conditions         []TeamCondition      `json:"conditions,omitempty"`

	// Placement reports the cell this tenant is bound to. Written only by
	// the placement pass inside TeamReconciler.
	// +optional
	Placement *TeamPlacementStatus `json:"placement,omitempty"`

	// Pipelines carries the pipelines tenancy's own status fields: the
	// door credentials and the teardown-purge retry count.
	// +optional
	Pipelines *TeamPipelinesStatus `json:"pipelines,omitempty"`

	// Rehydration reports this tenant's build-record rehydration state.
	// TeamReconciler copies it from the bound cell's own
	// Controller.status.cell.rehydration entry on the same tick that
	// writes Placement.
	// +optional
	Rehydration *TeamRehydrationStatus `json:"rehydration,omitempty"`

	// StaleAgentServiceNamespaces names every namespace a move left an
	// agent Service behind in. A move that crosses a namespace appends one,
	// and the tick that deletes that Service removes it again. The list
	// lives here, beside Placement, rather than on Pipelines: teardown
	// clears Pipelines in the same call that still owes these deletes.
	//
	// The list is durable because a move is not reversible. The recorder
	// already holds the new generation once the placement pass returns, so a
	// failed cleanup must never roll the placement back.
	// +optional
	StaleAgentServiceNamespaces []string `json:"staleAgentServiceNamespaces,omitempty"`
}

// TeamRehydrationStatus reports a pipelines tenant's build-record
// rehydration state.
type TeamRehydrationStatus struct {
	// Generation is the placement generation this state was built for.
	Generation int64 `json:"generation,omitempty"`
	// CellInstance is the cell pod UID this state was built for.
	CellInstance string `json:"cellInstance,omitempty"`
	// Records is the number of thin build records the last bundle
	// carried.
	Records int32 `json:"records,omitempty"`
	// Truncated is true when the last bundle dropped changelogs or older
	// records to fit the byte budget.
	Truncated bool `json:"truncated,omitempty"`
	// UnseededJobs counts the jobs no bundle has covered yet.
	UnseededJobs int32 `json:"unseededJobs,omitempty"`
	// Deferred lists the job paths the mite could not seed on the last
	// apply, because the job did not exist yet.
	// +kubebuilder:validation:MaxItems=50
	// +optional
	Deferred []string `json:"deferred,omitempty"`
	// Attempts counts the rehydration retries since the last applied
	// bundle.
	Attempts int32 `json:"attempts,omitempty"`
	// LastAttempt is when the operator last sent a rehydration bundle for
	// this tenant.
	// +optional
	LastAttempt *metav1.Time `json:"lastAttempt,omitempty"`
}

// TeamPlacementStatus reports the cell a pipelines tenant is bound to.
type TeamPlacementStatus struct {
	// Cluster is the cluster the bound cell runs in.
	Cluster string `json:"cluster,omitempty"`
	// Cell is the name of the bound cell Controller.
	Cell string `json:"cell,omitempty"`
	// CellNamespace is the namespace of the bound cell Controller.
	CellNamespace string `json:"cellNamespace,omitempty"`
	// Generation increases only when the bound cell changes. The fence
	// C6 owns rejects a claim carrying an older generation.
	Generation int64 `json:"generation,omitempty"`
	// Members reports every member of this tenant. The scalar fields above
	// keep describing slot 0.
	// +optional
	Members []TeamMemberStatus `json:"members,omitempty"`
}

// TeamMemberStatus reports one member of a pipelines tenant.
type TeamMemberStatus struct {
	// Cell is the name of the cell Controller that holds this member.
	Cell string `json:"cell,omitempty"`
	// CellNamespace is the namespace of that cell Controller.
	CellNamespace string `json:"cellNamespace,omitempty"`
	// Role is TeamMemberRoleActive or TeamMemberRoleStandby.
	Role string `json:"role,omitempty"`
	// Slot is set only on an active member.
	// +optional
	Slot *int32 `json:"slot,omitempty"`
	// Applied is true when the member's cell reports this tenant applied.
	Applied bool `json:"applied,omitempty"`
}

// TeamPipelinesStatus carries a pipelines tenant's door credentials and
// teardown-purge retry count.
type TeamPipelinesStatus struct {
	// WebhookToken authenticates a webhook delivery against this tenant's
	// door. Minted once, on the tick that first sees spec.pipelines; never
	// regenerated.
	WebhookToken string `json:"webhookToken,omitempty"`
	// WebhookSecret verifies the forge signature on a webhook delivery.
	// Minted once, on the tick that first sees spec.pipelines; never
	// regenerated.
	WebhookSecret string `json:"webhookSecret,omitempty"`
	// PurgeAttempts counts retries of the teardown schedule purge, so a
	// restart does not lose the count. Bounded at ten attempts.
	PurgeAttempts int32 `json:"purgeAttempts,omitempty"`
	// AgentNamespace mirrors spec.pipelines.agentNamespace while the tenant
	// is placed. Teardown reads it to find the tenant ServiceAccount and its
	// token Secret after spec.pipelines is gone, the same way Placement
	// keeps naming a cell after the placement pass stops writing to it.
	AgentNamespace string `json:"agentNamespace,omitempty"`
	// PendingPurgeGeneration names the old placement generation a move
	// still owes a schedule purge for. A move sets this field when it
	// requests the purge and clears it once the purge succeeds. The
	// placement pass retries every tick while it is set, so the retry
	// survives a transient tick that changes the Placed reason for an
	// unrelated cause.
	PendingPurgeGeneration *int64 `json:"pendingPurgeGeneration,omitempty"`
	// PendingPurgeCell names the cell whose schedule rows the pending
	// purge must delete. A slot move raises the tenant generation, so a
	// whole-tenant delete would drop every other slot's rows. The retry
	// therefore needs the cell the move left, not only the generation.
	// An empty value means a whole-tenant delete, which is what teardown
	// asks for.
	PendingPurgeCell string `json:"pendingPurgeCell,omitempty"`
	// PendingPurgeCellNamespace names that cell's namespace. A schedule
	// row is keyed by both, so a cell name alone can name two cells and
	// the delete would drop the rows of a same-named cell in another
	// namespace. It is set and cleared with PendingPurgeCell.
	PendingPurgeCellNamespace string `json:"pendingPurgeCellNamespace,omitempty"`
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
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="TEAM",type="string",JSONPath=".spec.team"
// +kubebuilder:printcolumn:name="LAUNCHER",type="string",JSONPath=".spec.launcher"
// +kubebuilder:printcolumn:name="CELL",type="string",JSONPath=".status.cell"
// +kubebuilder:printcolumn:name="CONNECTED",type="boolean",JSONPath=".status.connected"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"

// Runner is the Schema for the runners API. It binds one external machine to
// one pipelines tenant.
type Runner struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RunnerSpec   `json:"spec,omitempty"`
	Status            RunnerStatus `json:"status,omitempty"`
}

// RunnerSpec binds one external machine to one pipelines tenant.
// +kubebuilder:validation:XValidation:rule="self.team == oldSelf.team",message="spec.team is immutable"
// +kubebuilder:validation:XValidation:rule="self.launcher == oldSelf.launcher",message="spec.launcher is immutable"
// +kubebuilder:validation:XValidation:rule="!self.remoteFS.contains('${') && (!has(self.host) || !self.host.contains('${')) && (!has(self.javaPath) || !self.javaPath.contains('${'))",message="remoteFS, host, and javaPath must not contain a ${ substitution token"
// +kubebuilder:validation:XValidation:rule="self.launcher != 'ssh' || (has(self.host) && has(self.credentialRef))",message="an ssh runner requires host and credentialRef"
// +kubebuilder:validation:XValidation:rule="self.launcher != 'inbound' || !(has(self.host) || has(self.port) || has(self.credentialRef) || has(self.javaPath))",message="an inbound runner must not carry host, port, credentialRef, or javaPath"
// +kubebuilder:validation:XValidation:rule="!has(self.labels) || self.labels.all(l, l.matches(r'^[A-Za-z0-9_.-]+$'))",message="a labels entry must be a Jenkins label token"
type RunnerSpec struct {
	// Team names the pipelines Team that owns this runner. Immutable: the
	// rendered node name and every label derive from it.
	Team string `json:"team"`
	// Launcher selects the connection direction. Immutable: a change would
	// swap the Jenkins launcher class under a connected agent.
	// +kubebuilder:validation:Enum=ssh;inbound
	Launcher string `json:"launcher"`
	// Labels lists unprefixed label suffixes. The operator prefixes each
	// one with <team>-. Bounded so the label-token CEL rule stays under the
	// apiserver's estimated cost budget.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=63
	Labels []string `json:"labels,omitempty"`
	// RemoteFS is the agent work directory on the node.
	RemoteFS string `json:"remoteFS"`
	// NumExecutors is the node's executor count.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	NumExecutors int `json:"numExecutors,omitempty"`
	// Host is the SSH target. Required for launcher ssh. Forbidden for
	// launcher inbound.
	Host string `json:"host,omitempty"`
	// Port is a pointer with no CRD default. A default would make the field
	// present on an inbound runner, which the inbound CEL rule forbids.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port *int `json:"port,omitempty"`
	// CredentialRef names a Secret in the Team's agent namespace. A flat name
	// only: a namespace field would let one tenant read another's Secret.
	CredentialRef string `json:"credentialRef,omitempty"`
	// JavaPath is an explicit java binary path for the ssh launcher. A
	// Windows OpenSSH session often omits the JDK from its default PATH.
	JavaPath string `json:"javaPath,omitempty"`
}

// RunnerStatus reports where a runner's node is rendered and whether it is
// online.
type RunnerStatus struct {
	// NodeName is the rendered Jenkins node name, <team>-<runner name>.
	NodeName string `json:"nodeName,omitempty"`
	// Cell names the Controller that hosts this runner's node. Written from
	// the owning Team's status.placement.
	Cell string `json:"cell,omitempty"`
	// CellNamespace is the namespace of the cell named by Cell.
	CellNamespace string `json:"cellNamespace,omitempty"`
	// Token authenticates an inbound runner at the agent door. Minted once,
	// launcher inbound only.
	Token string `json:"token,omitempty"`
	// Connected reports the last liveness probe of this runner's node. No
	// omitempty: a merge patch must be able to clear a true value back to
	// false.
	Connected bool `json:"connected"`
	// LastSeen is the last time the probe reported this runner online.
	// +optional
	LastSeen *metav1.Time `json:"lastSeen,omitempty"`
	// ObservedGeneration is the metadata.generation the tick that last
	// reached a delivery decision saw.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions reports this runner's condition set. Type Ready.
	// +optional
	Conditions []RunnerCondition `json:"conditions,omitempty"`
}

// RunnerCondition is a status condition for a Runner.
type RunnerCondition struct {
	Type               string                 `json:"type"`
	Status             metav1.ConditionStatus `json:"status"`
	LastTransitionTime metav1.Time            `json:"lastTransitionTime,omitempty"`
	Reason             string                 `json:"reason,omitempty"`
	Message            string                 `json:"message,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// RunnerList contains a list of Runner objects.
type RunnerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Runner `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="PROFILE",type="string",JSONPath=".spec.profileRef"
// +kubebuilder:printcolumn:name="WARM",type="integer",JSONPath=".status.warmCells"
// +kubebuilder:printcolumn:name="PLACED",type="integer",JSONPath=".status.placedCells"
// +kubebuilder:printcolumn:name="FREE",type="integer",JSONPath=".status.freeTenantSlots"
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='PoolReady')].status"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"

// CellPool is the Schema for the cellpools API. It describes one pool of
// cells that serve the pipelines tenants of a single version profile.
type CellPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CellPoolSpec   `json:"spec,omitempty"`
	Status            CellPoolStatus `json:"status,omitempty"`
}

// CellPoolSpec holds the size, the capacity, and the template of one pool.
// +kubebuilder:validation:XValidation:rule="self.profileRef == oldSelf.profileRef",message="spec.profileRef is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.maxCells) || !has(self.warm) || self.maxCells == 0 || self.maxCells >= self.warm",message="spec.maxCells must be 0 or at least spec.warm"
type CellPoolSpec struct {
	// ProfileRef names the JenkinsVersionProfile the pool's cells run.
	// Immutable: the pass never restamps the profile label of a cell, so a
	// change would orphan every cell the pool owns.
	// +kubebuilder:validation:MinLength=1
	ProfileRef string `json:"profileRef"`
	// Warm is the target count of free cells the pool keeps ready.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	Warm int32 `json:"warm"`
	// MaxCells caps the pool's cells, placed cells included. Zero means
	// unlimited.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxCells int32 `json:"maxCells,omitempty"`
	// MaxTenantsPerCell is the capacity a pool cell uses when its own
	// spec.cell.maxTenants is zero.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxTenantsPerCell int32 `json:"maxTenantsPerCell,omitempty"`
	// UnreadyGracePeriod is how long a pool cell may stay unready before
	// the operator rolls its pod. An unset field falls back to the chart
	// value.
	// +optional
	UnreadyGracePeriod *metav1.Duration `json:"unreadyGracePeriod,omitempty"`
	// CellTemplate holds the shape of the cells the pool creates.
	// +optional
	CellTemplate *CellTemplate `json:"cellTemplate,omitempty"`
}

// CellTemplate is the shape a pool stamps on each cell it creates. It
// carries no raw strategic-merge overlay: such content is hard to read and
// hard to diff on a generated cell.
type CellTemplate struct {
	// Resources sets the Jenkins container's resource requirements.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// MiteResources sets the mite sidecar's resource requirements. The pool
	// stamps it on spec.miteSpec.resources and keeps the other miteSpec
	// fields of the cell.
	// +optional
	MiteResources *corev1.ResourceRequirements `json:"miteResources,omitempty"`
	// PodOverrides sets the pod-level overrides of each cell.
	// +optional
	PodOverrides *PodOverrides `json:"podOverrides,omitempty"`
}

// CellPoolStatus reports the pool's live cell and tenant counts. The four
// counters carry no omitempty, so a merge patch writes a count that falls
// to zero.
type CellPoolStatus struct {
	// ObservedGeneration is the pool generation the last pass saw.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// WarmCells counts the owned cells that hold no tenant.
	WarmCells int32 `json:"warmCells"`
	// PlacedCells counts the owned cells that hold at least one tenant.
	PlacedCells int32 `json:"placedCells"`
	// UnreadyCells counts the owned cells whose CellReady condition is not
	// True.
	UnreadyCells int32 `json:"unreadyCells"`
	// FreeTenantSlots sums the spare capacity of the placed cells only.
	// WarmCells already reports the empty ones.
	FreeTenantSlots int32 `json:"freeTenantSlots"`
	// Tenants lists every tenant on the pool, sorted.
	// +optional
	Tenants []string `json:"tenants,omitempty"`
	// Conditions reports this pool's condition set. Type PoolReady.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// CellPoolList contains a list of CellPool objects.
type CellPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CellPool `json:"items"`
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

// TemplateCatalogCondition is a generic status condition for catalogs and composed bundles.
type TemplateCatalogCondition struct {
	Type               string                 `json:"type"`
	Status             metav1.ConditionStatus `json:"status"`
	LastTransitionTime metav1.Time            `json:"lastTransitionTime,omitempty"`
	Reason             string                 `json:"reason,omitempty"`
	Message            string                 `json:"message,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// UpdateCenterCatalogSourceName is the reserved CatalogSource name for the
// update-center-backed variant: the only source allowed to carry neither
// repoURL nor ociRef. The literal is also spelled out in the CEL rule below,
// which cannot reference a Go constant, so the two must be changed together.
const UpdateCenterCatalogSourceName = "varroa-update-center"

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
	RepoURL  string `json:"repoURL,omitempty"`
	OCIRef   string `json:"ociRef,omitempty"`
	Revision string `json:"revision,omitempty"`
	Path     string `json:"path,omitempty"` // catalog root; default "."
	// SyncIntervalSeconds is the catalog poll cadence. Zero means the
	// default (300). The controller clamps anything lower than 30 up to 30,
	// so a stored value under the floor would lie about its own behavior. The
	// ceiling (one year) keeps the seconds-to-Duration conversion in range.
	// +kubebuilder:validation:XValidation:rule="self == 0 || (self >= 30 && self <= 31536000)",message="syncIntervalSeconds must be 0 (default) or between 30 and 31536000"
	SyncIntervalSeconds int    `json:"syncIntervalSeconds,omitempty"`
	SecretRef           string `json:"secretRef,omitempty"` // git creds Secret (same ns)
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
	// CatalogItemPipelineTemplate is a reusable, versioned pipeline template.
	// Content is one PipelineTemplate document (internal/bundle/pipelinetemplate),
	// not items YAML: a template's own schema and jenkinsfile or scmDefinition,
	// not a job definition.
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
	// UpgradePolicy governs whether a profile-driven version roll (a
	// JenkinsVersionProfile.Spec.ResolveVersion advance) may proceed
	// automatically. Empty/"auto" means proceed automatically; "manual" holds
	// the roll until a human intervenes. It does not affect resourceOverlay-
	// governed image changes or the underlying core-compat gate.
	// +kubebuilder:validation:Enum=auto;manual
	// +optional
	UpgradePolicy string `json:"upgradePolicy,omitempty"`
}

// UpgradeIsManual reports whether UpgradePolicy is set to "manual". Empty and
// "auto" both mean automatic.
func (s ProvisioningDefaultsSpec) UpgradeIsManual() bool {
	return s.UpgradePolicy == "manual"
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
	// Upgrade governs the upgrade verb, which releases or bulk-moves targets'
	// Jenkins version.
	// +optional
	Upgrade *BroodUpgradePolicy `json:"upgrade,omitempty"`
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

// BroodUpgradePolicy governs the upgrade brood verb, field-for-field
// identical to ExecuteGroovyPolicy.
type BroodUpgradePolicy struct {
	// Enabled turns the verb on or off cluster-wide. Nil means enabled.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// AllowedNamespaces restricts which namespaces may host an upgrade
	// BroodOperation. It is matched against the BroodOperation's OWN
	// namespace, not its targets'.
	//
	// Empty means every namespace, when Enabled is not false.
	// +optional
	AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`
}

// UpgradeAllowed reports whether an upgrade BroodOperation in opNamespace is
// permitted, and why not when it is not. Mirrors ExecuteGroovyAllowed's logic
// exactly.
//
// A nil receiver, a nil Upgrade, and a nil Enabled all mean allowed.
func (p *BroodPolicy) UpgradeAllowed(opNamespace string) (bool, string) {
	if p == nil || p.Upgrade == nil {
		return true, ""
	}
	up := p.Upgrade
	if up.Enabled != nil && !*up.Enabled {
		return false, "upgrade is disabled by ProvisioningDefaults.broodPolicy"
	}
	if len(up.AllowedNamespaces) == 0 {
		return true, ""
	}
	for _, ns := range up.AllowedNamespaces {
		if ns == opNamespace {
			return true, ""
		}
	}
	return false, fmt.Sprintf(
		"upgrade is not permitted in namespace %s; ProvisioningDefaults.broodPolicy allows %s",
		opNamespace, strings.Join(up.AllowedNamespaces, ", "))
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
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="BUILTIN",type="string",JSONPath=".metadata.labels.varroa\\.dev/builtin"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type VarroaRole struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              VarroaRoleSpec `json:"spec,omitempty"`
	// Status reports each hive where the delete of a stale federated
	// JenkinsRole failed. Only the core BFF federation pass writes it.
	// +optional
	Status VarroaRoleStatus `json:"status,omitempty"`
}

// VarroaRoleStatus reports the federation faults of a core VarroaRole.
type VarroaRoleStatus struct {
	// ObservedGeneration is the spec generation the federation entries describe.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Federation holds one entry for each hive where the delete of a stale
	// federated JenkinsRole failed. A hive with no fault has no entry.
	// +listType=map
	// +listMapKey=cluster
	// +optional
	Federation []BindingFederationStatus `json:"federation,omitempty"`
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
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="ROLE",type="string",JSONPath=".spec.roleRef"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type VarroaRoleBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              VarroaRoleBindingSpec `json:"spec,omitempty"`
	// Status reports the federation outcome of the binding on each hive. Only
	// the core BFF federation pass writes it.
	// +optional
	Status VarroaRoleBindingStatus `json:"status,omitempty"`
}

// VarroaRoleBindingStatus reports the federation outcome of a core binding.
type VarroaRoleBindingStatus struct {
	// ObservedGeneration is the spec generation the federation entries describe.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Federation holds one entry for each hive cluster. The core cluster has no entry.
	// +listType=map
	// +listMapKey=cluster
	// +optional
	Federation []BindingFederationStatus `json:"federation,omitempty"`
}

// BindingFederationState is the federation outcome of a binding on one hive.
// +kubebuilder:validation:Enum=federated;skipped;failed
type BindingFederationState string

// Federation states of a binding on one hive.
const (
	BindingFederated BindingFederationState = "federated"
	BindingSkipped   BindingFederationState = "skipped"
	BindingFailed    BindingFederationState = "failed"
)

// BindingFederationStatus is the federation outcome of a binding on one hive.
type BindingFederationStatus struct {
	// Cluster is the hive cluster name.
	Cluster string `json:"cluster"`
	// State is federated, skipped, or failed.
	State BindingFederationState `json:"state"`
	// Reason is a CamelCase code for the state.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message is the human-readable detail for the state.
	// +optional
	Message string `json:"message,omitempty"`
	// LastTransitionTime is the time the state, reason, or message last changed.
	LastTransitionTime metav1.Time `json:"lastTransitionTime"`
}

// VarroaRoleBindingSpec defines the desired state of VarroaRoleBinding.
type VarroaRoleBindingSpec struct {
	Subjects []SubjectRef `json:"subjects"`
	RoleRef  string       `json:"roleRef"`
	// Scope restricts which namespaces, controllers, or pipelines tenants this
	// binding applies to. A scope names one dimension: tenants, or
	// namespaces/controllerSelector, never both.
	// +kubebuilder:validation:XValidation:rule="!has(self.tenants) || (!has(self.namespaces) && !has(self.controllerSelector))",message="a scope names one dimension"
	// +kubebuilder:validation:XValidation:rule="!has(self.tenants) || size(self.tenants) > 0",message="a scope that names no tenant grants nothing"
	// +kubebuilder:validation:XValidation:rule="!has(self.tenants) || self.tenants.all(t, size(t.name) > 0)",message="a grant that names no tenant grants nothing"
	// +kubebuilder:validation:XValidation:rule="!has(self.tenants) || self.tenants.all(t, !t.name.contains('/'))",message="a tenant name is a Team name"
	// +kubebuilder:validation:XValidation:rule="!has(self.tenants) || self.tenants.all(t, !has(t.jobs) || size(t.jobs) > 0)",message="a grant that matches nothing reads as an oversight"
	// +kubebuilder:validation:XValidation:rule="!has(self.tenants) || self.tenants.all(t, !has(t.jobs) || t.jobs.all(j, size(j) > 0))",message="a grant that matches nothing reads as an oversight"
	// +kubebuilder:validation:XValidation:rule="!has(self.tenants) || self.tenants.all(t, !has(t.jobs) || t.jobs.all(j, !j.startsWith('/')))",message="a pattern is folder relative"
	// +optional
	Scope *VarroaRoleBindingScope `json:"scope,omitempty"`
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
	// Tenants is an optional allow-list of pipelines tenants.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Tenants []TenantScope `json:"tenants,omitempty"`
}

// TenantScope names one pipelines tenant and the jobs of it the binding covers.
type TenantScope struct {
	// Name is a Team name.
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Jobs is an optional list of folder-relative globs. An absent list covers every job.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=512
	// +optional
	Jobs []string `json:"jobs,omitempty"`
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
	// A Jenkins role binding has no tenant meaning, so tenants is rejected here.
	// +kubebuilder:validation:XValidation:rule="!has(self.tenants)",message="a Jenkins role binding has no tenant meaning"
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
	// (see AggregatePluginPrerequisitesNotMetException). Empty for
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

// ProfileCandidateSpec references the profile a newer patch was observed against and
// the resolved plugin closure produced for it.
type ProfileCandidateSpec struct {
	// ProfileRef names the JenkinsVersionProfile this candidate would promote into.
	ProfileRef string `json:"profileRef"`
	// ObservedVersion is the profile's own Spec.ResolveVersion at discovery time —
	// the exact patch promotion would be upgrading FROM (e.g. "2.555.3"), captured
	// for audit/display so a later reader isn't forced to reconstruct "what was this
	// profile pinned to before" from history.
	ObservedVersion string `json:"observedVersion"`
	// ResolveVersion is the newly discovered exact resolvable patch this candidate
	// would promote the profile TO (e.g. "2.555.4") — the same value
	// JenkinsVersionProfile.Spec.ResolveVersion would take on promotion.
	ResolveVersion string `json:"resolveVersion"`
	// ClosureContentRef names the owner-referenced ConfigMap holding the
	// internal/pluginresolve-resolved closure for ResolveVersion, keyed the same way
	// JenkinsVersionProfileStatus.ContentRef is.
	// +optional
	ClosureContentRef string `json:"closureContentRef,omitempty"`
}

// ProfileCandidatePhase is the lifecycle phase of a ProfileCandidate.
// +kubebuilder:validation:Enum=Pending;Ready;Promoted;Failed;Superseded
type ProfileCandidatePhase string

// ProfileCandidatePhase values.
const (
	ProfileCandidatePhasePending    ProfileCandidatePhase = "Pending"
	ProfileCandidatePhaseReady      ProfileCandidatePhase = "Ready"
	ProfileCandidatePhasePromoted   ProfileCandidatePhase = "Promoted"
	ProfileCandidatePhaseFailed     ProfileCandidatePhase = "Failed"
	ProfileCandidatePhaseSuperseded ProfileCandidatePhase = "Superseded"
)

// ProfileCandidateCondition.Type values.
const (
	ConditionCandidateResolved         = "Resolved"
	ConditionCandidateClosureClean     = "ClosureClean"
	ConditionCandidateCoreCompatOK     = "CoreCompatOK"
	ConditionCandidatePluginsServable  = "PluginsServable"
	ConditionCandidatePreflightChecked = "PreflightChecked"
	ConditionCandidatePromoted         = "Promoted"
)

// ProfileCandidateCondition follows the same five-field shape every other CRD's
// per-kind condition type uses in this codebase (ControllerCondition,
// JenkinsVersionProfileCondition, UpdateCenterCondition all carry
// LastTransitionTime; this is not a shortened four-field variant).
type ProfileCandidateCondition struct {
	Type               string                 `json:"type"`
	Status             metav1.ConditionStatus `json:"status"`
	LastTransitionTime metav1.Time            `json:"lastTransitionTime,omitempty"`
	Reason             string                 `json:"reason,omitempty"`
	Message            string                 `json:"message,omitempty"`
}

// ProfileCandidateFailingController is one controller the advisory fleet pre-flight
// found a pin conflict against. Deliberately minimal: a name/namespace + the conflict
// count and a truncated message, never the full internal/bundle.PinPreflightReport —
// keeping etcd size bounded the same way UpdateCenterStatus.Gaps is.
type ProfileCandidateFailingController struct {
	Namespace     string `json:"namespace"`
	Name          string `json:"name"`
	ConflictCount int    `json:"conflictCount"`
	Message       string `json:"message"`
}

// ProfileCandidatePreflightSummary is the advisory pre-flight result. It never
// blocks promotion — it is informational only.
type ProfileCandidatePreflightSummary struct {
	// ControllersChecked is the total count of controllers currently resolving to
	// ProfileRef's line that the pre-flight ran CheckPluginPins against.
	ControllersChecked int `json:"controllersChecked"`
	// ControllersFailing is the TRUE total count of controllers with at least one
	// conflict, even when FailingControllers below is truncated.
	ControllersFailing int `json:"controllersFailing"`
	// FailingControllers lists failing-controllers-only (never a per-controller OK
	// row), capped at maxPreflightFailures.
	// +optional
	FailingControllers []ProfileCandidateFailingController `json:"failingControllers,omitempty"`
}

// ProfileCandidateStatus defines the observed state of a ProfileCandidate.
type ProfileCandidateStatus struct {
	// +optional
	Phase ProfileCandidatePhase `json:"phase,omitempty"`
	// +optional
	Conditions []ProfileCandidateCondition `json:"conditions,omitempty"`
	// +optional
	Preflight *ProfileCandidatePreflightSummary `json:"preflight,omitempty"`
	// PromotedAt is set when Phase transitions to Promoted.
	// +optional
	PromotedAt *metav1.Time `json:"promotedAt,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="PHASE",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="PROFILE",type="string",JSONPath=".spec.profileRef"
// +kubebuilder:printcolumn:name="RESOLVE",type="string",JSONPath=".spec.resolveVersion"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"

// ProfileCandidate is a cluster-scoped, observation-produced candidate for promoting
// a JenkinsVersionProfile onto a newer upstream patch. See
// docs/operations/jenkins-upgrades.md.
type ProfileCandidate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ProfileCandidateSpec   `json:"spec,omitempty"`
	Status            ProfileCandidateStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ProfileCandidateList is a list of ProfileCandidate.
type ProfileCandidateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProfileCandidate `json:"items"`
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
	// SecretRef names a Secret in the operator namespace holding pull
	// credentials for the refs in Refs, which must all live on the same
	// registry — only one registry's credentials are used. Empty means
	// no explicit credentials.
	// +optional
	SecretRef string `json:"secretRef,omitempty"`
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
// +kubebuilder:validation:Enum=restart;reprovision;reconcile;stop;start;executeGroovy;upgrade
type BroodVerb string

// Brood operation verbs.
const (
	BroodVerbRestart       BroodVerb = "restart"
	BroodVerbReprovision   BroodVerb = "reprovision"
	BroodVerbReconcile     BroodVerb = "reconcile"
	BroodVerbStop          BroodVerb = "stop"
	BroodVerbStart         BroodVerb = "start"
	BroodVerbExecuteGroovy BroodVerb = "executeGroovy"
	BroodVerbUpgrade       BroodVerb = "upgrade"
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
// +kubebuilder:validation:XValidation:rule="self.verb == 'upgrade' ? has(self.upgrade) : !has(self.upgrade)",message="upgrade is required iff verb is upgrade"
type BroodAction struct {
	// Verb is the operation to perform.
	// +kubebuilder:validation:Enum=restart;reprovision;reconcile;stop;start;executeGroovy;upgrade
	Verb BroodVerb `json:"verb"`
	// Groovy carries the script for verb=executeGroovy. Required iff Verb ==
	// executeGroovy; must be absent for every other verb.
	// +optional
	Groovy *BroodGroovyAction `json:"groovy,omitempty"`
	// Upgrade carries the target version for verb=upgrade. Required iff Verb ==
	// upgrade; must be absent for every other verb.
	// +optional
	Upgrade *BroodUpgradeAction `json:"upgrade,omitempty"`
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

// BroodUpgradeAction selects the granularity of verb=upgrade. Absent
// TargetVersion releases each target's currently-held promoted revision
// (granularity B); a set TargetVersion bulk-writes it to every target's
// spec.version (granularity A), after admission-time resolution.
type BroodUpgradeAction struct {
	// TargetVersion is the Jenkins version to move targets to. Absent means
	// release the promoted revision each target's version-roll gate is
	// already holding, rather than moving to a new version.
	// +optional
	TargetVersion *string `json:"targetVersion,omitempty"`
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
