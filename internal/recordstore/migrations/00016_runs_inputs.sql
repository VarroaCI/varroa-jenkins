-- Migration 00016 adds "inputs" to "runs". This migration adds no table, so
-- the table set stays twelve.
--
-- "inputs" holds the pending input steps a run.input event reports. The
-- recorder replaces the whole value on each applying write, because the
-- event carries the run's whole pending set and never a delta.
--
-- The column is nullable and carries no default. An empty set writes NULL,
-- so no reader distinguishes NULL from an empty array.
--
-- The column carries no index. Every reader resolves it from a row it has
-- already found by primary key.

-- +goose Up

ALTER TABLE varroa.runs
    ADD COLUMN inputs jsonb;

-- +goose Down

ALTER TABLE varroa.runs
    DROP COLUMN IF EXISTS inputs;
