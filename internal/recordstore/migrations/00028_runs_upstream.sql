-- Migration 00027 adds four upstream columns to "runs". This migration adds
-- no table, so the table set stays thirteen.
--
-- The columns name the run that started this run, when another run started
-- it by a cross-tenant trigger, a varroaUpstream fan-out, or a Jenkins
-- upstream cause. The STARTED event writes them, and no later event changes
-- them.
--
-- "upstream_job" holds the tenant-qualified job path, the same form as
-- runs.job_path. "upstream_run_id" is null when the recorder found no
-- matching run, and the tenant and job keep their values.
--
-- The partial index serves the downstream list: every run that one upstream
-- run started, newest first.

-- +goose Up

ALTER TABLE varroa.runs
    ADD COLUMN upstream_run_id uuid,
    ADD COLUMN upstream_tenant text,
    ADD COLUMN upstream_job    text,
    ADD COLUMN upstream_number bigint;

CREATE INDEX IF NOT EXISTS runs_upstream_run_id_idx
    ON varroa.runs (upstream_run_id, created_at DESC, run_id DESC)
    WHERE upstream_run_id IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS varroa.runs_upstream_run_id_idx;

ALTER TABLE varroa.runs
    DROP COLUMN IF EXISTS upstream_number,
    DROP COLUMN IF EXISTS upstream_job,
    DROP COLUMN IF EXISTS upstream_tenant,
    DROP COLUMN IF EXISTS upstream_run_id;
