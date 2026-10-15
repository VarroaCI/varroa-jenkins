-- Migration 00009 adds "refired_at" and "refire_attempts" to
-- "varroa.run_intents". This migration adds no table, so the table set
-- stays twelve.
--
-- The recorder publishes one trigger_run action for a granted intent
-- whose owning cell instance is confirmed gone. "refired_at" records
-- that publish. The recorder sets it under a conditional update, so two
-- replicas publish one action.
--
-- A rejected trigger clears "refired_at" so a later round retries, and a
-- retried run keeps its own run identifier, so the runs table holds no
-- per-attempt evidence. "refire_attempts" is therefore the only bound on
-- that retry loop. It never decreases.

-- +goose Up

ALTER TABLE varroa.run_intents ADD COLUMN refired_at timestamptz;

ALTER TABLE varroa.run_intents ADD COLUMN refire_attempts integer NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE varroa.run_intents DROP COLUMN IF EXISTS refire_attempts;

ALTER TABLE varroa.run_intents DROP COLUMN IF EXISTS refired_at;
