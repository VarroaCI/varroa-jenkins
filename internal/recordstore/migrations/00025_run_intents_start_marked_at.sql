-- Migration 00025 adds "start_marked_at" to "varroa.run_intents". This
-- migration adds no table, so the table set stays thirteen.
--
-- "start_marked_at" is the time the owner of a grant recorded that its
-- build can start. The plugin writes this start mark before the queue item
-- leaves the queue, and the item stays queued until the mark answers. A
-- null value therefore proves that the build did not start.
--
-- A marked intent never moves to another instance or to another run. A
-- repeat claim from another instance answers "duplicate", and the re-fire
-- skips the intent.
--
-- The column carries no default and no index. The ledger reads it from a
-- row it has already locked, and the re-fire reads it beside its other
-- run_intents filters.

-- +goose Up

ALTER TABLE varroa.run_intents ADD COLUMN start_marked_at timestamptz;

-- +goose Down

ALTER TABLE varroa.run_intents DROP COLUMN IF EXISTS start_marked_at;
