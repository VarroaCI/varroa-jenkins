-- Migration 00024 adds "retired_incarnations" to "tenant_placements". This
-- migration adds no table, so the table set stays thirteen.
--
-- "retired_incarnations" holds the incarnations that a take-over replaced:
-- a jsonb array of "metadata.uid" strings, newest first. A take-over adds
-- the stored incarnation before it writes the new one. Kubernetes never
-- gives a uid to a second object, so a retired incarnation never comes
-- back legitimately. The announce refuses a request that names one, as
-- stale.
--
-- The recorder keeps the eight newest entries and applies no time limit.
-- A teardown purge adds no entry, because a Team that drops
-- "spec.pipelines" keeps its uid and can add the field again.
--
-- The default is an empty array. The column carries no index. Every reader
-- resolves it from a row it has already found by primary key.

-- +goose Up

ALTER TABLE varroa.tenant_placements
    ADD COLUMN retired_incarnations jsonb NOT NULL DEFAULT '[]'::jsonb;

-- +goose Down

ALTER TABLE varroa.tenant_placements
    DROP COLUMN IF EXISTS retired_incarnations;
