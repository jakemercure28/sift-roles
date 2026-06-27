-- +goose Up
--
-- Multi-tenant user_id columns for the SQLite self-host backend. Self-host is
-- single-tenant: every existing and new row is owned by 'local', so the original
-- primary keys still hold (no collisions with one user) and we only add the
-- user_id column plus the composite unique indexes the shared upserts target
-- (ON CONFLICT(user_id, ...)). Hosted multi-tenancy lives in the Postgres track,
-- which folds user_id into the primary keys directly.

ALTER TABLE jobs                ADD COLUMN user_id TEXT NOT NULL DEFAULT 'local';
ALTER TABLE metadata            ADD COLUMN user_id TEXT NOT NULL DEFAULT 'local';
ALTER TABLE api_usage           ADD COLUMN user_id TEXT NOT NULL DEFAULT 'local';
ALTER TABLE company_notes       ADD COLUMN user_id TEXT NOT NULL DEFAULT 'local';
ALTER TABLE events              ADD COLUMN user_id TEXT NOT NULL DEFAULT 'local';
ALTER TABLE rejection_email_log ADD COLUMN user_id TEXT NOT NULL DEFAULT 'local';
ALTER TABLE job_aliases         ADD COLUMN user_id TEXT NOT NULL DEFAULT 'local';
ALTER TABLE status_snapshots    ADD COLUMN user_id TEXT NOT NULL DEFAULT 'local';
ALTER TABLE ats_resolution_cache ADD COLUMN user_id TEXT NOT NULL DEFAULT 'local';

-- Composite unique indexes so the shared ON CONFLICT(user_id, <natural key>)
-- upserts resolve to an index. With one user these are equivalent to the
-- original single-column keys.
CREATE UNIQUE INDEX IF NOT EXISTS idx_metadata_user_key
  ON metadata(user_id, key);
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_usage_user_date_model
  ON api_usage(user_id, date, model);
CREATE UNIQUE INDEX IF NOT EXISTS idx_company_notes_user_company
  ON company_notes(user_id, company);
-- rejection_email_log keeps its original single-tenant unique index
-- (mailbox, uid_validity, uid): the rejectionsync package still upserts against
-- that target and, on self-host SQLite, is single-tenant. Its full tenant-scoping
-- (and the matching user-scoped unique index) lands with the Postgres rejection
-- sync work. A user-scoped index is added here too for forward parity.
CREATE UNIQUE INDEX IF NOT EXISTS idx_rejection_email_user_mailbox_uid
  ON rejection_email_log(user_id, mailbox, uid_validity, uid);

-- Filtering indexes mirroring the Postgres track.
CREATE INDEX IF NOT EXISTS idx_jobs_user        ON jobs(user_id);
CREATE INDEX IF NOT EXISTS idx_events_user       ON events(user_id);
CREATE INDEX IF NOT EXISTS idx_job_aliases_user  ON job_aliases(user_id);
CREATE INDEX IF NOT EXISTS idx_status_snapshots_user ON status_snapshots(user_id);

-- +goose Down
DROP INDEX IF EXISTS idx_status_snapshots_user;
DROP INDEX IF EXISTS idx_job_aliases_user;
DROP INDEX IF EXISTS idx_events_user;
DROP INDEX IF EXISTS idx_jobs_user;
DROP INDEX IF EXISTS idx_rejection_email_user_mailbox_uid;
DROP INDEX IF EXISTS idx_company_notes_user_company;
DROP INDEX IF EXISTS idx_api_usage_user_date_model;
DROP INDEX IF EXISTS idx_metadata_user_key;
ALTER TABLE ats_resolution_cache DROP COLUMN user_id;
ALTER TABLE status_snapshots    DROP COLUMN user_id;
ALTER TABLE job_aliases         DROP COLUMN user_id;
ALTER TABLE rejection_email_log DROP COLUMN user_id;
ALTER TABLE events              DROP COLUMN user_id;
ALTER TABLE company_notes       DROP COLUMN user_id;
ALTER TABLE api_usage           DROP COLUMN user_id;
ALTER TABLE metadata            DROP COLUMN user_id;
ALTER TABLE jobs                DROP COLUMN user_id;
