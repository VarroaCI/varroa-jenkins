package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// runInputSequenceSQL is the guard every run.input arm reads. $11 is the
// payload's own seq, and the stored watermark is floored at zero, so a
// first write whose seq is zero applies. An equal seq applies too, so a
// redelivered envelope stays safe.
const runInputSequenceSQL = `$11::bigint >= COALESCE((runs.payload->>'inputs_seq')::bigint, 0)`

// runInputWritableSQL is the status test every run.input arm reads. A
// completed run holds no pending input, so such a row refuses the write
// whatever the payload's seq holds. A heartbeat-lost mark is the one
// exception: its owner can still wait on an input, and the mark keeps the
// set, so a settled input must still leave it.
const runInputWritableSQL = `(runs.status <> 'COMPLETED' OR ` + heartbeatLostRunSQL + `)`

// applyRunInput replaces the run's whole pending set with the payload's
// set, and inserts a QUEUED placeholder row when the run holds none yet.
// The event carries the run's whole pending set and never a delta, so the
// write merges nothing and keeps no input the run has settled. An empty
// set writes NULL, never an empty array.
//
// It answers refused true when the event's seq sits below the stored
// inputs_seq, and when the row holds a terminal status. The write then
// changes no stored set.
func applyRunInput(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope) (refused bool, err error) {
	var payload RunInputPayload
	if unmarshalErr := json.Unmarshal(env.Payload, &payload); unmarshalErr != nil {
		return false, fmt.Errorf("%w: decode run.input payload: %v", ErrMalformedEnvelope, unmarshalErr)
	}

	// An empty set stores SQL NULL, so no reader distinguishes it from an
	// absent set.
	var stored any
	if len(payload.Inputs) > 0 {
		encoded, marshalErr := json.Marshal(payload.Inputs)
		if marshalErr != nil {
			return false, fmt.Errorf("marshal the run's pending inputs: %w", marshalErr)
		}
		stored = string(encoded)
	}

	var seq int64
	if payload.Seq != nil {
		seq = *payload.Seq
	}

	var storedSeq *int64
	err = tx.QueryRow(ctx, `
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, inputs, payload
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8,
			'QUEUED', $9::jsonb,
			jsonb_build_object('run.input', $10::jsonb, 'inputs_seq', $11::bigint)
		)
		ON CONFLICT (created_at, run_id) DO UPDATE SET
			inputs  = CASE WHEN `+runInputSequenceSQL+` AND `+runInputWritableSQL+`
			               THEN EXCLUDED.inputs ELSE runs.inputs END,
			payload = (COALESCE(runs.payload, '{}'::jsonb) `+acceptedPayloadKeysSQL+`)
			          || CASE WHEN `+runInputSequenceSQL+` AND `+runInputWritableSQL+`
			                  THEN jsonb_build_object('inputs_seq', $11::bigint)
			                  ELSE '{}'::jsonb END
		 WHERE `+runInputWritableSQL+`
		RETURNING (payload->>'inputs_seq')::bigint
	`,
		intent.runID, intent.createdAt, intent.intentKey, intent.cluster, intent.namespace,
		intent.cellValue(), intent.tenant, intent.jobPath,
		stored, string(env.Payload), seq,
	).Scan(&storedSeq)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The row holds a terminal status, so the write applied nothing.
		return true, nil
	case err != nil:
		return false, fmt.Errorf("merge run.input payload: %w", err)
	}

	// A stored value above the payload's own seq means the guard refused
	// the write, and the row still holds its newer set.
	return storedSeq != nil && *storedSeq > seq, nil
}
