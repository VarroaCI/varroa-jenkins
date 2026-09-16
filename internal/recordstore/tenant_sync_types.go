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
)

// TenantSyncDeadInstance is one confirmed-gone cell instance a
// TenantSyncRequest of kind "announce" carries. The operator lists an
// instance here only when it confirms that instance is gone; the
// recorder merges the list into the stored one, because an operator
// restart loses every deleted pod identity.
type TenantSyncDeadInstance struct {
	Instance   string    `json:"instance"`
	LastSeenAt time.Time `json:"last_seen_at"`
	// ConfirmedGoneAt is the time the operator's own pod read answered
	// "no pod" for this instance. An entry the recorder stored before
	// this field existed decodes with a nil value, and such an entry
	// keeps the five-minute moved window.
	ConfirmedGoneAt *time.Time `json:"confirmed_gone_at,omitempty"`
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

	// JobPaths and PerJob apply to kind "records". An empty JobPaths
	// means enumerate.
	JobPaths []string `json:"job_paths,omitempty"`
	PerJob   int      `json:"per_job,omitempty"`
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

	// Jobs applies to kind "records".
	Jobs []TenantSyncJobRecords `json:"jobs,omitempty"`

	Error string `json:"error,omitempty"`
}
