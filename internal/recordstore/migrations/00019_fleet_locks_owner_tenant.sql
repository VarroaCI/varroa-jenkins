-- Migration 00019 adds "owner_tenant" to "fleet_locks". This migration adds
-- no table, so the table set stays thirteen.
--
-- "owner_tenant" names the tenant that owns the lock. "name" keeps the
-- qualified form "<owner_tenant>/<bare name>", so the partial unique index
-- still needs no second column. A controller tenant holds a "/" of its own
-- ("<namespace>/<controller>"), so no reader may split "name" to find the
-- owner. Every reader filters on this column instead.
--
-- The backfill splits each stored name at its first "/". Every row that
-- exists before this migration belongs to a cell tenant, whose name holds
-- no "/", so the first segment is the owner.
--
-- The column gets a default only for the backfill. The default is then
-- dropped, so a writer that omits the owner fails at once. A row with an
-- empty owner would hold its name against every caller and appear in no
-- owner's read.
--
-- The index serves the tenant read, which filters on cluster and owner.

-- +goose Up

ALTER TABLE varroa.fleet_locks
    ADD COLUMN owner_tenant text NOT NULL DEFAULT '';

UPDATE varroa.fleet_locks
    SET owner_tenant = split_part(name, '/', 1)
    WHERE owner_tenant = '';

ALTER TABLE varroa.fleet_locks
    ALTER COLUMN owner_tenant DROP DEFAULT;

CREATE INDEX fleet_locks_cluster_owner_tenant ON varroa.fleet_locks (cluster, owner_tenant);

-- +goose Down

DROP INDEX IF EXISTS varroa.fleet_locks_cluster_owner_tenant;

ALTER TABLE varroa.fleet_locks
    DROP COLUMN IF EXISTS owner_tenant;
