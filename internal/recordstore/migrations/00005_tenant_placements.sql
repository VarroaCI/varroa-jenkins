-- Migration 00005 creates "tenant_placements" in schema "varroa". The
-- table set becomes eleven after migration 00001's seven, migration
-- 00002's one, and migration 00003's two.
--
-- The recorder is the sole writer. "cluster" and "tenant" form the
-- primary key: one row names the whole fleet's placement of one tenant.
-- "rehydrated_instance" and "pending_jobs" are the seed state the
-- execution fence reads. "cell_alive", "cell_alive_at",
-- "cell_last_seen_at", and "dead_instances" are the cell-liveness
-- signal the interrupted-run sweep reads, because the recorder holds
-- no bus permission on a "$KV." subject.

-- +goose Up

CREATE TABLE varroa.tenant_placements (
    cluster              text NOT NULL,
    tenant               text NOT NULL,
    namespace            text NOT NULL DEFAULT '',
    cell                 text NOT NULL DEFAULT '',
    generation           bigint NOT NULL DEFAULT 0,
    cell_instance        text NOT NULL DEFAULT '',
    policy               jsonb NOT NULL DEFAULT '{}',
    rehydrated_instance  text NOT NULL DEFAULT '',
    pending_jobs         jsonb NOT NULL DEFAULT '[]',
    cell_alive           boolean NOT NULL DEFAULT false,
    cell_alive_at        timestamptz,
    cell_last_seen_at    timestamptz,
    dead_instances       jsonb NOT NULL DEFAULT '[]',
    announced_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster, tenant)
);

-- +goose Down

DROP TABLE IF EXISTS varroa.tenant_placements;
