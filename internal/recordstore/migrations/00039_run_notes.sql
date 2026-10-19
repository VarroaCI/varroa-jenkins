-- Migration 00039 adds the run_notes table. A note is either a run note
-- or a job note. The recorder is the only writer of the table. The
-- BFF reads it through varroa_client, which keeps SELECT only. The down
-- step drops the table.

-- +goose Up
CREATE TABLE varroa.run_notes (
  id uuid PRIMARY KEY,
  run_id uuid,
  run_created_at timestamptz,
  cluster text NOT NULL,
  team text NOT NULL,
  job_path text NOT NULL CHECK (job_path <> ''),
  principal text NOT NULL,
  session_id uuid,
  body text NOT NULL CHECK (octet_length(body) BETWEEN 1 AND 16384),
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((run_id IS NULL) = (run_created_at IS NULL))
);
CREATE INDEX run_notes_run_idx ON varroa.run_notes (run_id, created_at);
CREATE INDEX run_notes_job_idx ON varroa.run_notes (cluster, team, job_path, created_at DESC);
CREATE INDEX run_notes_run_created_idx ON varroa.run_notes (run_created_at);
CREATE UNIQUE INDEX run_notes_session_idx ON varroa.run_notes (session_id)
  WHERE session_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS varroa.run_notes;
