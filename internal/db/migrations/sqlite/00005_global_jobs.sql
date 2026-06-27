-- +goose Up
--
-- GLOBAL, cross-tenant cache of scraped job descriptions. Tenant rows in jobs
-- remain isolated and hold score/status/application state; this table stores the
-- tenant-independent market fact so a first-run tenant can be seeded from recent
-- known jobs without waiting for discovery + scraper bootstrap.
ALTER TABLE jobs ADD COLUMN global_job_id TEXT;
-- Backfill existing rows so the (user_id, global_job_id) index dedups a re-scrape
-- of an already-known posting. Without this, a row inserted before this column
-- existed (global_job_id NULL) would not match the new tenant-scoped row id a
-- later scrape computes, silently duplicating every still-open job. jobs.id is a
-- global primary key, so id is unique per tenant and the backfill never collides.
UPDATE jobs SET global_job_id = id WHERE global_job_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_user_global_job
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
  first_seen_at         TEXT DEFAULT (datetime('now')),
  last_seen_at          TEXT DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_global_jobs_seen
  ON global_jobs(last_seen_at DESC);
CREATE INDEX IF NOT EXISTS idx_global_jobs_company_platform
  ON global_jobs(lower(trim(company)), lower(coalesce(platform, '')), posted_at DESC);

-- +goose Down
DROP TABLE IF EXISTS global_jobs;
DROP INDEX IF EXISTS idx_jobs_user_global_job;
ALTER TABLE jobs DROP COLUMN global_job_id;
