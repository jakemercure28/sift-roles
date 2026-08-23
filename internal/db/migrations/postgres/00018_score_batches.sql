-- +goose Up
--
-- Gemini Batch API scoring state. A batch is submitted with up to a few
-- hundred unscored jobs (score_batch_jobs holds the membership) and polled by
-- the batch-score loop until it reaches a terminal state; keeping all state in
-- these tables means the poller survives restarts and deploys mid-flight.
-- Members of an OPEN ('submitted') batch are excluded from the sync unscored
-- queue so free-tier quota is never spent on a job the batch will score
-- anyway; closing a batch (any terminal state) deletes its member rows, which
-- returns unapplied jobs to the pool automatically.
CREATE TABLE IF NOT EXISTS score_batches (
  user_id     TEXT NOT NULL,
  batch_name  TEXT NOT NULL,
  state       TEXT NOT NULL DEFAULT 'submitted',
  model       TEXT NOT NULL,
  resume_hash TEXT,
  job_count   INTEGER NOT NULL DEFAULT 0,
  error       TEXT,
  created_at  TEXT DEFAULT to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS'),
  updated_at  TEXT DEFAULT to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS'),
  PRIMARY KEY (user_id, batch_name)
);
CREATE INDEX IF NOT EXISTS idx_score_batches_state ON score_batches (state);

CREATE TABLE IF NOT EXISTS score_batch_jobs (
  user_id    TEXT NOT NULL,
  batch_name TEXT NOT NULL,
  job_id     TEXT NOT NULL,
  PRIMARY KEY (user_id, batch_name, job_id)
);
CREATE INDEX IF NOT EXISTS idx_score_batch_jobs_user_job ON score_batch_jobs (user_id, job_id);

-- Same dormant tenant-isolation policies as 00002_rls.sql: ENABLE (not FORCE)
-- so the service role's cron access is unaffected until RLS_ENFORCE flips the
-- restricted role on.
-- +goose StatementBegin
DO $$
DECLARE
  t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['score_batches', 'score_batch_jobs']
  LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON %I '
      || 'USING (user_id = current_setting(''app.user_id'', true)) '
      || 'WITH CHECK (user_id = current_setting(''app.user_id'', true))', t);
  END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS score_batch_jobs;
DROP TABLE IF EXISTS score_batches;
