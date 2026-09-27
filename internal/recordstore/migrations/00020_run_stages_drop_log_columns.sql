-- Migration 00020 drops "log_offset" and "log_length" from "run_stages".
-- This migration adds no table, so the table set stays thirteen.
--
-- No writer ever filled the two columns, so every row holds NULL in both.
-- A per-stage log resolves through the stage tags of the log index, not
-- through a byte range on the stage row.
--
-- The down step adds the two columns back as nullable. It restores no
-- data, because the columns held none.

-- +goose Up

ALTER TABLE varroa.run_stages
    DROP COLUMN IF EXISTS log_offset,
    DROP COLUMN IF EXISTS log_length;

-- +goose Down

ALTER TABLE varroa.run_stages
    ADD COLUMN IF NOT EXISTS log_offset bigint,
    ADD COLUMN IF NOT EXISTS log_length bigint;
