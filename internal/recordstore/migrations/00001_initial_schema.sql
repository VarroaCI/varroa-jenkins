-- Migration 00001 creates the seven build-record tables in schema
-- "varroa". The runner creates the schema before this file runs; no
-- statement here creates it.
--
-- Monthly RANGE partition names on "runs" and "log_chunks" follow the
-- pattern "<table>_p<YYYYMM>", for example "runs_p202609". The retention
-- ticker that creates later partitions must use the same pattern, so a
-- partition this migration created and one the ticker creates for the same
-- month resolve to the same table name.

-- +goose Up

CREATE TABLE varroa.job_numbers (
    cluster     text NOT NULL,
    tenant      text NOT NULL,
    job_path    text NOT NULL,
    next_number bigint NOT NULL DEFAULT 1,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster, tenant, job_path)
);

CREATE TABLE varroa.run_intents (
    intent_key           text NOT NULL PRIMARY KEY,
    run_id               uuid NOT NULL,
    created_at           timestamptz NOT NULL,
    cluster              text NOT NULL,
    namespace            text NOT NULL,
    tenant               text NOT NULL,
    job_path             text NOT NULL,
    cell                 text,
    build_number         bigint,
    placement_generation bigint NOT NULL DEFAULT 0,
    state                text NOT NULL,
    actor                text,
    cause                text,
    params_digest        text,
    consumed_at          timestamptz,
    cell_instance        text,
    CONSTRAINT run_intents_intent_key_length CHECK (octet_length(intent_key) <= 512),
    CONSTRAINT run_intents_state_check CHECK (state IN ('GRANTED', 'CONSUMED')),
    CONSTRAINT run_intents_run_id_key UNIQUE (run_id),
    CONSTRAINT run_intents_build_number_key UNIQUE (cluster, tenant, job_path, build_number)
);

CREATE INDEX run_intents_conflict_idx
    ON varroa.run_intents (cluster, tenant, job_path, build_number DESC);

CREATE INDEX run_intents_orphan_idx
    ON varroa.run_intents (state, created_at);

CREATE TABLE varroa.runs (
    run_id                    uuid NOT NULL,
    created_at                timestamptz NOT NULL,
    intent_key                text NOT NULL,
    cluster                   text NOT NULL,
    namespace                 text NOT NULL,
    cell                      text NOT NULL,
    tenant                    text NOT NULL,
    job_path                  text NOT NULL,
    build_number              bigint,
    branch                    text,
    scm_revision              text,
    status                    text NOT NULL,
    result                    text,
    queued_at                 timestamptz,
    started_at                timestamptz,
    ended_at                  timestamptz,
    last_heartbeat_at         timestamptz,
    interrupted_superseded_at timestamptz,
    duration_ms               bigint,
    actor                     text,
    cause                     jsonb,
    params                    jsonb,
    changes                   jsonb,
    tests_total               integer,
    tests_failed              integer,
    tests_skipped             integer,
    payload                   jsonb,
    CONSTRAINT runs_status_check CHECK (status IN ('QUEUED', 'RUNNING', 'COMPLETED', 'DUPLICATE')),
    CONSTRAINT runs_result_check CHECK (
        result IS NULL OR result IN ('SUCCESS', 'UNSTABLE', 'FAILURE', 'ABORTED', 'NOT_BUILT', 'INTERRUPTED')
    ),
    PRIMARY KEY (created_at, run_id)
) PARTITION BY RANGE (created_at);

CREATE INDEX runs_run_id_idx ON varroa.runs (run_id);
CREATE INDEX runs_job_idx ON varroa.runs (cluster, tenant, job_path, created_at DESC);
CREATE INDEX runs_tenant_idx ON varroa.runs (cluster, tenant, created_at DESC);
CREATE INDEX runs_scm_revision_idx ON varroa.runs (cluster, tenant, job_path, scm_revision);

CREATE TABLE varroa.run_stages (
    run_id      uuid NOT NULL,
    stage_id    text NOT NULL,
    created_at  timestamptz NOT NULL,
    parent_id   text,
    name        text NOT NULL,
    node_type   text NOT NULL,
    status      text NOT NULL,
    started_at  timestamptz,
    ended_at    timestamptz,
    duration_ms bigint,
    error       text,
    log_offset  bigint,
    log_length  bigint,
    payload     jsonb,
    CONSTRAINT run_stages_node_type_check CHECK (node_type IN ('stage', 'parallel', 'step')),
    CONSTRAINT run_stages_status_check CHECK (status IN (
        'IN_PROGRESS', 'SUCCESS', 'UNSTABLE', 'FAILURE', 'ABORTED',
        'NOT_BUILT', 'SKIPPED', 'NOT_EXECUTED', 'PAUSED'
    )),
    PRIMARY KEY (run_id, stage_id)
);

CREATE INDEX run_stages_created_at_idx ON varroa.run_stages (created_at);

CREATE TABLE varroa.run_tests (
    run_id      uuid NOT NULL,
    suite       text NOT NULL,
    created_at  timestamptz NOT NULL,
    total       integer NOT NULL DEFAULT 0,
    failed      integer NOT NULL DEFAULT 0,
    skipped     integer NOT NULL DEFAULT 0,
    duration_ms bigint,
    PRIMARY KEY (run_id, suite)
);

CREATE INDEX run_tests_created_at_idx ON varroa.run_tests (created_at);

CREATE TABLE varroa.run_artifacts (
    run_id       uuid NOT NULL,
    path         text NOT NULL,
    created_at   timestamptz NOT NULL,
    size_bytes   bigint NOT NULL,
    digest       text,
    content_type text,
    blob_key     text NOT NULL,
    PRIMARY KEY (run_id, path)
);

CREATE INDEX run_artifacts_created_at_idx ON varroa.run_artifacts (created_at);

CREATE TABLE varroa.log_chunks (
    object_key   text NOT NULL,
    chunk_seq    bigint NOT NULL,
    created_at   timestamptz NOT NULL,
    object_class text NOT NULL,
    content_type text,
    digest       text NOT NULL,
    bytes        bytea NOT NULL,
    CONSTRAINT log_chunks_object_class_check CHECK (object_class IN ('log', 'artifact')),
    PRIMARY KEY (created_at, object_key, chunk_seq)
) PARTITION BY RANGE (created_at);

CREATE INDEX log_chunks_object_idx ON varroa.log_chunks (object_key, chunk_seq);
CREATE INDEX log_chunks_class_idx ON varroa.log_chunks (object_class, created_at);

-- The parent tables above accept no row until a partition covers its
-- "created_at". This block creates the partition for the previous month,
-- the current month, and the next two months, so a fresh install accepts
-- writes right away. The retention ticker repeats the same four-month
-- window every hour on its own schedule.
-- +goose StatementBegin
DO $$
DECLARE
    month_start date;
    suffix text;
BEGIN
    FOR month_start IN
        SELECT generate_series(
            date_trunc('month', now()) - interval '1 month',
            date_trunc('month', now()) + interval '2 month',
            interval '1 month'
        )::date
    LOOP
        suffix := to_char(month_start, 'YYYYMM');
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS varroa.%I PARTITION OF varroa.runs FOR VALUES FROM (%L) TO (%L)',
            'runs_p' || suffix, month_start, month_start + interval '1 month'
        );
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS varroa.%I PARTITION OF varroa.log_chunks FOR VALUES FROM (%L) TO (%L)',
            'log_chunks_p' || suffix, month_start, month_start + interval '1 month'
        );
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down

DROP TABLE IF EXISTS varroa.log_chunks;
DROP TABLE IF EXISTS varroa.run_artifacts;
DROP TABLE IF EXISTS varroa.run_tests;
DROP TABLE IF EXISTS varroa.run_stages;
DROP TABLE IF EXISTS varroa.runs;
DROP TABLE IF EXISTS varroa.run_intents;
DROP TABLE IF EXISTS varroa.job_numbers;
