-- Migration 00033 adds the upstream origin table, "varroa.upstream_origins".
-- The table set grows from sixteen to seventeen.
--
-- The recorder writes one row here before it publishes a trigger_run action
-- that carries an upstream intent key. The row names the child run's tenant
-- and tenant-qualified job path. The run.started ingest stamps an upstream
-- link from an upstream key only when a row matches the child run. The
-- retention round deletes rows older than the oldest runs partition. The
-- tenant purge deletes the rows of a purged tenant.

-- +goose Up

CREATE TABLE varroa.upstream_origins (
    intent_key  text PRIMARY KEY,
    cluster     text NOT NULL,
    tenant      text NOT NULL,
    job_path    text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT upstream_origins_intent_key_length CHECK (octet_length(intent_key) <= 512)
);
CREATE INDEX upstream_origins_tenant_idx ON varroa.upstream_origins (cluster, tenant);
CREATE INDEX upstream_origins_created_idx ON varroa.upstream_origins (created_at);

-- +goose Down

DROP INDEX IF EXISTS varroa.upstream_origins_created_idx;
DROP INDEX IF EXISTS varroa.upstream_origins_tenant_idx;
DROP TABLE IF EXISTS varroa.upstream_origins;
