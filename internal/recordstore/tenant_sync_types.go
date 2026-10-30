package recordstore

import (
	"encoding/json"
	"time"
)

// Tenant-sync request kinds. bus.TenantSyncSubject carries both: the
// operator announces a tenant's placement, or asks for a tenant's build
// records.
const (
	TenantSyncKindAnnounce = "announce"
	TenantSyncKindRecords  = "records"
	// TenantSyncKindHomes asks for a tenant's member list and its job
	// home map. The recorder is the only owner of that map.
	TenantSyncKindHomes = "homes"
)

// TenantSyncMember is one member of a pipelines tenant: one cell that
// holds the tenant's folder. The announce request carries the whole list,
// and the recorder stores it in tenant_placements.members.
//
// A cell holds at most one member of a tenant, so the cell and namespace
// pair is the member's key. An active member carries a slot index. A
// standby carries no slot and is never a job's home.
type TenantSyncMember struct {
	Cell      string `json:"cell"`
	Namespace string `json:"namespace"`
	// Role is MemberRoleActive or MemberRoleStandby.
	Role string `json:"role"`
	// Slot is set only on an active member.
	Slot *int32 `json:"slot,omitempty"`
	// Instance is the cell pod's unique identifier, empty when no pod
	// runs.
	Instance string `json:"instance,omitempty"`
	// RehydratedInstance is the instance whose rehydration seed applied
	// for this member.
	RehydratedInstance string `json:"rehydrated_instance,omitempty"`
	// PendingJobs holds the job paths this member has not seeded yet.
	PendingJobs []string `json:"pending_jobs,omitempty"`
	// AppliedAt is when this member's cell last reported the tenant
	// applied.
	AppliedAt *time.Time `json:"applied_at,omitempty"`
}

// TenantSyncDeadInstance is one gone cell instance. A TenantSyncRequest
// of kind "announce" carries the operator's entries, and the recorder
// merges them into the stored list, because an operator restart loses
// every deleted pod identity. The recorder also stores entries it derives
// from a change of its own member list.
type TenantSyncDeadInstance struct {
	Instance   string    `json:"instance"`
	LastSeenAt time.Time `json:"last_seen_at"`
	// ConfirmedGoneAt is the time the operator's own pod read answered
	// "no pod" for this instance. An entry the recorder stored before
	// this field existed decodes with a nil value, and such an entry
	// keeps the five-minute moved window.
	ConfirmedGoneAt *time.Time `json:"confirmed_gone_at,omitempty"`
	// Derived is DeadInstanceDerivedRolled or DeadInstanceDerivedDropped
	// on an entry the recorder derived, and empty on an operator entry.
	// The fleet-lock release applies the moved window to a rolled entry
	// only. Every other entry needs a confirmation, because the cell of
	// such an instance can still be alive.
	Derived string `json:"derived,omitempty"`
	// Cell and Namespace name the cell that ran the instance. The
	// operator reads that cell's namespace again for an entry with no
	// confirmation, until no pod with the instance's identifier runs
	// there. An entry with no cell is never read again.
	Cell      string `json:"cell,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// TenantSyncRequest is the request body of a
// varroa.recordstore.tenant.sync call. Kind selects between the two
// field groups below; a field a kind does not use stays at its zero
// value and is omitted from the JSON.
type TenantSyncRequest struct {
	Kind    string `json:"kind"`
	Cluster string `json:"cluster"`
	Tenant  string `json:"tenant"`

	// Namespace, Cell, Generation, CellInstance, Policy,
	// RehydratedInstance, PendingJobs, CellAlive, CellAliveAt,
	// CellLastSeenAt, and DeadInstances apply to kind "announce".
	Namespace          string                   `json:"namespace,omitempty"`
	Cell               string                   `json:"cell,omitempty"`
	Generation         int64                    `json:"generation,omitempty"`
	CellInstance       string                   `json:"cell_instance,omitempty"`
	Policy             json.RawMessage          `json:"policy,omitempty"`
	RehydratedInstance string                   `json:"rehydrated_instance,omitempty"`
	PendingJobs        []string                 `json:"pending_jobs,omitempty"`
	CellAlive          bool                     `json:"cell_alive,omitempty"`
	CellAliveAt        *time.Time               `json:"cell_alive_at,omitempty"`
	CellLastSeenAt     *time.Time               `json:"cell_last_seen_at,omitempty"`
	DeadInstances      []TenantSyncDeadInstance `json:"dead_instances,omitempty"`
	// Members carries the whole member list of the tenant, so the
	// recorder never observes a half-applied change. Slot 0's entry
	// repeats the scalar fields above. An empty list is stored as an
	// empty list, which every reader resolves to one synthetic slot-0
	// member.
	Members []TenantSyncMember `json:"members,omitempty"`

	// Incarnation applies to kind "announce". It is the identity of the
	// tenant object this announcement describes: the object's
	// metadata.uid. A tenant created again under one name carries a new
	// value, and that change hands the row to the new tenant. An empty
	// value means the caller names no incarnation.
	Incarnation string `json:"incarnation,omitempty"`

	// AgentNamespace applies to kind "announce". It is the namespace of
	// the tenant's build agents. The recorder stores the announced value
	// as it is, so an empty value clears the stored one.
	AgentNamespace string `json:"agent_namespace,omitempty"`

	// JobPaths and PerJob apply to kind "records". An empty JobPaths
	// means enumerate.
	JobPaths []string `json:"job_paths,omitempty"`
	PerJob   int      `json:"per_job,omitempty"`

	// NotificationWebhook applies to kind "announce". It carries the
	// tenant's completion webhook: the receiver URL and the HMAC signing
	// key. Nil clears the stored webhook. The recorder never logs it.
	NotificationWebhook *TenantSyncWebhook `json:"notification_webhook,omitempty"`

	// BadgeToken applies to kind "announce". It is set only while the
	// tenant's badge switch is on. An empty value clears the stored token.
	// The recorder never logs it.
	BadgeToken string `json:"badge_token,omitempty"`
}

// TenantSyncWebhook is a tenant's completion webhook as the operator
// announces it. Key is the HMAC signing key, read from the tenant's
// Secret.
type TenantSyncWebhook struct {
	URL string `json:"url"`
	Key string `json:"key"`
}

// TenantSyncRecord is one thin build record a "records" reply carries
// for one job path.
type TenantSyncRecord struct {
	BuildNumber          int64           `json:"build_number"`
	QueuedAt             *time.Time      `json:"queued_at,omitempty"`
	StartedAt            *time.Time      `json:"started_at,omitempty"`
	EndedAt              *time.Time      `json:"ended_at,omitempty"`
	DurationMs           *int64          `json:"duration_ms,omitempty"`
	Result               string          `json:"result,omitempty"`
	CauseDescription     string          `json:"cause,omitempty"`
	Params               json.RawMessage `json:"params,omitempty"`
	MaskedParameterNames []string        `json:"masked_parameter_names,omitempty"`
	Changes              json.RawMessage `json:"changes,omitempty"`
}

// TenantSyncJobRecords is one job path's answer inside a "records"
// reply: its next build number and the records selected for it.
type TenantSyncJobRecords struct {
	JobPath         string             `json:"job_path"`
	NextBuildNumber int64              `json:"next_build_number"`
	Records         []TenantSyncRecord `json:"records"`
}

// TenantSyncResponse is the reply body of a
// varroa.recordstore.tenant.sync call, following the bus.WakeResponse
// precedent. Error is set only for an unknown kind or a server fault;
// Ok, InterruptedMarked, and Jobs are empty whenever Error is set.
type TenantSyncResponse struct {
	// Ok and InterruptedMarked apply to kind "announce".
	Ok                bool `json:"ok,omitempty"`
	InterruptedMarked int  `json:"interrupted_marked,omitempty"`
	// Stale applies to kind "announce". It is true when the recorder
	// refused the request because it describes an older state than the
	// stored row. Ok is then false.
	Stale bool `json:"stale,omitempty"`
	// Unconfirmed applies to kind "announce". It lists every stored
	// dead_instances entry that names a cell and carries no
	// confirmed_gone_at. The operator reads the pod of each one and
	// sends the confirmation once the pod is gone. The stored row is the
	// durable list, so an operator restart loses nothing.
	Unconfirmed []TenantSyncDeadInstance `json:"unconfirmed_dead_instances,omitempty"`

	// Jobs applies to kind "records".
	Jobs []TenantSyncJobRecords `json:"jobs,omitempty"`

	// Members and JobHomes apply to kind "homes". Members holds the
	// tenant's stored member list, and JobHomes maps a job path to a slot
	// index. A tenant with no stored list answers one synthetic slot-0
	// member built from the row's scalar columns.
	Members  []TenantSyncMember `json:"members,omitempty"`
	JobHomes map[string]int32   `json:"job_homes,omitempty"`

	Error string `json:"error,omitempty"`
}
