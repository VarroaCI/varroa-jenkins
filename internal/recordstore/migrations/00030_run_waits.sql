-- Migration 00030 adds the tenant concurrency wait table, "varroa.run_waits",
-- and three indexes. The table set grows from fourteen to fifteen.
--
-- A claim that a concurrency cap holds writes one row here, keyed by the run
-- id. The row names the cap that blocks the run and the count that the cap
-- saw. The ledger deletes the row when it grants the run. The sweep deletes
-- a row that no claim has refreshed for an hour. The waiting-reason reads
-- take only the rows a claim refreshed in the last two minutes.
--
-- The two partial indexes serve the cap count. They let the claim read the
-- granted intents and the running runs of one tenant without a full scan.

-- +goose Up

CREATE TABLE varroa.run_waits (
    run_id        uuid PRIMARY KEY,
    cluster       text NOT NULL,
    namespace     text NOT NULL,
    tenant        text NOT NULL,
    job_path      text NOT NULL,
    cell          text NOT NULL DEFAULT '',
    cap_kind      text NOT NULL CHECK (cap_kind IN ('tenant', 'cell')),
    cap_name      text NOT NULL DEFAULT '',
    cap_max       int NOT NULL,
    running       int NOT NULL,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX run_waits_tenant_idx ON varroa.run_waits (cluster, tenant, last_seen_at);
CREATE INDEX run_waits_job_idx ON varroa.run_waits (cluster, tenant, job_path, first_seen_at);
CREATE INDEX IF NOT EXISTS run_intents_granted_tenant_idx
    ON varroa.run_intents (cluster, tenant) WHERE state = 'GRANTED';
CREATE INDEX IF NOT EXISTS runs_running_tenant_idx
    ON varroa.runs (cluster, tenant) WHERE status = 'RUNNING';

-- +goose Down

DROP INDEX IF EXISTS varroa.runs_running_tenant_idx;
DROP INDEX IF EXISTS varroa.run_intents_granted_tenant_idx;
DROP INDEX IF EXISTS varroa.run_waits_job_idx;
DROP INDEX IF EXISTS varroa.run_waits_tenant_idx;
DROP TABLE IF EXISTS varroa.run_waits;
