package bus

import "encoding/json"

// Code constants for bus error responses (§2.1).
const (
	CodeNotFound = "not_found"
	CodeConflict = "conflict"
	CodeInvalid  = "invalid"
	CodeInternal = "internal"
	CodeDraining = "draining"
)

// OperatorReconcileSubject returns the subject for triggering an on-demand
// reconcile of a controller.
// Format: operator.<cluster>.reconcile
// Request body: ReconcileRequest, Response body: ReconcileResponse.
func OperatorReconcileSubject(cluster string) string {
	return "operator." + cluster + ".reconcile"
}

// OperatorNudgeSubject returns the subject for waking a per-controller
// reconcile goroutine for immediate reconciliation. It does not touch
// hibernation: against a parked controller it is a no-op.
// Format: operator.<cluster>.nudge
// Request body: ReconcileRequest, Response body: ReconcileResponse.
func OperatorNudgeSubject(cluster string) string {
	return "operator." + cluster + ".nudge"
}

// OperatorWakeSubject returns the subject for the authenticated wake action,
// which clears a controller's hibernation via the status compare-and-swap
// helper and re-provisions it. Distinct from OperatorNudgeSubject: a nudge
// against a hibernated controller is a no-op, so the two must not share a
// subject.
// Format: operator.<cluster>.wake
// Request body: WakeRequest, Response body: WakeResponse.
func OperatorWakeSubject(cluster string) string {
	return "operator." + cluster + ".wake"
}

// OperatorHibernateSubject returns the subject for the authenticated hibernate
// action, which parks a controller regardless of spec.hibernation.enabled.
// Format: operator.<cluster>.hibernate
// Request body: HibernateRequest, Response body: HibernateResponse.
func OperatorHibernateSubject(cluster string) string {
	return "operator." + cluster + ".hibernate"
}

// OperatorApproveSubject returns the subject for triggering an approve-restart
// on a controller.
// Format: operator.<cluster>.approverestart
// Request body: ApproveRestartRequest, Response body: ApproveRestartResponse.
func OperatorApproveSubject(cluster string) string {
	return "operator." + cluster + ".approverestart"
}

// OperatorReprovisionSubject returns the subject for forcing a desired-state
// re-push to a controller's mite.
// Format: operator.<cluster>.reprovision
// Request body: ReconcileRequest, Response body: ReconcileResponse.
func OperatorReprovisionSubject(cluster string) string {
	return "operator." + cluster + ".reprovision"
}

// OperatorApproveDeletionSubject returns the subject for triggering a targeted
// delete of a deferred item.
// Format: operator.<cluster>.approvedeletion
// Request body: ApproveDeletionRequest, Response body: ApproveDeletionResponse.
func OperatorApproveDeletionSubject(cluster string) string {
	return "operator." + cluster + ".approvedeletion"
}

// OperatorControllersSubject returns operator.<cluster>.controllers.<verb>.
// verb ∈ list|get|create|update|delete|deletepod.
// §2: controller CRUD operations via NATS request-reply, queue operator-workers.
func OperatorControllersSubject(cluster, verb string) string {
	return "operator." + cluster + ".controllers." + verb
}

// OperatorClusterSubject returns operator.<cluster>.cluster.<verb>.
// Verbs: "drain", "draincancel".
func OperatorClusterSubject(cluster, verb string) string {
	return "operator." + cluster + ".cluster." + verb
}

// ClusterDrainRequest is the payload for operator.<cluster>.cluster.drain.
type ClusterDrainRequest struct {
	RequestedBy string `json:"requestedBy"`
}

// ClusterDrainResponse is the reply for operator.<cluster>.cluster.drain.
type ClusterDrainResponse struct {
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}

// ClusterDrainCancelRequest is the payload for operator.<cluster>.cluster.draincancel.
type ClusterDrainCancelRequest struct {
	RequestedBy string `json:"requestedBy"`
}

// ClusterDrainCancelResponse is the reply for operator.<cluster>.cluster.draincancel.
type ClusterDrainCancelResponse struct {
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}

// ReconcileRequest is the payload for operator.<cluster>.reconcile and
// operator.<cluster>.nudge.
type ReconcileRequest struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// ReconcileResponse is the reply for operator.<cluster>.reconcile and
// operator.<cluster>.nudge.
// An empty Error field indicates success.
type ReconcileResponse struct {
	Error string `json:"error,omitempty"`
}

// HibernateRequest is the payload for operator.<cluster>.hibernate.
type HibernateRequest struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// HibernateResponse is the reply for operator.<cluster>.hibernate. Code is
// "conflict" when the action is refused (e.g. the controller is Stopped).
// An empty Error field indicates success.
type HibernateResponse struct {
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}

// WakeRequest is the payload for operator.<cluster>.wake (the authenticated
// hibernation wake action).
type WakeRequest struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// WakeResponse is the reply for operator.<cluster>.wake. Code is "conflict"
// when the action is refused (e.g. the controller is Stopped). An empty Error
// field indicates success.
type WakeResponse struct {
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}

// ApproveRestartRequest is the payload for operator.<cluster>.approverestart.
type ApproveRestartRequest struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Action    string `json:"action"` // "reload" or "restart"
}

// ApproveRestartResponse is the reply for operator.<cluster>.approverestart.
// An empty Error field indicates success.
type ApproveRestartResponse struct {
	Error string `json:"error,omitempty"`
}

// ApproveDeletionRequest is the payload for operator.<cluster>.approvedeletion.
type ApproveDeletionRequest struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Path      string `json:"path"`
}

// ApproveDeletionResponse is the reply for operator.<cluster>.approvedeletion.
// An empty Error field indicates success.
type ApproveDeletionResponse struct {
	Error string `json:"error,omitempty"`
}

// --- Controller CRUD request/response types (§2) ---

// ControllersListRequest is the payload for operator.<cluster>.controllers.list.
// Namespace is an optional narrow filter.
type ControllersListRequest struct {
	Namespace string `json:"namespace,omitempty"`
}

// ControllersListResponse is the reply for operator.<cluster>.controllers.list.
type ControllersListResponse struct {
	Items []json.RawMessage `json:"items,omitempty"`
	Error string            `json:"error,omitempty"`
	Code  string            `json:"code,omitempty"`
}

// ControllersGetRequest is the payload for operator.<cluster>.controllers.get.
type ControllersGetRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// ControllersGetResponse is the reply for operator.<cluster>.controllers.get.
type ControllersGetResponse struct {
	Item  json.RawMessage `json:"item,omitempty"`
	Error string          `json:"error,omitempty"`
	Code  string          `json:"code,omitempty"`
}

// ControllersCreateRequest is the payload for operator.<cluster>.controllers.create.
type ControllersCreateRequest struct {
	Namespace  string          `json:"namespace"`
	Controller json.RawMessage `json:"controller"`       // full CR (apiVersion/kind/metadata/spec)
	Bundle     json.RawMessage `json:"bundle,omitempty"` // optional inline ComposedBundleSpec
	DryRun     bool            `json:"dryRun,omitempty"` // preflight only; nothing persisted
}

// ControllersCreateResponse is the reply for operator.<cluster>.controllers.create.
type ControllersCreateResponse struct {
	Item   json.RawMessage `json:"item,omitempty"`
	Checks []Check         `json:"checks,omitempty"`
	Error  string          `json:"error,omitempty"`
	Code   string          `json:"code,omitempty"`
}

// ControllersUpdateRequest is the payload for operator.<cluster>.controllers.update.
type ControllersUpdateRequest struct {
	Namespace    string          `json:"namespace"`
	Name         string          `json:"name"`
	Patch        json.RawMessage `json:"patch"`                  // {"spec": {...}} partial spec; BFF pre-validated, applied via SSA
	FieldManager string          `json:"fieldManager,omitempty"` // server-side apply field manager
	Force        bool            `json:"force,omitempty"`        // re-assert ownership over conflicting fields
}

// FieldConflict mirrors controller.FieldConflict across the wire.
type FieldConflict struct {
	Field   string `json:"field"`
	Manager string `json:"manager,omitempty"`
	Message string `json:"message"`
}

// ControllersUpdateResponse is the reply for operator.<cluster>.controllers.update.
// Was `type ControllersUpdateResponse = ControllersCreateResponse` (alias) — broken into a
// standalone struct (same fields as ControllersCreateResponse) to add Conflicts without
// affecting controllers.create.
type ControllersUpdateResponse struct {
	Item              json.RawMessage    `json:"item,omitempty"`
	Checks            []Check            `json:"checks,omitempty"`
	Error             string             `json:"error,omitempty"`
	Code              string             `json:"code,omitempty"`
	Conflicts         []FieldConflict    `json:"conflicts,omitempty"`
	UnappliedRemovals []UnappliedRemoval `json:"unappliedRemovals,omitempty"`
}

// UnappliedRemoval names one requested spec removal (an explicit JSON null)
// that did not take effect: the field is still present on the applied object
// because another field manager owns it. Field carries the spec-relative wire
// path, e.g. "spec.composedBundleRef". There is deliberately no owners key —
// ownership does not survive stripManagedFields on the Brood path, so it could
// only be reported on the local route.
type UnappliedRemoval struct {
	Field string `json:"field"`
}

// ControllersDeleteRequest is the payload for operator.<cluster>.controllers.delete.
type ControllersDeleteRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// ControllersDeletePodRequest is the payload for operator.<cluster>.controllers.deletepod.
type ControllersDeletePodRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// ControllersDeleteResponse is the reply for operator.<cluster>.controllers.delete
// and operator.<cluster>.controllers.deletepod.
type ControllersDeleteResponse struct {
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}

// OperatorNamespacesSubject returns operator.<cluster>.namespaces.list —
// deployable-namespace discovery inputs for the target cluster (F4, registry §3.1).
// Request body: NamespacesListRequest, Response body: NamespacesListResponse.
func OperatorNamespacesSubject(cluster string) string {
	return "operator." + cluster + ".namespaces.list"
}

// NamespacesListRequest is the payload for operator.<cluster>.namespaces.list.
// Intentionally empty; reserved for future filters.
type NamespacesListRequest struct{}

// NamespacesListResponse carries the target cluster's raw deployable-namespace
// discovery inputs. Assembly with caller RBAC scopes happens core-side.
type NamespacesListResponse struct {
	// ManagedNamespaces is the operator's MANAGED_NAMESPACES env tokenized on
	// space/comma with empties dropped (nil/empty ⇒ target is cluster-wide mode).
	// Deliberately NOT tenancy.NewManagedSet: no operator-namespace injection,
	// matching the core BFF's parseManagedNamespaces semantics exactly.
	ManagedNamespaces []string `json:"managedNamespaces,omitempty"`
	// CuratedNamespaces is the default-first curated list from the target's
	// varroa-defaults ProvisioningDefaults ([curatedDefault] + Spec.Namespaces
	// minus duplicates of the default).
	CuratedNamespaces []string `json:"curatedNamespaces,omitempty"`
	// CuratedDefault is Spec.DefaultNamespace resolved with the "varroa" fallback.
	CuratedDefault string `json:"curatedDefault,omitempty"`
	Error          string `json:"error,omitempty"`
	Code           string `json:"code,omitempty"`
}

// OperatorBroodOpsSubject returns operator.<cluster>.broodops.<verb>.
// verb ∈ create|get|list|cancel|suspend|preview.
// §3: brood operation commands via NATS request-reply, queue operator-workers.
func OperatorBroodOpsSubject(cluster, verb string) string {
	return "operator." + cluster + ".broodops." + verb
}

// OperatorBroodSchedulesSubject returns operator.<cluster>.broodschedules.<verb>.
// verb ∈ create|get|list|delete|suspend.
func OperatorBroodSchedulesSubject(cluster, verb string) string {
	return "operator." + cluster + ".broodschedules." + verb
}

// --- BroodOps request/response types (§3) ---

// BroodOpsCreateRequest is the payload for operator.<cluster>.broodops.create.
type BroodOpsCreateRequest struct {
	Namespace string          `json:"namespace"`
	Name      string          `json:"name"`
	Spec      json.RawMessage `json:"spec"`
	StartedBy string          `json:"startedBy"`
}

// BroodOpsGetRequest is the payload for operator.<cluster>.broodops.get.
type BroodOpsGetRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// BroodOpsListRequest is the payload for operator.<cluster>.broodops.list.
type BroodOpsListRequest struct {
	Namespace string `json:"namespace,omitempty"`
}

// BroodOpsCancelRequest is the payload for operator.<cluster>.broodops.cancel.
type BroodOpsCancelRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// BroodOpsSuspendRequest is the payload for operator.<cluster>.broodops.suspend.
type BroodOpsSuspendRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Suspend   bool   `json:"suspend"`
}

// BroodOpsPreviewRequest is the payload for operator.<cluster>.broodops.preview.
type BroodOpsPreviewRequest struct {
	Namespace string          `json:"namespace"`
	Spec      json.RawMessage `json:"spec"`
}

// BroodOpsOpResponse is the reply for create/get/suspend.
type BroodOpsOpResponse struct {
	Op    json.RawMessage `json:"op,omitempty"`
	Code  string          `json:"code,omitempty"`
	Error string          `json:"error,omitempty"`
}

// BroodOpsListResponse is the reply for list.
type BroodOpsListResponse struct {
	Ops   []json.RawMessage `json:"ops,omitempty"`
	Code  string            `json:"code,omitempty"`
	Error string            `json:"error,omitempty"`
}

// BroodOpsCancelResponse is the reply for cancel.
type BroodOpsCancelResponse struct {
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// BroodPreviewTarget is a single target entry in a brood ops preview response.
type BroodPreviewTarget struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Wave       int32  `json:"wave"`
	Applicable bool   `json:"applicable"`
	Reason     string `json:"reason,omitempty"`
}

// BroodOpsPreviewResponse is the reply for preview.
type BroodOpsPreviewResponse struct {
	Targets []BroodPreviewTarget `json:"targets,omitempty"`
	Code    string               `json:"code,omitempty"`
	Error   string               `json:"error,omitempty"`
}

// Check mirrors internal/preflight.Check {ID, Status, Message}.
// Duplicated here because internal/preflight imports internal/bus —
// bus must stay import-leaf.
type Check struct {
	ID      string `json:"id"`
	Status  string `json:"status"` // "pass" | "warn" | "fail"
	Message string `json:"message"`
}

// --- BroodSchedules request/response types ---

// BroodScheduleCreateRequest is the payload for operator.<cluster>.broodschedules.create.
type BroodScheduleCreateRequest struct {
	Namespace string          `json:"namespace"`
	Name      string          `json:"name"`
	Spec      json.RawMessage `json:"spec"`
}

// BroodScheduleGetRequest is the payload for operator.<cluster>.broodschedules.get.
type BroodScheduleGetRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// BroodScheduleListRequest is the payload for operator.<cluster>.broodschedules.list.
type BroodScheduleListRequest struct {
	Namespace string `json:"namespace,omitempty"`
}

// BroodScheduleDeleteRequest is the payload for operator.<cluster>.broodschedules.delete.
type BroodScheduleDeleteRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// BroodScheduleSuspendRequest is the payload for operator.<cluster>.broodschedules.suspend.
type BroodScheduleSuspendRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Suspend   bool   `json:"suspend"`
}

// BroodScheduleResponse is the reply for create/get/suspend.
type BroodScheduleResponse struct {
	Namespace string          `json:"namespace,omitempty"`
	Name      string          `json:"name,omitempty"`
	Cluster   string          `json:"cluster,omitempty"`
	Spec      json.RawMessage `json:"spec,omitempty"`
	Status    json.RawMessage `json:"status,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// BroodScheduleListResponse is the reply for list.
type BroodScheduleListResponse struct {
	Items []BroodScheduleResponse `json:"items,omitempty"`
	Error string                  `json:"error,omitempty"`
}
