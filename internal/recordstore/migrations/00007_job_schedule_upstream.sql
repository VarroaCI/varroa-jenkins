-- Migration 00007 adds the three upstream columns to "job_schedules" in
-- schema "varroa". This migration adds no table, so the table set stays
-- twelve.
--
-- "upstream_tenant", "upstream_job", and "upstream_result" describe a
-- "varroa-upstream" trigger row. Every other row carries the three
-- columns empty. The index serves the cross-tenant fan-out query, which
-- looks up every subscriber of one producer's tenant and job path.

-- +goose Up

ALTER TABLE varroa.job_schedules
    ADD COLUMN upstream_tenant text NOT NULL DEFAULT '',
    ADD COLUMN upstream_job    text NOT NULL DEFAULT '',
    ADD COLUMN upstream_result text NOT NULL DEFAULT '';

CREATE INDEX job_schedules_upstream_idx ON varroa.job_schedules (upstream_tenant, upstream_job);

-- +goose Down

DROP INDEX IF EXISTS varroa.job_schedules_upstream_idx;

ALTER TABLE varroa.job_schedules
    DROP COLUMN upstream_tenant,
    DROP COLUMN upstream_job,
    DROP COLUMN upstream_result;
