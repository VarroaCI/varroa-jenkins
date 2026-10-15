-- Migration 00011 adds "members" and "job_homes" to "tenant_placements".
-- This migration adds no table, so the table set stays twelve.
--
-- "members" holds one entry for each member of the tenant. Slot 0's entry
-- repeats the row's scalar "cell", "namespace", "cell_instance",
-- "rehydrated_instance", and "pending_jobs". An empty list resolves to one
-- synthetic slot-0 active member built from those scalar columns, so a
-- controller with no cell role needs no branch at any reader.
--
-- "job_homes" maps a job path to a slot index. The recorder is its only
-- writer. A job path with no entry resolves to slot 0.
--
-- Neither column carries an index. Every reader resolves them from a row it
-- has already read by primary key.

-- +goose Up

ALTER TABLE varroa.tenant_placements
    ADD COLUMN members   jsonb NOT NULL DEFAULT '[]',
    ADD COLUMN job_homes jsonb NOT NULL DEFAULT '{}';

-- +goose Down

ALTER TABLE varroa.tenant_placements
    DROP COLUMN IF EXISTS members,
    DROP COLUMN IF EXISTS job_homes;
