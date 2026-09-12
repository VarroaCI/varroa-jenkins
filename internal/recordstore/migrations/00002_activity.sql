-- Migration 00002 creates the "activity" table in schema "varroa". The
-- table set becomes ten after migration 00001's seven and migration
-- 00003's two.
--
-- Monthly RANGE partition names follow the pattern "activity_p<YYYYMM>",
-- matching migration 00001's naming for "runs" and "log_chunks". The
-- retention round's partition-creation step must use the same pattern.
--
-- This migration also adds the GIN expression search index on the
-- existing "runs" table from migration 00001, which is never itself
-- edited. CREATE INDEX on a RANGE-partitioned table creates a matching
-- index on every existing and future partition.

-- +goose Up

CREATE TABLE varroa.activity (
    event_key   text NOT NULL DEFAULT '',
    ts          timestamptz NOT NULL,
    type        text NOT NULL DEFAULT '',
    source      text NOT NULL DEFAULT '',
    severity    text NOT NULL DEFAULT '',
    cluster     text NOT NULL DEFAULT '',
    namespace   text NOT NULL DEFAULT '',
    controller  text NOT NULL DEFAULT '',
    actor       text NOT NULL DEFAULT '',
    message     text NOT NULL DEFAULT '',
    phase       text NOT NULL DEFAULT '',
    reason      text NOT NULL DEFAULT '',
    item_path   text NOT NULL DEFAULT '',
    build_number bigint,
    result      text NOT NULL DEFAULT '',
    url         text NOT NULL DEFAULT '',
    payload     jsonb NOT NULL,
    search_tsv  tsvector NOT NULL GENERATED ALWAYS AS (
        to_tsvector('simple', message||' '||type||' '||actor||' '||controller||' '||namespace||' '||item_path)
    ) STORED,
    PRIMARY KEY (ts, event_key)
) PARTITION BY RANGE (ts);

CREATE INDEX activity_namespace_controller_idx ON varroa.activity (namespace, controller, ts DESC);
CREATE INDEX activity_cluster_idx ON varroa.activity (cluster, ts DESC);
CREATE INDEX activity_search_tsv_idx ON varroa.activity USING GIN (search_tsv);

CREATE INDEX runs_search_tsv_idx ON varroa.runs USING GIN (
    to_tsvector('simple', job_path||' '||coalesce(branch,'')||' '||coalesce(actor,'')||' '||coalesce(build_number::text,''))
);

-- The parent table above accepts no row until a partition covers its
-- "ts". This block creates the partition for the previous month, the
-- current month, and the next two months, matching migration 00001's
-- window for "runs" and "log_chunks". The retention round reaches
-- further back, to retention.activity, on its own schedule.
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
            'CREATE TABLE IF NOT EXISTS varroa.%I PARTITION OF varroa.activity FOR VALUES FROM (%L) TO (%L)',
            'activity_p' || suffix, month_start, month_start + interval '1 month'
        );
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down

DROP TABLE IF EXISTS varroa.activity;
