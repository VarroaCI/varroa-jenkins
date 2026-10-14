-- Migration 00036 deletes the stored sentinel suite rows named "_total".
-- The table set and the column set do not change.
--
-- Earlier plugins sent one "_total" row for each run. The suite rows now
-- hold a breakdown, and the run's totals come from the runs row. A stored
-- "_total" row would show as a suite named "_total" in the run's suite
-- list, so the migration removes every such row. The runs row keeps the
-- run's totals.
--
-- The delete cannot be reversed. The down step keeps the schema at this
-- version and restores no row.

-- +goose Up

DELETE FROM varroa.run_tests WHERE suite = '_total';

-- +goose Down

SELECT 1;
