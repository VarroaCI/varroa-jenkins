-- Migration 00026 adds "writer" to "varroa.log_chunks". This migration
-- adds no table, so the table set stays thirteen.
--
-- "writer" names the writer of a log part: the namespace, the cell, and
-- the cell instance of the sender. The gateway writes it with each part.
-- Restart recovery stops at a part whose writer differs from the writer
-- of the highest index entry. A part that a lost cell's write lands after
-- a mesh takeover therefore never becomes a recovered entry.
--
-- The column is not null with an empty default, so a row that names no
-- writer reads as "". It carries no index: a reader reads it from a row it
-- already found by its key and sequence.

-- +goose Up

ALTER TABLE varroa.log_chunks ADD COLUMN writer text NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE varroa.log_chunks DROP COLUMN IF EXISTS writer;
