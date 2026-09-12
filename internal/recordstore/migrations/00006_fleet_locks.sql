-- Migration 00006 creates "fleet_locks" and its sequence in schema
-- "varroa". The table set becomes twelve after migration 00005's eleven.
--
-- The recorder is the sole writer. "name" holds the qualified lock name
-- "<tenant>/<name>", so one global namespace covers every lock and the
-- partial unique index needs no second column. The index allows only one
-- "held" or "releasing" row per name, so a second holder is impossible
-- at the database level, and a lock under cancellation admits no second
-- holder either.

-- +goose Up

CREATE SEQUENCE varroa.fleet_lock_fence_seq;

CREATE TABLE varroa.fleet_locks (
    name                 text        NOT NULL,
    run_id               text        NOT NULL,
    cluster              text        NOT NULL DEFAULT '',
    tenant               text        NOT NULL DEFAULT '',
    job_path             text        NOT NULL DEFAULT '',
    cell_instance        text        NOT NULL DEFAULT '',
    state                text        NOT NULL,
    fence_token          bigint      NOT NULL DEFAULT 0,
    waiting_since        timestamptz NOT NULL,
    acquired_at          timestamptz,
    releasing_since      timestamptz,
    expires_at           timestamptz NOT NULL,
    placement_generation bigint      NOT NULL DEFAULT 0,
    PRIMARY KEY (name, run_id),
    CHECK (state IN ('held', 'waiting', 'releasing')),
    CHECK (octet_length(name) <= 200)
);

CREATE UNIQUE INDEX fleet_locks_one_holder ON varroa.fleet_locks (name)
    WHERE state IN ('held', 'releasing');

-- +goose Down

DROP TABLE IF EXISTS varroa.fleet_locks;
DROP SEQUENCE IF EXISTS varroa.fleet_lock_fence_seq;
