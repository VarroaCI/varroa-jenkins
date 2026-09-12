-- Migration 00003 creates "job_schedules" and "job_schedule_cells" in
-- schema "varroa". The table set becomes ten after migration 00001's
-- seven and migration 00002's one.
--
-- Neither table is partitioned: a schedule row describes live state, not
-- a time-bounded history row.

-- +goose Up

CREATE TABLE varroa.job_schedules (
    cluster               text NOT NULL DEFAULT '',
    namespace             text NOT NULL DEFAULT '',
    cell                  text NOT NULL DEFAULT '',
    tenant                text NOT NULL DEFAULT '',
    job_path              text NOT NULL DEFAULT '',
    trigger_type          text NOT NULL DEFAULT '',
    spec                  text NOT NULL DEFAULT '',
    timezone              text NOT NULL DEFAULT '',
    disabled              boolean NOT NULL DEFAULT false,
    next_fire_times       timestamptz[] NOT NULL DEFAULT '{}',
    observed_at           timestamptz NOT NULL,
    placement_generation  bigint NOT NULL DEFAULT 0,
    payload               jsonb NOT NULL,
    PRIMARY KEY (cluster, tenant, job_path, trigger_type)
);

CREATE INDEX job_schedules_tenant_trigger_idx ON varroa.job_schedules (tenant, trigger_type);
CREATE INDEX job_schedules_search_tsv_idx ON varroa.job_schedules USING GIN (to_tsvector('simple', job_path));

-- job_schedule_cells is the cell's schedule watermark. It holds one row
-- per cell and outlives an empty set, so a cell with no schedule row
-- still rejects a stale envelope. The recorder row-locks it inside the
-- envelope transaction, which serializes two replicas that read the
-- same cell.
CREATE TABLE varroa.job_schedule_cells (
    cluster     text NOT NULL DEFAULT '',
    namespace   text NOT NULL DEFAULT '',
    cell        text NOT NULL DEFAULT '',
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (cluster, namespace, cell)
);

-- +goose Down

DROP TABLE IF EXISTS varroa.job_schedule_cells;
DROP TABLE IF EXISTS varroa.job_schedules;
