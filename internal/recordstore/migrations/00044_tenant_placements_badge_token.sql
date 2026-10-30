-- Migration 00044 adds the badge token of a member-placed tenant to
-- tenant_placements. The column is NULL when the tenant's badge is off.
-- It never joins policy. No reader outside the recorder selects it, except
-- the TenantBadgeToken read that the BFF uses for the anonymous badge. The
-- down step drops the column.

-- +goose Up

ALTER TABLE varroa.tenant_placements
    ADD COLUMN badge_token text;

-- +goose Down

ALTER TABLE varroa.tenant_placements DROP COLUMN IF EXISTS badge_token;
