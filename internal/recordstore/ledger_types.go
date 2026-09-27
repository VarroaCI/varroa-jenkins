package recordstore

import "time"

// ClaimRequest is the request body of a varroa.ledger.claim call.
// CellInstance is the Jenkins pod UID of the calling cell. C2 owns the
// matching proto field and uses the same JSON names. Origin is the
// plugin's origin of the queue item, as the mite received it.
type ClaimRequest struct {
	IntentKey              string    `json:"intent_key"`
	Origin                 string    `json:"origin"`
	RunID                  string    `json:"run_id"`
	Cluster                string    `json:"cluster"`
	Namespace              string    `json:"namespace"`
	Cell                   string    `json:"cell"`
	CellInstance           string    `json:"cell_instance"`
	Tenant                 string    `json:"tenant"`
	JobPath                string    `json:"job_path"`
	Branch                 string    `json:"branch"`
	Actor                  string    `json:"actor"`
	Cause                  string    `json:"cause"`
	ParamsDigest           string    `json:"params_digest"`
	PlacementGeneration    int64     `json:"placement_generation"`
	JenkinsNextBuildNumber int64     `json:"jenkins_next_build_number"`
	RequestedAt            time.Time `json:"requested_at"`
}

// ClaimResponse is the reply body of a varroa.ledger.claim call. Error and
// Code carry a server failure, following the bus.WakeResponse precedent.
// Decision is empty whenever Error is set.
type ClaimResponse struct {
	Decision    string `json:"decision,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	BuildNumber int64  `json:"build_number,omitempty"`
	ReasonCode  string `json:"reason_code,omitempty"`
	Message     string `json:"message,omitempty"`
	Error       string `json:"error,omitempty"`
	Code        string `json:"code,omitempty"`
}

// Decision values a ClaimResponse carries.
const (
	// DecisionGranted means the caller may schedule the build.
	DecisionGranted = "granted"
	// DecisionDuplicate means a different run already holds the key.
	DecisionDuplicate = "duplicate"
	// DecisionVetoed means a policy refused the claim.
	DecisionVetoed = "vetoed"
)

// ClaimOriginVarroaIntent is the claim origin of a queue item that a
// Varroa path dispatched: the API trigger, a retry, a replay, or a
// trigger_run ledger action. Only such a run can hold a runs row that
// waits for its claim.
const ClaimOriginVarroaIntent = "VARROA_INTENT"

// Reason codes a ClaimResponse carries in ReasonCode. No code emits
// THROTTLED yet.
const (
	ReasonInvalidKey  = "INVALID_KEY"
	ReasonFenced      = "FENCED"
	ReasonNotReady    = "NOT_READY"
	ReasonCrossTenant = "CROSS_TENANT"
	ReasonThrottled   = "THROTTLED"
	// ReasonNotHome refuses a claim from a standby, or from an active
	// slot that is not the job's home slot. It carries the same retry
	// class as FENCED: it is permanent, because a non-home cell never
	// becomes the home by waiting.
	ReasonNotHome = "NOT_HOME"
)

// ReasonCodes lists every reason code a Fence or the ledger may answer
// with.
var ReasonCodes = []string{
	ReasonInvalidKey,
	ReasonFenced,
	ReasonNotReady,
	ReasonCrossTenant,
	ReasonThrottled,
	ReasonNotHome,
}

// markableVeto reports whether a veto with this reason code is permanent
// for the run record. The set mirrors the plugin's VETOED_OTHER branch:
// the plugin cancels the queue item on every veto but NOT_READY and
// NOT_HOME, so no later event moves the run. NOT_READY retries, and
// NOT_HOME forwards the item to the home slot. Change this set and that
// plugin branch together.
func markableVeto(code string) bool {
	switch code {
	case ReasonFenced, ReasonCrossTenant, ReasonInvalidKey, ReasonThrottled:
		return true
	default:
		return false
	}
}

// Failure codes a ClaimResponse carries in Code, following the
// bus.WakeResponse precedent.
const (
	// CodeUnavailable means Postgres, or a Fence dependency, did not
	// answer.
	CodeUnavailable = "UNAVAILABLE"
	// CodeInternal means a server fault other than an unreachable
	// dependency.
	CodeInternal = "INTERNAL"
	// CodeNotFound means the request names a record the recorder does
	// not hold: an unknown run identifier or an artifact path the run
	// did not record. Only the cross-tenant responder answers it.
	CodeNotFound = "NOT_FOUND"
)
