-- Migration 00043 adds the dropped counts to the run_coverage and
-- run_warnings tables. A count stores the reports or metrics that a run
-- published and the store does not hold. The plugin and the gateway
-- trim a set at fixed bounds, and the recorder dedupes ids, so each
-- count keeps the cut visible. Every existing row defaults to zero. The
-- down step drops the three columns.

-- +goose Up

ALTER TABLE varroa.run_coverage
    ADD COLUMN reports_dropped integer NOT NULL DEFAULT 0,
    ADD COLUMN metrics_dropped integer NOT NULL DEFAULT 0;

ALTER TABLE varroa.run_warnings
    ADD COLUMN reports_dropped integer NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE varroa.run_warnings DROP COLUMN IF EXISTS reports_dropped;
ALTER TABLE varroa.run_coverage DROP COLUMN IF EXISTS metrics_dropped;
ALTER TABLE varroa.run_coverage DROP COLUMN IF EXISTS reports_dropped;
