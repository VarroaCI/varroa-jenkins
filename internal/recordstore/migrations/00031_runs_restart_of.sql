-- Migration 00031 adds "restart_of" and "restart_stage" to "runs". This
-- migration adds no table, so the table set stays fifteen.
--
-- "restart_of" names the run a restarted build reran from. "restart_stage"
-- names the stage the restart began at. The STARTED event of the restarted
-- build carries both values, and no later event changes them.
--
-- Both columns are nullable and carry no default. A build that is not a
-- restart keeps NULL in both.
--
-- The columns carry no index. Every reader resolves them from a row it has
-- already found by primary key.

-- +goose Up

ALTER TABLE varroa.runs
    ADD COLUMN restart_of    uuid,
    ADD COLUMN restart_stage text;

-- +goose Down

ALTER TABLE varroa.runs
    DROP COLUMN IF EXISTS restart_stage,
    DROP COLUMN IF EXISTS restart_of;
