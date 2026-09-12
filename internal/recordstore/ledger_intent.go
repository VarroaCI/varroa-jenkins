package recordstore

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// consumeIntent moves a GRANTED intent to CONSUMED for a run.started
// event. It writes the build number Jenkins assigned and the consumed
// timestamp.
func consumeIntent(ctx context.Context, db DBTX, runID uuid.UUID, buildNumber int64, consumedAt time.Time) error {
	if _, err := db.Exec(ctx, `
		UPDATE varroa.run_intents
		SET state = $1, consumed_at = $2, build_number = $3
		WHERE run_id = $4
	`, stateConsumed, consumedAt, buildNumber, runID); err != nil {
		return fmt.Errorf("consume intent: %w", err)
	}
	return nil
}
