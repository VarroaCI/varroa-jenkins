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

// PoolReady reasons. The condition carries exactly these three.
const (
	CellPoolReasonReady          = "Ready"
	CellPoolReasonCellCapReached = "CellCapReached"
	CellPoolReasonProfileMissing = "ProfileMissing"
)

// CellPoolFinalizerDrain holds a deleted pool until its free cells are gone.
const CellPoolFinalizerDrain = "varroa.dev/cellpool-drain"
