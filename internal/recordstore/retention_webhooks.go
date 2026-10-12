package recordstore

import (
	"context"
	"fmt"
	"time"
)

// webhookDeliveryRetention is how long a finished webhook delivery row
// stays for inspection before the maintenance round deletes it.
const webhookDeliveryRetention = 7 * 24 * time.Hour

// deleteFinishedWebhookDeliveries deletes the outbox rows that left the
// pending state before cutoff. A pending row is never deleted, because
// the dispatcher still owes it a send.
func deleteFinishedWebhookDeliveries(ctx context.Context, db maintDB, cutoff time.Time) (int64, error) {
	tag, err := db.Exec(ctx, `
		DELETE FROM varroa.webhook_deliveries
		WHERE state <> 'pending' AND updated_at < $1
	`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete finished webhook deliveries: %w", err)
	}
	return tag.RowsAffected(), nil
}
