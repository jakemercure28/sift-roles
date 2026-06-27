-- +goose Up
--
-- Audit column: records *why* a row was auto-archived (which dedup pass,
-- canonicalize, or low score). NULL means a user archived it (or the row
-- predates this column). Auto-archives used to be silent overwrites, which is
-- how the dedup passes could delete unique listings to zero rows unnoticed;
-- attributing every automatic archive makes that class of bug queryable.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS archive_reason TEXT;

-- +goose Down
ALTER TABLE jobs DROP COLUMN IF EXISTS archive_reason;
