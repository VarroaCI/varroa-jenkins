-- Migration 00023 adds "granted_at" to "varroa.run_intents". This
-- migration adds no table, so the table set stays thirteen.
--
-- "granted_at" is the time the ledger last granted the intent to the
-- instance that "cell_instance" names. The ledger writes it on an insert,
-- on a transfer, and on an ownership move. A same-instance repeat does
-- not change it.
--
-- A repeat claim from another instance waits while that grant can still
-- be building. The age of the grant is one of the two tests that end the
-- wait. A null value counts as an old grant, so a row written before this
-- migration never holds a claim.
--
-- The column carries no default and no index. The ledger reads it only
-- from a row it has already locked.

-- +goose Up

ALTER TABLE varroa.run_intents ADD COLUMN granted_at timestamptz;

-- +goose Down

ALTER TABLE varroa.run_intents DROP COLUMN IF EXISTS granted_at;
