-- Migration 00022 adds "agent_namespace" to "tenant_placements". This
-- migration adds no table, so the table set stays thirteen.
--
-- "agent_namespace" holds the namespace of the tenant's build agents, as
-- the tenant's own cluster announces it. A member cluster holds the Team
-- object, so the core has no other copy of this value. The cell usage
-- read reports it on each tenant row.
--
-- The default is the empty string, which means "not announced". A row
-- keeps that value until its tenant announces again.
--
-- The column carries no index. Every reader resolves it from a row it has
-- already found.

-- +goose Up

ALTER TABLE varroa.tenant_placements
    ADD COLUMN agent_namespace text NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE varroa.tenant_placements
    DROP COLUMN IF EXISTS agent_namespace;
