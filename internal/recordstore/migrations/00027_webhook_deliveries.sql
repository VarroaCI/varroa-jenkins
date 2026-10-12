-- Migration 00027 adds the completion webhook. It adds one column to
-- "varroa.tenant_placements" and one table, "varroa.webhook_deliveries",
-- so the table set grows from thirteen to fourteen.
--
-- "notification_webhook" holds the receiver URL and the HMAC signing key
-- of a tenant's completion webhook. The operator announces it on
-- tenant.sync. It is NULL when the tenant declares no webhook. It never
-- joins "policy", because the grant evaluator and the backstop read
-- "policy".
--
-- "webhook_deliveries" is the outbox of the completion webhook. The
-- recorder writes one row for each owner run.completed that carries a
-- result, in the same transaction as the run's terminal write. The
-- dispatcher leases rows with FOR UPDATE SKIP LOCKED, so several replicas
-- never send one delivery twice. "body" holds the exact bytes the
-- dispatcher signs and sends.

-- +goose Up

ALTER TABLE varroa.tenant_placements ADD COLUMN notification_webhook jsonb;

CREATE TABLE varroa.webhook_deliveries (
    delivery_id     uuid PRIMARY KEY,
    run_id          uuid NOT NULL UNIQUE,
    cluster         text NOT NULL,
    tenant          text NOT NULL,
    -- The tenant incarnation at enqueue. A dispatcher sends only to the
    -- same incarnation, so a replacement Team never receives its
    -- predecessor's deliveries.
    incarnation     text NOT NULL DEFAULT '',
    body            bytea NOT NULL,
    state           text NOT NULL DEFAULT 'pending'
                    CHECK (state IN ('pending', 'delivered', 'failed', 'dropped')),
    attempts        int NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_status     int,
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX webhook_deliveries_pending_idx
    ON varroa.webhook_deliveries (next_attempt_at) WHERE state = 'pending';
CREATE INDEX webhook_deliveries_state_idx
    ON varroa.webhook_deliveries (state, updated_at);

-- +goose Down

DROP TABLE IF EXISTS varroa.webhook_deliveries;
ALTER TABLE varroa.tenant_placements DROP COLUMN IF EXISTS notification_webhook;
