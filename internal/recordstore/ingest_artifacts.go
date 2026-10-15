package recordstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// applyRunArtifacts upserts run_artifacts on the natural key
// (run_id, path), copying created_at from the intent.
func applyRunArtifacts(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope) error {
	var payload RunArtifactsPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("%w: decode run.artifacts payload: %v", ErrMalformedEnvelope, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.run_artifacts (run_id, path, created_at, size_bytes, digest, content_type, blob_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (run_id, path) DO UPDATE SET
			size_bytes   = EXCLUDED.size_bytes,
			digest       = EXCLUDED.digest,
			content_type = EXCLUDED.content_type,
			blob_key     = EXCLUDED.blob_key
	`, intent.runID, payload.Path, intent.createdAt, payload.SizeBytes, payload.Digest, payload.ContentType, payload.BlobKey); err != nil {
		return fmt.Errorf("upsert run_artifacts: %w", err)
	}
	return nil
}
