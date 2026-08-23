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
  created_at  TEXT DEFAULT (datetime('now')),
  updated_at  TEXT DEFAULT (datetime('now')),
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

-- +goose Down
DROP TABLE IF EXISTS score_batch_jobs;
DROP TABLE IF EXISTS score_batches;
