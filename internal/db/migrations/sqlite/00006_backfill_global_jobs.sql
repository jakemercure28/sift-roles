-- +goose Up
--
-- One-time backfill of the forward-only global_jobs cache from jobs already
-- scraped (see the Postgres 00007 twin for rationale). Idempotent via INSERT OR
-- IGNORE; a no-op on a fresh/empty jobs table. The inner GROUP BY with
-- max(length(description)) uses SQLite's bare-columns rule to pick the
-- richest-description row per global_job_id.
INSERT OR IGNORE INTO global_jobs
  (id, title, company, url, platform, location, posted_at, scraped_at, description, first_seen_by, first_seen_at, last_seen_at)
SELECT global_job_id, title, company, url, platform, location, posted_at, scraped_at,
       description, user_id, datetime('now'), datetime('now')
FROM (
  SELECT *, max(length(coalesce(description, ''))) AS _maxlen
  FROM jobs
  WHERE global_job_id IS NOT NULL
    AND coalesce(url, '') <> ''
    AND coalesce(description, '') <> ''
    AND lower(coalesce(platform, '')) NOT IN ('linkedin', 'manual')
  GROUP BY global_job_id
);

-- +goose Down
-- Data-only backfill; intentionally not reversed (global_jobs is a rebuildable cache).
SELECT 1;
