-- Migration 00008 widens "runs_result_check" in schema "varroa" so
-- "runs.result" accepts TRIGGER_FAILED. This migration adds no table, so
-- the table set stays twelve.
--
-- A TRIGGER_FAILED row records a build trigger that Jenkins refused. The
-- constraint sits on the partitioned parent "varroa.runs", so every
-- partition inherits the new list.
--
-- The down step deletes every TRIGGER_FAILED row first. A row left behind
-- fails the narrower constraint.

-- +goose Up

ALTER TABLE varroa.runs DROP CONSTRAINT runs_result_check;

ALTER TABLE varroa.runs ADD CONSTRAINT runs_result_check CHECK (
    result IS NULL OR result IN ('SUCCESS', 'UNSTABLE', 'FAILURE', 'ABORTED', 'NOT_BUILT', 'INTERRUPTED', 'TRIGGER_FAILED')
);

-- +goose Down

DELETE FROM varroa.runs WHERE result = 'TRIGGER_FAILED';

ALTER TABLE varroa.runs DROP CONSTRAINT runs_result_check;

ALTER TABLE varroa.runs ADD CONSTRAINT runs_result_check CHECK (
    result IS NULL OR result IN ('SUCCESS', 'UNSTABLE', 'FAILURE', 'ABORTED', 'NOT_BUILT', 'INTERRUPTED')
);
