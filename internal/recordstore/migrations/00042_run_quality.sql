-- Migration 00042 adds the run_coverage and run_warnings tables. Each row
-- holds the summary numbers of one coverage or warnings report of one
-- completed run. The tables hold no foreign key to runs and no
-- partitioning. Each row copies its scope from the run's intent and its
-- created_at from the run, so retention deletes it on the run's cutoff.
-- The down step drops both tables.

-- +goose Up

CREATE TABLE varroa.run_coverage (
    run_id                uuid        NOT NULL,
    report_id             text        NOT NULL,
    cluster               text        NOT NULL DEFAULT '',
    tenant                text        NOT NULL DEFAULT '',
    job_path              text        NOT NULL DEFAULT '',
    name                  text        NOT NULL DEFAULT '',
    line_covered          integer,
    line_total            integer,
    branch_covered        integer,
    branch_total          integer,
    modified_line_covered integer,
    modified_line_total   integer,
    metrics               jsonb       NOT NULL DEFAULT '{}',
    gate_status           text,
    created_at            timestamptz NOT NULL,
    PRIMARY KEY (run_id, report_id)
);
CREATE INDEX run_coverage_job_idx
    ON varroa.run_coverage (cluster, tenant, job_path, report_id, created_at DESC);
CREATE INDEX run_coverage_created_at_idx ON varroa.run_coverage (created_at);

CREATE TABLE varroa.run_warnings (
    run_id      uuid        NOT NULL,
    report_id   text        NOT NULL,
    cluster     text        NOT NULL DEFAULT '',
    tenant      text        NOT NULL DEFAULT '',
    job_path    text        NOT NULL DEFAULT '',
    name        text        NOT NULL DEFAULT '',
    total       integer     NOT NULL,
    error       integer     NOT NULL DEFAULT 0,
    high        integer     NOT NULL DEFAULT 0,
    normal      integer     NOT NULL DEFAULT 0,
    low         integer     NOT NULL DEFAULT 0,
    new_total   integer,
    fixed_total integer,
    gate_status text,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (run_id, report_id)
);
CREATE INDEX run_warnings_job_idx
    ON varroa.run_warnings (cluster, tenant, job_path, report_id, created_at DESC);
CREATE INDEX run_warnings_created_at_idx ON varroa.run_warnings (created_at);

-- +goose Down

DROP TABLE IF EXISTS varroa.run_warnings;
DROP TABLE IF EXISTS varroa.run_coverage;
