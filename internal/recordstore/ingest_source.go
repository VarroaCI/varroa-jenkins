package recordstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// runSourceNewerSQL is the guard every run.source merge arm reads. The
// arriving envelope wins when its source_seq sits at or above the
// stored value. A lower value overwrites nothing, so a redelivery
// stays safe and a late envelope cannot lower a stored description.
const runSourceNewerSQL = `COALESCE((EXCLUDED.payload->>'source_seq')::bigint, 0)
  >= COALESCE((runs.payload->>'source_seq')::bigint, 0)`

// applyRunSource merges a run.source envelope into one runs row, and
// inserts a QUEUED placeholder row when the run holds none yet. The
// merge writes branch and scm_revision only when the event is at or
// above the stored source_seq and carries a value. The source column
// merges key by key, so a second event adds its own keys and drops
// none the first event wrote.
//
// It answers refused true when the event's seq sits below the stored
// source_seq. The write then applies the accepted-key strip alone, and
// no other stored value changes.
func applyRunSource(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope) (refused bool, err error) {
	var payload RunSourcePayload
	if unmarshalErr := json.Unmarshal(env.Payload, &payload); unmarshalErr != nil {
		return false, fmt.Errorf("%w: decode run.source payload: %v", ErrMalformedEnvelope, unmarshalErr)
	}

	// The column holds three keys, and the payload carries five more.
	// Marshalling the stored type alone keeps the two in step.
	stored, err := json.Marshal(StoredRunSource{
		DefinitionPath: payload.DefinitionPath,
		Libraries:      payload.Libraries,
		PodTemplates:   payload.PodTemplates,
	})
	if err != nil {
		return false, fmt.Errorf("marshal stored run source: %w", err)
	}

	var storedSeq int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, branch, scm_revision, source, payload
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8,
			'QUEUED', $9, $10, $11::jsonb,
			jsonb_build_object('run.source', $12::jsonb, 'source_seq', $13::bigint)
		)
		ON CONFLICT (created_at, run_id) DO UPDATE SET
			branch       = CASE WHEN `+runSourceNewerSQL+` AND EXCLUDED.branch IS NOT NULL
			                    THEN EXCLUDED.branch ELSE runs.branch END,
			scm_revision = CASE WHEN `+runSourceNewerSQL+` AND EXCLUDED.scm_revision IS NOT NULL
			                    THEN EXCLUDED.scm_revision ELSE runs.scm_revision END,
			source       = CASE WHEN `+runSourceNewerSQL+`
			                    THEN COALESCE(runs.source, '{}'::jsonb) || EXCLUDED.source
			                    ELSE runs.source END,
			payload      = (COALESCE(runs.payload, '{}'::jsonb) `+acceptedPayloadKeysSQL+`)
			               || CASE WHEN `+runSourceNewerSQL+` THEN EXCLUDED.payload ELSE '{}'::jsonb END
		RETURNING (payload->>'source_seq')::bigint
	`,
		intent.runID, intent.createdAt, intent.intentKey, intent.cluster, intent.namespace,
		intent.cellValue(), intent.tenant, intent.jobPath,
		payload.Branch, payload.SCMRevision, string(stored), string(env.Payload), payload.Seq,
	).Scan(&storedSeq); err != nil {
		return false, fmt.Errorf("merge run.source payload: %w", err)
	}

	// A stored value above the event's own seq means the guard refused
	// the write, and the row still holds its newer description.
	return storedSeq > payload.Seq, nil
}
