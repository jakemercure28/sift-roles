-- +goose Up
--
-- One-time backfill of the forward-only global_jobs cache from jobs already
-- scraped. global_jobs (00006) only harvests on NEW scrapes, so without this a
-- freshly deployed environment starts with a near-empty cache and new tenants
-- can only be seeded from post-deploy harvests instead of the full market
-- history. Idempotent (ON CONFLICT DO NOTHING); a no-op on a fresh/empty jobs
-- table. DISTINCT ON keeps the richest (longest-description) copy when several
-- tenants scraped the same posting.
INSERT INTO global_jobs
  (id, title, company, url, platform, location, posted_at, scraped_at, description, first_seen_by, first_seen_at, last_seen_at)
SELECT DISTINCT ON (global_job_id)
  global_job_id, title, company, url, platform, location, posted_at, scraped_at,
  description, user_id,
  to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS'),
  to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS')
FROM jobs
WHERE global_job_id IS NOT NULL
  AND coalesce(url, '') <> ''
  AND coalesce(description, '') <> ''
  AND lower(coalesce(platform, '')) NOT IN ('linkedin', 'manual')
ORDER BY global_job_id, length(coalesce(description, '')) DESC
ON CONFLICT (id) DO NOTHING;

-- +goose Down
-- Data-only backfill; intentionally not reversed. Backfilled and harvested rows
-- are indistinguishable, and global_jobs is a rebuildable cache.
SELECT 1;
