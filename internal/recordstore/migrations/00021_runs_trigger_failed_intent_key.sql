-- Migration 00021 adds a partial index on "runs (intent_key)" for rows
-- whose result is TRIGGER_FAILED. This migration adds no table, so the
-- table set stays thirteen.
--
-- A run that failed to trigger holds no run_intents row. The cross-tenant
-- status read by intent key finds such a run through this index. The
-- predicate keeps the index small, and a real run never enters it. A
-- query must name the same literal predicate to use the index.

-- +goose Up

CREATE INDEX IF NOT EXISTS runs_trigger_failed_intent_key_idx
    ON varroa.runs (intent_key)
    WHERE result = 'TRIGGER_FAILED';

-- +goose Down

DROP INDEX IF EXISTS varroa.runs_trigger_failed_intent_key_idx;
