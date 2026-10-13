-- Migration 00034 changes the completion webhook outbox. It adds the
-- claiming incarnation to "run_intents" and a delivery key to
-- "webhook_deliveries". The table set does not change.
--
-- "run_intents.incarnation" holds the tenant incarnation that owned the
-- placement when the claim granted the run. The recorder stamps a
-- webhook delivery from this value, not from the current placement. A
-- run that a replaced Team left behind therefore never reaches the
-- replacement's receiver.
--
-- "webhook_deliveries.delivery_key" tells the rows of one run apart. The
-- owner completion uses the key "completed". A synthetic end uses the key
-- "interrupted:<reason>". A run can therefore hold one row for each
-- distinct synthetic reason and one completion row. The unique key moves
-- from run_id to (run_id, delivery_key).

-- +goose Up

ALTER TABLE varroa.run_intents ADD COLUMN incarnation text NOT NULL DEFAULT '';

ALTER TABLE varroa.webhook_deliveries ADD COLUMN delivery_key text NOT NULL DEFAULT 'completed';
ALTER TABLE varroa.webhook_deliveries DROP CONSTRAINT webhook_deliveries_run_id_key;
ALTER TABLE varroa.webhook_deliveries ADD CONSTRAINT webhook_deliveries_run_key_key UNIQUE (run_id, delivery_key);

-- +goose Down

DELETE FROM varroa.webhook_deliveries WHERE delivery_key <> 'completed';
ALTER TABLE varroa.webhook_deliveries DROP CONSTRAINT IF EXISTS webhook_deliveries_run_key_key;
ALTER TABLE varroa.webhook_deliveries ADD CONSTRAINT webhook_deliveries_run_id_key UNIQUE (run_id);
ALTER TABLE varroa.webhook_deliveries DROP COLUMN IF EXISTS delivery_key;
ALTER TABLE varroa.run_intents DROP COLUMN IF EXISTS incarnation;
