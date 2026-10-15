-- Migration 00014 adds "source" to "runs". This migration adds no table, so
-- the table set stays twelve.
--
-- "source" holds the facts a run.source event reports about how the run was
-- defined: definition_path, libraries, and pod_templates. The recorder merges
-- the object key by key, so a later event adds a key without dropping one.
--
-- The column is nullable and carries no default. A run that reports no source
-- keeps NULL, and every reader treats NULL and '{}' alike.
--
-- The column carries no index. Every reader resolves it from a row it has
-- already found by primary key.

-- +goose Up

ALTER TABLE varroa.runs
    ADD COLUMN source jsonb;

-- +goose Down

ALTER TABLE varroa.runs
    DROP COLUMN IF EXISTS source;
