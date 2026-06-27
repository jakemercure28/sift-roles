-- +goose Up
--
-- Audit column: records *why* a row was auto-archived (which dedup pass,
-- canonicalize, or low score). NULL means a user archived it (or the row
-- predates this column). Mirrors the Postgres track; makes every automatic
-- archive attributable so the dedup-cascade class of bug is queryable.
ALTER TABLE jobs ADD COLUMN archive_reason TEXT;

-- +goose Down
ALTER TABLE jobs DROP COLUMN archive_reason;
