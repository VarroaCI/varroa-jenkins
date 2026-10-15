-- Migration 00017 creates "run_test_cases" in schema "varroa" and adds
-- "test_report_key" to "runs". The table set becomes thirteen after
-- migration 00006's twelve.
--
-- "run_test_cases" holds the bounded failed and skipped case set a
-- run.tests envelope reports. The recorder replaces the run's whole set on
-- each write, because the envelope carries the whole set and never a delta.
--
-- "cluster", "tenant", and "job_path" come from the run's intent row, so the
-- case history read needs no join to find its scope. "created_at" equals the
-- run's own "created_at", so retention deletes a case row on the run cutoff.
--
-- "status" carries no CHECK constraint. A reporter can add a status name,
-- and a narrow CHECK would fail the recorder's INSERT.
--
-- The table is not partitioned and holds no foreign key to "runs", because
-- DROP TABLE on a "runs" partition fails while a referencing row exists.
--
-- "runs.test_report_key" holds the blob key of the archived test report. It
-- is nullable, carries no default, and carries no index.

-- +goose Up

CREATE TABLE varroa.run_test_cases (
    run_id        uuid        NOT NULL,
    suite         text        NOT NULL,
    class_name    text        NOT NULL,
    name          text        NOT NULL,
    cluster       text        NOT NULL DEFAULT '',
    tenant        text        NOT NULL DEFAULT '',
    job_path      text        NOT NULL DEFAULT '',
    status        text        NOT NULL,
    duration_ms   bigint,
    error_details text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL,
    PRIMARY KEY (run_id, suite, class_name, name)
);

CREATE INDEX run_test_cases_history_idx
    ON varroa.run_test_cases (cluster, tenant, job_path, class_name, name, created_at DESC);

CREATE INDEX run_test_cases_created_at_idx ON varroa.run_test_cases (created_at);

ALTER TABLE varroa.runs
    ADD COLUMN test_report_key text;

-- +goose Down

DROP TABLE IF EXISTS varroa.run_test_cases;

ALTER TABLE varroa.runs
    DROP COLUMN IF EXISTS test_report_key;
