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
