-- Migration 00041 renames the Team terms in the stored schema to Tenant.
-- It renames the agent_sessions column and index, the team_agent_spend
-- table, its column and primary key, and the run_notes column. It
-- rewrites the stored kind values, the activity type prefixes, and the
-- agent settings key. Rows keep their data; no row is copied. History
-- columns that hold JSON keys or tool arguments keep their old text. The
-- down step reverses every statement in reverse order.

-- +goose Up

ALTER TABLE varroa.agent_sessions DROP CONSTRAINT agent_sessions_kind_check;
UPDATE varroa.agent_sessions SET kind = 'tenant_agent' WHERE kind = 'team_agent';
ALTER TABLE varroa.agent_sessions ADD CONSTRAINT agent_sessions_kind_check
  CHECK (kind IN ('assistant','tenant_agent'));

ALTER TABLE varroa.agent_sessions RENAME COLUMN team TO tenant;
ALTER TABLE varroa.agent_sessions RENAME COLUMN team_agent_generation TO tenant_agent_generation;
ALTER INDEX varroa.agent_sessions_team_started_idx RENAME TO agent_sessions_tenant_started_idx;

ALTER TABLE varroa.team_agent_spend RENAME TO tenant_agent_spend;
ALTER TABLE varroa.tenant_agent_spend RENAME COLUMN team TO tenant;
ALTER TABLE varroa.tenant_agent_spend RENAME CONSTRAINT team_agent_spend_pkey TO tenant_agent_spend_pkey;

ALTER TABLE varroa.run_notes RENAME COLUMN team TO tenant;

UPDATE varroa.activity
  SET type = replace(replace(type, 'team_agent.', 'tenant_agent.'), 'teamagent.', 'tenantagent.')
  WHERE type LIKE 'team%';

UPDATE varroa.agent_settings
  SET settings = (settings - 'team_agent_unattended')
    || jsonb_build_object('tenant_agent_unattended', settings->'team_agent_unattended')
  WHERE settings ? 'team_agent_unattended';

-- +goose Down

UPDATE varroa.agent_settings
  SET settings = (settings - 'tenant_agent_unattended')
    || jsonb_build_object('team_agent_unattended', settings->'tenant_agent_unattended')
  WHERE settings ? 'tenant_agent_unattended';

UPDATE varroa.activity
  SET type = replace(replace(type, 'tenant_agent.', 'team_agent.'), 'tenantagent.', 'teamagent.')
  WHERE type LIKE 'tenant%';

ALTER TABLE varroa.run_notes RENAME COLUMN tenant TO team;

ALTER TABLE varroa.tenant_agent_spend RENAME CONSTRAINT tenant_agent_spend_pkey TO team_agent_spend_pkey;
ALTER TABLE varroa.tenant_agent_spend RENAME COLUMN tenant TO team;
ALTER TABLE varroa.tenant_agent_spend RENAME TO team_agent_spend;

ALTER INDEX varroa.agent_sessions_tenant_started_idx RENAME TO agent_sessions_team_started_idx;
ALTER TABLE varroa.agent_sessions RENAME COLUMN tenant_agent_generation TO team_agent_generation;
ALTER TABLE varroa.agent_sessions RENAME COLUMN tenant TO team;

ALTER TABLE varroa.agent_sessions DROP CONSTRAINT agent_sessions_kind_check;
UPDATE varroa.agent_sessions SET kind = 'team_agent' WHERE kind = 'tenant_agent';
ALTER TABLE varroa.agent_sessions ADD CONSTRAINT agent_sessions_kind_check
  CHECK (kind IN ('assistant','team_agent'));
