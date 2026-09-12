package recordstore

import (
	"encoding/json"
	"time"
)

// The eight allocated CrossTenantRequest.Kind values (design.md D4). The
// gateway relay already rejects any other value before a request ever
// reaches this responder; the dispatch here still rejects one on its
// own, because this subject has no other guard.
const (
	CrossTenantKindBuild          = "build"
	CrossTenantKindStatus         = "status"
	CrossTenantKindLastSuccessful = "last_successful"
	CrossTenantKindArtifactList   = "artifact_list"
	CrossTenantKindArtifactURL    = "artifact_url"
	CrossTenantKindLockAcquire    = "lock_acquire"
	CrossTenantKindLockRenew      = "lock_renew"
	CrossTenantKindLockRelease    = "lock_release"
)

// The five allocated cross-tenant run states (design.md D1, D8). A
// status or a build reply never answers "pending" once a run_intents
// row exists for the key; "pending" names the absence of that row.
const (
	CrossTenantStatePending     = "pending"
	CrossTenantStateQueued      = "queued"
	CrossTenantStateRunning     = "running"
	CrossTenantStateCompleted   = "completed"
	CrossTenantStateInterrupted = "interrupted"
)

// The fleet-lock reply states (design.md D11). "released" answers a
// lock_release call; it never appears as a stored fleet_locks.state
// value, because a released lock is a deleted row.
const (
	FleetLockStateHeld      = "held"
	FleetLockStateWaiting   = "waiting"
	FleetLockStateReleasing = "releasing"
	FleetLockStateLost      = "lost"
	FleetLockStateReleased  = "released"
)

// CrossTenantRequest is the request body of a varroa.ledger.crosstenant
// call. It is the union of every kind's fields (design.md D4): a field a
// kind does not use stays at its zero value and is never read. The
// gateway overwrites Cluster, Namespace, Cell, CallerTenant, and
// PlacementGeneration before it sends the request, whatever the calling
// cell supplied.
type CrossTenantRequest struct {
	Cluster             string    `json:"cluster"`
	Namespace           string    `json:"namespace"`
	Cell                string    `json:"cell"`
	CallerTenant        string    `json:"caller_tenant"`
	PlacementGeneration int64     `json:"placement_generation"`
	Kind                string    `json:"kind"`
	RequestedAt         time.Time `json:"requested_at"`

	// build
	CallerRunID  string          `json:"caller_run_id,omitempty"`
	Seq          string          `json:"seq,omitempty"`
	TargetTenant string          `json:"target_tenant,omitempty"`
	JobPath      string          `json:"job_path,omitempty"`
	Params       json.RawMessage `json:"params,omitempty"`

	// status
	RunID     string `json:"run_id,omitempty"`
	IntentKey string `json:"intent_key,omitempty"`

	// artifact_url
	Path string `json:"path,omitempty"`

	// lock_acquire, lock_renew, lock_release
	Name         string     `json:"name,omitempty"`
	LeaseSeconds int64      `json:"lease_seconds,omitempty"`
	WaitingSince *time.Time `json:"waiting_since,omitempty"`
}

// CrossTenantResponse is the reply body of a varroa.ledger.crosstenant
// call. It is the union of every kind's reply fields (design.md D4).
// Error and Code follow C1's rule: UNAVAILABLE when Postgres does not
// answer, INTERNAL for any other fault, and no new code. ReasonCode and
// Message carry a GrantEvaluator veto, which is never a server fault.
type CrossTenantResponse struct {
	Kind       string `json:"kind,omitempty"`
	ReasonCode string `json:"reason_code,omitempty"`
	Message    string `json:"message,omitempty"`
	Error      string `json:"error,omitempty"`
	Code       string `json:"code,omitempty"`

	// build, status, last_successful
	RunID       string  `json:"run_id,omitempty"`
	State       string  `json:"state,omitempty"`
	BuildNumber *int64  `json:"build_number,omitempty"`
	Result      *string `json:"result,omitempty"`
	JobPath     string  `json:"job_path,omitempty"`
	URL         string  `json:"url,omitempty"`

	// last_successful
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`

	// artifact_list
	Artifacts []CrossTenantArtifact `json:"artifacts,omitempty"`

	// artifact_url
	ExpiresAt *time.Time `json:"expires_at,omitempty"`

	// lock_acquire, lock_renew
	Holder     string `json:"holder,omitempty"`
	Position   int    `json:"position,omitempty"`
	FenceToken int64  `json:"fence_token,omitempty"`
}

// CrossTenantArtifact is one entry of an artifact_list reply. It never
// carries the stored blob_key: only artifact_url signs that key, and
// only for the one path a caller names.
type CrossTenantArtifact struct {
	Path        string `json:"path"`
	SizeBytes   int64  `json:"size_bytes"`
	Digest      string `json:"digest,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

// qualifyCrossTenantJobPath turns a build or a last_successful request's
// job_path into the qualified form the stored run_intents.job_path and
// runs.job_path columns hold. design.md D10 fixes the wire form as
// folder relative to targetTenant, matching the documented step call
// job: 'release'. A handler qualifies once, at this boundary, before it
// runs the grant check or reads a run or an intent by job path.
func qualifyCrossTenantJobPath(targetTenant, jobPath string) string {
	return targetTenant + "/" + jobPath
}
