-- Migration 00037 adds a partial index on "runs" for the restart-copy
-- retention round. This migration adds no table and no column, so the
-- table set and the column set do not change.
--
-- A run that holds a restart copy carries the key "restart_source" in its
-- payload. The index lists only those rows, in the order the round reads
-- them. The round reads a batch at a time and deletes each batch's copies,
-- so a row leaves the index as soon as its key is cleared.
--
-- The down step drops the index. It restores no data.

-- +goose Up

CREATE INDEX IF NOT EXISTS runs_restart_source_idx
    ON varroa.runs (created_at, run_id)
    WHERE payload ? 'restart_source';

-- +goose Down

DROP INDEX IF EXISTS varroa.runs_restart_source_idx;
