package recordstore

import (
	"encoding/json"
	"fmt"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// Ledger-action kinds. The recorder publishes cancel_run today. C6 adds
// the operator subscriber and the retry_run and wake_cell producers.
// trigger_run has two producers: the cross-tenant responder's build kind
// (a step call) and the upstream fan-out.
const (
	LedgerActionCancelRun  = "cancel_run"
	LedgerActionRetryRun   = "retry_run"
	LedgerActionWakeCell   = "wake_cell"
	LedgerActionTriggerRun = "trigger_run"
)

// LedgerActionRecorder is the literal actor value the recorder sets when
// it publishes an action on its own decision, with no requesting
// principal behind it.
const LedgerActionRecorder = "varroa-recorder"

// LedgerAction is the envelope the recorder publishes on
// bus.LedgerActionSubject. Kind, Cluster, Namespace, Controller, and
// Actor are common to every kind. The extra fields below apply only to
// the kind that names them; a field an action's kind does not use stays
// at its zero value and is omitted from the JSON.
type LedgerAction struct {
	Kind       string `json:"kind"`
	Cluster    string `json:"cluster"`
	Namespace  string `json:"namespace"`
	Controller string `json:"controller"`
	Actor      string `json:"actor"`

	// RunID, JobPath, and BuildNumber apply to cancel_run.
	RunID       string `json:"run_id,omitempty"`
	JobPath     string `json:"job_path,omitempty"`
	BuildNumber *int64 `json:"build_number,omitempty"`

	// IntentKey and Parameters apply to retry_run and trigger_run,
	// alongside RunID and JobPath above.
	IntentKey  string          `json:"intent_key,omitempty"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

// PublishLedgerAction marshals action and publishes it on
// bus.LedgerActionSubject(action.Cluster) as core NATS, at most once. A
// publish with no operator leader subscribing is lost; the recorder never
// republishes it.
func PublishLedgerAction(conn *bus.Conn, action LedgerAction) error {
	data, err := json.Marshal(action)
	if err != nil {
		return fmt.Errorf("marshal ledger action: %w", err)
	}
	if err := conn.Publish(bus.LedgerActionSubject(action.Cluster), data); err != nil {
		return fmt.Errorf("publish ledger action: %w", err)
	}
	return nil
}
