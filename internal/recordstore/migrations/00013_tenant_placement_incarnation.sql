-- Migration 00013 adds "incarnation" to "tenant_placements". This migration
-- adds no table, so the table set stays twelve.
--
-- "incarnation" holds the identity of the Team object the row describes: the
-- object's "metadata.uid". A Team deleted and created again under one name
-- carries a new uid, so the column tells one tenant from the next.
--
-- The teardown purge reads it. That purge can arrive after the Team finalizer
-- clears, and a tenant of the same name created in that window owns the row by
-- then. A purge that names another incarnation than the stored one does
-- nothing.
--
-- The default is the empty string, which means "unknown". A row written before
-- this column existed keeps that value until its tenant announces again, and
-- every test treats an empty value as a match.
--
-- The column carries no index. Every reader resolves it from a row it has
-- already found by primary key.

-- +goose Up

ALTER TABLE varroa.tenant_placements
    ADD COLUMN incarnation text NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE varroa.tenant_placements
    DROP COLUMN IF EXISTS incarnation;
