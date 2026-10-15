-- Migration 00032 creates "job_branch_projects" in schema "varroa" and
-- adds "branches_observed_at" to "job_schedule_cells". The table set
-- grows by one, to the count the schema test pins.
--
-- The table holds the branch and change-request inventory of each
-- multibranch job of a cell. It is not partitioned: a row describes the
-- live state of one job, not a time-bounded history. No retention step
-- ages a row. A row leaves only when its cell stops exporting it, or
-- when its tenant is purged.
--
-- The cell's job.branches watermark is a second column on
-- job_schedule_cells, so the two envelope kinds share one row lock.

-- +goose Up

CREATE TABLE varroa.job_branch_projects (
    cluster               text NOT NULL DEFAULT '',
    namespace             text NOT NULL DEFAULT '',
    cell                  text NOT NULL DEFAULT '',
    tenant                text NOT NULL DEFAULT '',
    job_path              text NOT NULL DEFAULT '',
    placement_generation  bigint NOT NULL DEFAULT 0,
    observed_at           timestamptz NOT NULL,
    indexed_at            timestamptz,
    index_result          text NOT NULL DEFAULT '',
    heads                 jsonb NOT NULL,
    heads_truncated       boolean NOT NULL DEFAULT false,
    last_scan             jsonb,
    PRIMARY KEY (cluster, tenant, job_path)
);

CREATE INDEX job_branch_projects_cell_idx ON varroa.job_branch_projects (cluster, namespace, cell);

ALTER TABLE varroa.job_schedule_cells
    ADD COLUMN branches_observed_at timestamptz NOT NULL DEFAULT '-infinity';

-- +goose Down

ALTER TABLE varroa.job_schedule_cells DROP COLUMN IF EXISTS branches_observed_at;
DROP TABLE IF EXISTS varroa.job_branch_projects;
