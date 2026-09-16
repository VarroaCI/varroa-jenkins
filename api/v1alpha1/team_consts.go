package v1alpha1

// Team condition types.
const (
	TeamConditionReady           = "TeamReady"
	TeamConditionRBACReady       = "TeamRBACReady"
	TeamConditionNamespacesReady = "TeamNamespacesReady"
)

// TeamReady reasons.
const (
	TeamReasonReconciled           = "Reconciled"
	TeamReasonInvalidRoleRef       = "InvalidRoleRef"
	TeamReasonChildApplyFailed     = "ChildApplyFailed"
	TeamReasonNamespaceUnsatisfied = "NamespaceUnsatisfied"
)

// TeamRBACReady reasons.
const (
	TeamReasonMaterialized       = "Materialized"
	TeamReasonGroupApplyFailed   = "GroupApplyFailed"
	TeamReasonBindingApplyFailed = "BindingApplyFailed"
)

// TeamNamespacesReady reasons.
const (
	TeamReasonNamespacesSatisfied   = "NamespacesSatisfied"
	TeamReasonNamespaceMissing      = "NamespaceMissing"
	TeamReasonNamespaceUnmanaged    = "NamespaceUnmanaged"
	TeamReasonNamespaceEnsureFailed = "NamespaceEnsureFailed"
)

// TeamNamespaceState values.
const (
	TeamNamespaceStateManaged   = "Managed"
	TeamNamespaceStateMissing   = "Missing"
	TeamNamespaceStateUnmanaged = "Unmanaged"
	TeamNamespaceStateCreated   = "Created"
)

// Team condition types for pipelines tenancy.
const (
	TeamConditionPlaced         = "Placed"
	TeamConditionTenantWarnings = "TenantWarnings"
)

// Placed reasons.
const (
	TeamReasonTenantPlaced            = "TenantPlaced"
	TeamReasonTenantMoved             = "TenantMoved"
	TeamReasonTenantMoving            = "TenantMoving"
	TeamReasonAwaitingCell            = "AwaitingCell"
	TeamReasonProfileNotFound         = "ProfileNotFound"
	TeamReasonCellPoolNotFound        = "CellPoolNotFound"
	TeamReasonCellCapReached          = "CellCapReached"
	TeamReasonAgentNamespaceUnmanaged = "AgentNamespaceUnmanaged"
	TeamReasonRecordStoreUnsupported  = "RecordStoreUnsupported"
	TeamReasonBlobTierUnsupported     = "BlobTierUnsupported"
	TeamReasonCellNotReady            = "CellNotReady"
	TeamReasonTenantTeardownPending   = "TenantTeardownPending"
	// TeamReasonAgentServiceFailed reports a tenant whose slot 0 agent
	// Service could not be written this tick. It leaves the placement
	// unchanged.
	TeamReasonAgentServiceFailed = "AgentServiceFailed"
)

// TenantWarnings reasons.
const (
	TeamReasonBundleKeysIgnored  = "BundleKeysIgnored"
	TeamReasonCredentialSkipped  = "CredentialSkipped"
	TeamReasonHibernationIgnored = "HibernationIgnored"
)

// Team condition type for build-record rehydration.
const (
	TeamConditionRehydrated = "Rehydrated"
)

// Rehydrated reasons.
const (
	TeamReasonTenantRehydrated     = "TenantRehydrated"
	TeamReasonRehydrationPending   = "RehydrationPending"
	TeamReasonRehydrationTruncated = "RehydrationTruncated"
	TeamReasonRehydrationFailed    = "RehydrationFailed"
)

// TeamFinalizerTenantPlacement is held by a pipelines tenant until the
// teardown schedule purge succeeds (design D4, "Teardown when the cell is
// gone").
const TeamFinalizerTenantPlacement = "varroa.dev/tenant-placement"

// CellPool condition type.
const (
	CellPoolConditionPoolReady = "PoolReady"
)

// PoolReady reasons. RecordStoreUnsupported and BlobTierUnsupported reuse
// the Controller CellReady reason strings; ArtifactSecretMissing and
// ArtifactPluginMissing are pool-only.
const (
	CellPoolReasonReady                  = "Ready"
	CellPoolReasonCellCapReached         = "CellCapReached"
	CellPoolReasonProfileMissing         = "ProfileMissing"
	CellPoolReasonRecordStoreUnsupported = "RecordStoreUnsupported"
	CellPoolReasonBlobTierUnsupported    = "BlobTierUnsupported"
	CellPoolReasonArtifactSecretMissing  = "ArtifactSecretMissing"
	CellPoolReasonArtifactPluginMissing  = "ArtifactPluginMissing"
	// CellPoolReasonSecretsMissing reports a pool with no usable
	// confidential store. It applies whether or not the pass wanted a fill,
	// because the store is a property of the pool.
	CellPoolReasonSecretsMissing = "SecretsMissing"
)

// CellPoolFinalizerDrain holds a deleted pool until its free cells are gone.
const CellPoolFinalizerDrain = "varroa.dev/cellpool-drain"
