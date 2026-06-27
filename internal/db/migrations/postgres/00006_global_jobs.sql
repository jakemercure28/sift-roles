-- +goose Up
-- +goose NO TRANSACTION
--
-- GLOBAL, cross-tenant cache of scraped job descriptions. Tenant rows in jobs
-- remain isolated and hold score/status/application state; this table stores the
-- tenant-independent market fact so a first-run tenant can be seeded from recent
-- known jobs without waiting for discovery + scraper bootstrap.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS global_job_id TEXT;
-- Backfill existing rows so the (user_id, global_job_id) index dedups a re-scrape
-- of an already-known posting. Without this, a row inserted before this column
-- existed (global_job_id NULL) would not match the new tenant-scoped row id a
-- later scrape computes, silently duplicating every still-open job for existing
-- tenants. jobs.id is a global primary key, so id is unique per tenant and the
-- backfill never collides with the partial unique index created below.
UPDATE jobs SET global_job_id = id WHERE global_job_id IS NULL;
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_jobs_user_global_job
  ON jobs(user_id, global_job_id)
  WHERE global_job_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS global_jobs (
  id                    TEXT PRIMARY KEY,
  title                 TEXT,
  company               TEXT,
  url                   TEXT,
  platform              TEXT,
  location              TEXT,
  posted_at             TEXT,
  scraped_at            TEXT,
  description           TEXT,
  description_hash      TEXT,
  first_seen_by         TEXT,
  first_seen_at         TEXT DEFAULT (to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS')),
  last_seen_at          TEXT DEFAULT (to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS'))
);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_global_jobs_seen
  ON global_jobs(last_seen_at DESC);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_global_jobs_company_platform
  ON global_jobs(lower(trim(company)), lower(coalesce(platform, '')), posted_at DESC);

-- Grant DML to the restricted RLS role. global_jobs deliberately has no RLS
-- policy because job descriptions are shared market data; tenant-specific state
-- remains in jobs.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_rls') THEN
    GRANT SELECT, INSERT, UPDATE ON global_jobs TO app_rls;
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS global_jobs;
DROP INDEX CONCURRENTLY IF EXISTS idx_jobs_user_global_job;
ALTER TABLE jobs DROP COLUMN IF EXISTS global_job_id;
