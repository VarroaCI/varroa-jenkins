-- Migration 00035 records whether the last applied job inventory of each
-- kind was truncated. It adds two columns to "job_schedule_cells". The
-- table set does not change.
--
-- "schedules_truncated" holds the truncation flag of the last applied
-- job.schedules envelope of the cell. "branches_truncated" holds the
-- flag of the last applied job.branches envelope. A truncated set is
-- incomplete, so a reader cannot tell which declared jobs it omits.
--
-- The columns add with the default true. A row that exists before this
-- migration has no recorded flag, so it reads as truncated. Its cell
-- stays unknown until the cell reports a complete set again. After the
-- backfill the default becomes false, so a new row starts complete. Every
-- ingest write sets both flags explicitly.

-- +goose Up

ALTER TABLE varroa.job_schedule_cells
    ADD COLUMN schedules_truncated boolean NOT NULL DEFAULT true,
    ADD COLUMN branches_truncated  boolean NOT NULL DEFAULT true;

ALTER TABLE varroa.job_schedule_cells
    ALTER COLUMN schedules_truncated SET DEFAULT false,
    ALTER COLUMN branches_truncated SET DEFAULT false;

-- +goose Down

ALTER TABLE varroa.job_schedule_cells
    DROP COLUMN IF EXISTS branches_truncated,
    DROP COLUMN IF EXISTS schedules_truncated;
