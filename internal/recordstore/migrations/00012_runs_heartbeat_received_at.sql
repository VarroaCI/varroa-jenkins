-- Migration 00012 adds "heartbeat_received_at" to "varroa.runs". This
-- migration adds no table, so the table set stays twelve.
--
-- "last_heartbeat_at" holds the heartbeat envelope's "occurred_at", which a
-- Jenkins pod stamps. Every liveness window compares that value with a
-- recorder timestamp, so the answer carries the skew between two machines.
-- "heartbeat_received_at" holds the moment the recorder applied the same
-- envelope. Every liveness comparison reads it, and "last_heartbeat_at"
-- serves the reader alone.
--
-- The column sits on the partitioned parent "varroa.runs", so every
-- partition inherits it. It carries no index: every reader resolves it from
-- a row it has already found.
--
-- The backfill is mandatory. A live run whose new column stays null loses
-- its liveness anchor. The recorder then measures that run from
-- "accepted_at" or "created_at", and the first sweep after the upgrade marks
-- the whole fleet interrupted. The copy writes a pod clock into a
-- recorder-clock column for one generation of rows, which is bounded by the
-- skew that already applies. A null is bounded by nothing.
--
-- The backfill covers QUEUED and RUNNING rows alone. Those are the rows a
-- liveness window reads. A terminal row needs none: the finalize anchor
-- prefers "ended_at", which a completed run always holds.

-- +goose Up

ALTER TABLE varroa.runs ADD COLUMN heartbeat_received_at timestamptz;

UPDATE varroa.runs
   SET heartbeat_received_at = last_heartbeat_at
 WHERE last_heartbeat_at IS NOT NULL
   AND status IN ('QUEUED', 'RUNNING');

-- +goose Down

ALTER TABLE varroa.runs DROP COLUMN IF EXISTS heartbeat_received_at;
