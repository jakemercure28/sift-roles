-- +goose Up
-- +goose NO TRANSACTION
--
-- Composite indexes for the dashboard fragment routes. Hosted page hydration
-- waits on /api/dashboard-list, so cold misses need to avoid tenant scans and
-- temp sorts for the default Jobs page and Analytics page.

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_jobs_dash_not_applied_score_page
  ON jobs(user_id, (score IS NULL), score DESC, posted_at DESC, created_at DESC)
  WHERE status NOT IN ('applied','responded','archived','closed','ghosted')
    AND COALESCE(stage, '') NOT IN ('closed','rejected','ghosted');

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_jobs_dash_active_score_page
  ON jobs(user_id, (score IS NULL), score DESC, posted_at DESC, created_at DESC)
  WHERE status NOT IN ('archived','rejected','closed','ghosted');

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_events_stage_user_job_created_id
  ON events(user_id, job_id, created_at, id)
  WHERE event_type = 'stage_change';

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_events_stage_user_to_job
  ON events(user_id, to_value, job_id)
  WHERE event_type = 'stage_change';

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_events_user_to_created_job
  ON events(user_id, to_value, created_at DESC, job_id);

ANALYZE jobs;
ANALYZE events;

-- +goose Down
-- +goose NO TRANSACTION
DROP INDEX CONCURRENTLY IF EXISTS idx_events_user_to_created_job;
DROP INDEX CONCURRENTLY IF EXISTS idx_events_stage_user_to_job;
DROP INDEX CONCURRENTLY IF EXISTS idx_events_stage_user_job_created_id;
DROP INDEX CONCURRENTLY IF EXISTS idx_jobs_dash_active_score_page;
DROP INDEX CONCURRENTLY IF EXISTS idx_jobs_dash_not_applied_score_page;
