-- Migration 00028 adds one index to "runs". This migration adds no table,
-- so the table set stays thirteen.
--
-- The index serves the dependency edge's last-run lookups. Each lookup
-- names one job path and one producer job, and reads the newest run of
-- that pair inside the 30-day window. Without this index, the lookup reads
-- every run of the job path.

-- +goose Up

CREATE INDEX IF NOT EXISTS runs_upstream_edge_idx
    ON varroa.runs (cluster, tenant, job_path, upstream_tenant, upstream_job, created_at DESC);

-- +goose Down

DROP INDEX IF EXISTS varroa.runs_upstream_edge_idx;
