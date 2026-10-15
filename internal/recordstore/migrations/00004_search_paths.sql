-- Migration 00004 corrects the job-path search expression on
-- "job_schedules" and "runs" in schema "varroa". Postgres's "simple"
-- parser keeps a "/"-joined segment as one lexeme, so a bare "job_path"
-- column never lets a prefix query match a segment after the first "/".
-- This migration replaces "/" with a space before tokenizing, so every
-- segment becomes its own lexeme.
--
-- Neither index changes name: only the indexed expression changes.

-- +goose Up

DROP INDEX varroa.job_schedules_search_tsv_idx;
DROP INDEX varroa.runs_search_tsv_idx;

CREATE INDEX job_schedules_search_tsv_idx ON varroa.job_schedules USING GIN (
    to_tsvector('simple', replace(job_path, '/', ' '))
);

CREATE INDEX runs_search_tsv_idx ON varroa.runs USING GIN (
    to_tsvector('simple', replace(job_path, '/', ' ')||' '||coalesce(branch,'')||' '||coalesce(actor,'')||' '||coalesce(build_number::text,''))
);

-- +goose Down

DROP INDEX varroa.job_schedules_search_tsv_idx;
DROP INDEX varroa.runs_search_tsv_idx;

CREATE INDEX job_schedules_search_tsv_idx ON varroa.job_schedules USING GIN (to_tsvector('simple', job_path));

CREATE INDEX runs_search_tsv_idx ON varroa.runs USING GIN (
    to_tsvector('simple', job_path||' '||coalesce(branch,'')||' '||coalesce(actor,'')||' '||coalesce(build_number::text,''))
);
