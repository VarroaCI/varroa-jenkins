-- Migration 00010 widens "log_chunks_object_class_check" to accept the
-- object class "state". This migration adds no table, so the table set
-- stays twelve.
--
-- A run-state object holds one file of a running Pipeline build, so the
-- pipeline mesh can resume that build on another cell.

-- +goose Up

ALTER TABLE varroa.log_chunks DROP CONSTRAINT log_chunks_object_class_check;
ALTER TABLE varroa.log_chunks ADD CONSTRAINT log_chunks_object_class_check
    CHECK (object_class IN ('log', 'artifact', 'state'));

-- +goose Down
ALTER TABLE varroa.log_chunks DROP CONSTRAINT log_chunks_object_class_check;
ALTER TABLE varroa.log_chunks ADD CONSTRAINT log_chunks_object_class_check
    CHECK (object_class IN ('log', 'artifact'));
