package v1alpha1

// Tenant condition types.
const (
	TenantConditionReady           = "TenantReady"
	TenantConditionRBACReady       = "TenantRBACReady"
	TenantConditionNamespacesReady = "TenantNamespacesReady"
)

// TenantReady reasons.
const (
	TenantReasonReconciled           = "Reconciled"
	TenantReasonInvalidRoleRef       = "InvalidRoleRef"
	TenantReasonChildApplyFailed     = "ChildApplyFailed"
	TenantReasonNamespaceUnsatisfied = "NamespaceUnsatisfied"
)

// TenantRBACReady reasons.
const (
	TenantReasonMaterialized       = "Materialized"
	TenantReasonGroupApplyFailed   = "GroupApplyFailed"
	TenantReasonBindingApplyFailed = "BindingApplyFailed"
)

// TenantNamespacesReady reasons.
const (
	TenantReasonNamespacesSatisfied   = "NamespacesSatisfied"
	TenantReasonNamespaceMissing      = "NamespaceMissing"
	TenantReasonNamespaceUnmanaged    = "NamespaceUnmanaged"
	TenantReasonNamespaceEnsureFailed = "NamespaceEnsureFailed"
)

// TenantNamespaceState values.
const (
	TenantNamespaceStateManaged   = "Managed"
	TenantNamespaceStateMissing   = "Missing"
	TenantNamespaceStateUnmanaged = "Unmanaged"
	TenantNamespaceStateCreated   = "Created"
)

// Tenant condition types for pipelines tenancy.
const (
	TenantConditionPlaced         = "Placed"
	TenantConditionTenantWarnings = "TenantWarnings"
	TenantConditionNotifications  = "Notifications"
)

// Notifications reasons.
const (
	TenantReasonWebhookReady         = "WebhookReady"
	TenantReasonWebhookSecretMissing = "WebhookSecretMissing"
)

// TenantWebhookDefaultKey is the Secret data key a webhook reads when its
// secretRef names no key.
const TenantWebhookDefaultKey = "hmac-key"

// Placed reasons.
const (
	TenantReasonTenantPlaced            = "TenantPlaced"
	TenantReasonTenantMoved             = "TenantMoved"
	TenantReasonTenantMoving            = "TenantMoving"
	TenantReasonAwaitingCell            = "AwaitingCell"
	TenantReasonProfileNotFound         = "ProfileNotFound"
	TenantReasonCellPoolNotFound        = "CellPoolNotFound"
	TenantReasonCellCapReached          = "CellCapReached"
	TenantReasonAgentNamespaceUnmanaged = "AgentNamespaceUnmanaged"
	TenantReasonRecordStoreUnsupported  = "RecordStoreUnsupported"
	TenantReasonBlobTierUnsupported     = "BlobTierUnsupported"
	TenantReasonCellNotReady            = "CellNotReady"
	TenantReasonTenantTeardownPending   = "TenantTeardownPending"
	// TenantReasonAgentServiceFailed reports a tenant whose agent Service
	// could not be written this tick. It leaves the placement unchanged.
	TenantReasonAgentServiceFailed = "AgentServiceFailed"
	// TenantReasonSlotPending reports a tenant with at least one active slot
	// that holds no cell. It outranks TenantReasonStandbyPending.
	TenantReasonSlotPending = "SlotPending"
	// TenantReasonStandbyPending reports a tenant whose every active slot
	// holds a cell and whose at least one standby does not.
	TenantReasonStandbyPending = "StandbyPending"
	// TenantReasonMembersApplied reports a tenant whose every member cell
	// lists it as applied.
	TenantReasonMembersApplied = "MembersApplied"
)

// TenantWarnings reasons.
const (
	// TenantReasonItemsHeld reports a tenant whose items fail a cell check.
	// The cell keeps the tenant's folder as it is and pushes the other
	// tenants. The message names the fault.
	TenantReasonItemsHeld = "ItemsHeld"
	// TenantReasonItemsApplyFailed reports a tenant item that Jenkins refused
	// during the cell's items apply. The message names each refused item
	// path and carries its Jenkins error.
	TenantReasonItemsApplyFailed  = "ItemsApplyFailed"
	TenantReasonBundleKeysIgnored = "BundleKeysIgnored"
	// TenantReasonPodTemplateSkipped reports a tenant bundle pod template
	// that the operator does not lift onto the tenant cloud.
	TenantReasonPodTemplateSkipped = "PodTemplateSkipped"
	TenantReasonCredentialSkipped  = "CredentialSkipped"
	TenantReasonHibernationIgnored = "HibernationIgnored"
	// TenantReasonBranchScanFailed reports a tenant whose multibranch project
	// or organization folder has a last completed branch scan that did not
	// succeed, while branch jobs of the tenant stay deferred.
	TenantReasonBranchScanFailed = "BranchScanFailed"
)

// Tenant condition type for build-record rehydration.
const (
	TenantConditionRehydrated = "Rehydrated"
)

// Rehydrated reasons.
const (
	TenantReasonTenantRehydrated     = "TenantRehydrated"
	TenantReasonRehydrationPending   = "RehydrationPending"
	TenantReasonRehydrationTruncated = "RehydrationTruncated"
	TenantReasonRehydrationFailed    = "RehydrationFailed"
)

// TenantFinalizerTenantPlacement is held by a pipelines tenant until the
// teardown schedule purge succeeds (design D4, "Teardown when the cell is
// gone").
const TenantFinalizerTenantPlacement = "varroa.dev/tenant-placement"

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

// Tenant-credential Secret label, annotations, and the credential kinds the
// forge path reads. The cell projection and the forge path share these names.
const (
	TenantCredentialLabel          = "varroa.dev/tenant-credential"
	TenantCredentialSelector       = TenantCredentialLabel + "=true"
	CredentialIDAnnotation         = "varroa.dev/credential-id"
	CredentialKindAnnotation       = "varroa.dev/credential-kind"
	CredentialKindUsernamePassword = "usernamePassword"
	CredentialKindSecretText       = "secretText"
)
