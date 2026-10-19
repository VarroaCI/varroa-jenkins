-- Migration 00040 adds the team_agent_spend table. The recorder keeps one
-- row per Team and UTC day. The BFF and the agent runner read it, and the
-- client role writes it with INSERT and UPDATE only. No role deletes a row
-- by hand; the retention round removes rows past the runs window. The down
-- step drops the table.

-- +goose Up
CREATE TABLE varroa.team_agent_spend (
  team        text NOT NULL,
  day         date NOT NULL,
  tokens      bigint NOT NULL DEFAULT 0,
  sessions    int NOT NULL DEFAULT 0,
  skipped     int NOT NULL DEFAULT 0,
  failed      int NOT NULL DEFAULT 0,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (team, day)
);

-- +goose Down
DROP TABLE IF EXISTS varroa.team_agent_spend;
