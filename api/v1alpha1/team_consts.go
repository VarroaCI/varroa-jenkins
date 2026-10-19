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
	TeamConditionNotifications  = "Notifications"
)

// Notifications reasons.
const (
	TeamReasonWebhookReady         = "WebhookReady"
	TeamReasonWebhookSecretMissing = "WebhookSecretMissing"
)

// TeamWebhookDefaultKey is the Secret data key a webhook reads when its
// secretRef names no key.
const TeamWebhookDefaultKey = "hmac-key"

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
	// TeamReasonAgentServiceFailed reports a tenant whose agent Service
	// could not be written this tick. It leaves the placement unchanged.
	TeamReasonAgentServiceFailed = "AgentServiceFailed"
	// TeamReasonSlotPending reports a tenant with at least one active slot
	// that holds no cell. It outranks TeamReasonStandbyPending.
	TeamReasonSlotPending = "SlotPending"
	// TeamReasonStandbyPending reports a tenant whose every active slot
	// holds a cell and whose at least one standby does not.
	TeamReasonStandbyPending = "StandbyPending"
	// TeamReasonMembersApplied reports a tenant whose every member cell
	// lists it as applied.
	TeamReasonMembersApplied = "MembersApplied"
)

// TenantWarnings reasons.
const (
	// TeamReasonItemsHeld reports a tenant whose items fail a cell check.
	// The cell keeps the tenant's folder as it is and pushes the other
	// tenants. The message names the fault.
	TeamReasonItemsHeld = "ItemsHeld"
	// TeamReasonItemsApplyFailed reports a tenant item that Jenkins refused
	// during the cell's items apply. The message names the item path and
	// carries the Jenkins error.
	TeamReasonItemsApplyFailed  = "ItemsApplyFailed"
	TeamReasonBundleKeysIgnored = "BundleKeysIgnored"
	// TeamReasonPodTemplateSkipped reports a tenant bundle pod template
	// that the operator does not lift onto the tenant cloud.
	TeamReasonPodTemplateSkipped = "PodTemplateSkipped"
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
