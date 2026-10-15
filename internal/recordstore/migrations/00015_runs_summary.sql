-- Migration 00015 adds "summary" to "runs". This migration adds no table, so
-- the table set stays twelve.
--
-- "summary" holds the one line of text a run attaches to itself through
-- varroa.summary. The recorder raises it from a run.heartbeat envelope and
-- from a run.completed envelope, and it gates each raise on the envelope's
-- own seq against the payload's summary_seq key. A run.source write uses the
-- same gate for source_seq.
--
-- The column is nullable and carries no default. A run that never calls
-- varroa.summary keeps NULL, and no reader treats NULL as an empty string.
--
-- The column carries no index. Every reader resolves it from a row it has
-- already found by primary key.

-- +goose Up

ALTER TABLE varroa.runs
    ADD COLUMN summary text;

-- +goose Down

ALTER TABLE varroa.runs
    DROP COLUMN IF EXISTS summary;
