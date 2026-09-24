-- Migration 00018 adds "replay_of" to "runs". This migration adds no table,
-- so the table set stays thirteen.
--
-- "replay_of" names the run a replayed build copied its script from. The
-- STARTED event of the replayed build carries the value, and no later event
-- changes it.
--
-- The column is nullable and carries no default. A build that is not a
-- replay keeps NULL.
--
-- The column carries no index. Every reader resolves it from a row it has
-- already found by primary key.

-- +goose Up

ALTER TABLE varroa.runs
    ADD COLUMN replay_of uuid;

-- +goose Down

ALTER TABLE varroa.runs
    DROP COLUMN IF EXISTS replay_of;
