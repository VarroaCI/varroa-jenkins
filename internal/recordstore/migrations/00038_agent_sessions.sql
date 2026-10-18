-- Migration 00038 adds the four agent tables beside the record tables.
-- They do not count toward the record table set. The BFF inserts and
-- updates their rows through varroa_client. The recorder deletes them in
-- the maintenance round. The down step drops the four tables.

-- +goose Up

CREATE TABLE varroa.agent_sessions (
  id uuid PRIMARY KEY,
  kind text NOT NULL CHECK (kind IN ('assistant','team_agent')),
  principal text NOT NULL,
  team text NOT NULL DEFAULT '',
  cluster text NOT NULL,
  trigger text NOT NULL DEFAULT 'manual',
  context jsonb NOT NULL DEFAULT '{}',
  provider text NOT NULL,
  model text NOT NULL,
  key_source text NOT NULL CHECK (key_source IN ('chart','user')),
  data_sharing text NOT NULL CHECK (data_sharing IN ('none','metadata','logs')),
  instructions_sha text,
  team_agent_generation bigint,
  state text NOT NULL CHECK (state IN ('running','waiting_approval','completed','failed',
                                       'cancelled','skipped','expired')),
  skip_reason text CHECK (skip_reason IN ('budget_day','kill_switch',
                                          'data_sharing_none','provider_unavailable')),
  prompt_tokens bigint NOT NULL DEFAULT 0,
  completion_tokens bigint NOT NULL DEFAULT 0,
  steps int NOT NULL DEFAULT 0,
  lease_owner text,
  lease_expires_at timestamptz,
  started_at timestamptz NOT NULL,
  ended_at timestamptz,
  CHECK ((state = 'skipped') = (skip_reason IS NOT NULL))
);
CREATE INDEX agent_sessions_principal_started_idx ON varroa.agent_sessions (principal, started_at);
CREATE INDEX agent_sessions_team_started_idx ON varroa.agent_sessions (team, started_at);
CREATE INDEX agent_sessions_lease_idx ON varroa.agent_sessions (lease_expires_at)
  WHERE state = 'running';

CREATE TABLE varroa.agent_session_events (
  session_id uuid NOT NULL,
  seq int NOT NULL,
  type text NOT NULL CHECK (type IN ('user','assistant','tool_call','tool_result',
                                     'approval_requested','approval_resolved','error','done')),
  payload jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (session_id, seq)
);

CREATE TABLE varroa.agent_approvals (
  id uuid PRIMARY KEY,
  session_id uuid NOT NULL,
  call_id text NOT NULL,
  tool text NOT NULL,
  arguments jsonb NOT NULL,
  target_kind text NOT NULL CHECK (target_kind IN ('run','job','controller','none')),
  target_ref text NOT NULL DEFAULT '',
  state text NOT NULL CHECK (state IN ('pending','approved','denied','expired')),
  requested_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  resolved_by text,
  resolved_at timestamptz,
  reason text
);
CREATE INDEX agent_approvals_session_idx ON varroa.agent_approvals (session_id);
CREATE INDEX agent_approvals_pending_idx ON varroa.agent_approvals (expires_at)
  WHERE state = 'pending';
CREATE INDEX agent_approvals_target_idx ON varroa.agent_approvals (target_kind, target_ref);

CREATE TABLE varroa.agent_settings (
  scope text PRIMARY KEY CHECK (scope = 'global' OR scope LIKE 'user:%'),
  paused boolean NOT NULL DEFAULT false,
  paused_by text,
  paused_at timestamptz,
  settings jsonb NOT NULL DEFAULT '{}',
  updated_at timestamptz NOT NULL DEFAULT now(),
  updated_by text
);
INSERT INTO varroa.agent_settings (scope) VALUES ('global') ON CONFLICT DO NOTHING;

-- +goose Down

DROP TABLE IF EXISTS varroa.agent_approvals;
DROP TABLE IF EXISTS varroa.agent_session_events;
DROP TABLE IF EXISTS varroa.agent_sessions;
DROP TABLE IF EXISTS varroa.agent_settings;
