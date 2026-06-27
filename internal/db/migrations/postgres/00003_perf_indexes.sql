-- +goose Up
-- +goose NO TRANSACTION
--
-- Composite indexes for the hosted Postgres path. The baseline indexes were
-- mostly single-column SQLite carryovers; hosted queries nearly always combine
-- user_id with a dashboard filter, sort, or background-worker predicate.
-- CONCURRENTLY keeps a live beta database readable/writable while the indexes
-- build.

-- Dashboard list views and Market Research signatures.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_jobs_user_score_posted_created
  ON jobs(user_id, score DESC, posted_at DESC, created_at DESC);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_jobs_user_created_desc
  ON jobs(user_id, created_at DESC);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_jobs_user_updated_desc
  ON jobs(user_id, updated_at DESC);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_jobs_user_status_score_created
  ON jobs(user_id, status, score DESC, posted_at DESC, created_at DESC);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_jobs_user_stage_updated
  ON jobs(user_id, stage, updated_at DESC);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_jobs_user_applied_at
  ON jobs(user_id, applied_at);

-- Scoring queue: pending unscored jobs, never-attempted first, then oldest
-- attempt, then oldest row.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_jobs_unscored_pending_order
  ON jobs(
    user_id,
    (CASE WHEN last_score_attempt_at IS NULL THEN 0 ELSE 1 END),
    last_score_attempt_at ASC,
    created_at ASC
  )
  WHERE score IS NULL AND status = 'pending';

-- Dedup and scrape pre-filtering compare normalized title/company within a
-- tenant. This turns the correlated duplicate checks from repeated scans into
-- indexed lookups.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_jobs_user_title_company_status_created
  ON jobs(user_id, lower(trim(title)), lower(trim(company)), status, created_at DESC);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_jobs_user_platform_title_company
  ON jobs(user_id, lower(coalesce(platform, '')), lower(trim(title)), lower(trim(company)));

-- Analytics and activity feeds are event-heavy once users start moving jobs
-- through the pipeline.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_events_user_created_desc
  ON events(user_id, created_at DESC);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_events_user_type_to_job
  ON events(user_id, event_type, to_value, job_id);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_events_user_job_created
  ON events(user_id, job_id, created_at DESC);

-- Rejection sync status and insight queries.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_rejection_email_user_status_created
  ON rejection_email_log(user_id, match_status, created_at DESC);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_rejection_email_user_matched_job
  ON rejection_email_log(user_id, matched_job_id);

ANALYZE jobs;
ANALYZE events;
ANALYZE rejection_email_log;

-- +goose Down
-- +goose NO TRANSACTION
DROP INDEX CONCURRENTLY IF EXISTS idx_rejection_email_user_matched_job;
DROP INDEX CONCURRENTLY IF EXISTS idx_rejection_email_user_status_created;
DROP INDEX CONCURRENTLY IF EXISTS idx_events_user_job_created;
DROP INDEX CONCURRENTLY IF EXISTS idx_events_user_type_to_job;
DROP INDEX CONCURRENTLY IF EXISTS idx_events_user_created_desc;
DROP INDEX CONCURRENTLY IF EXISTS idx_jobs_user_platform_title_company;
DROP INDEX CONCURRENTLY IF EXISTS idx_jobs_user_title_company_status_created;
DROP INDEX CONCURRENTLY IF EXISTS idx_jobs_unscored_pending_order;
DROP INDEX CONCURRENTLY IF EXISTS idx_jobs_user_applied_at;
DROP INDEX CONCURRENTLY IF EXISTS idx_jobs_user_stage_updated;
DROP INDEX CONCURRENTLY IF EXISTS idx_jobs_user_status_score_created;
DROP INDEX CONCURRENTLY IF EXISTS idx_jobs_user_updated_desc;
DROP INDEX CONCURRENTLY IF EXISTS idx_jobs_user_created_desc;
DROP INDEX CONCURRENTLY IF EXISTS idx_jobs_user_score_posted_created;
