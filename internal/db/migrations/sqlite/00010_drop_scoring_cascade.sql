-- +goose Up
--
-- Remove the Gemma triage cascade: scoring is Flash-only again. Re-queue any jobs
-- left mid-cascade (triage promoted them but the confirm model never scored them,
-- so their public score is only a provisional triage value) by clearing the score
-- so the Flash path re-scores them, then drop the cascade provenance columns.
UPDATE jobs SET score = NULL, reasoning = NULL WHERE confirm_pending = 1;
DROP INDEX IF EXISTS idx_jobs_confirm_pending;
ALTER TABLE jobs DROP COLUMN confirm_pending;
ALTER TABLE jobs DROP COLUMN confirmed_scored_at;
ALTER TABLE jobs DROP COLUMN confirmed_model;
ALTER TABLE jobs DROP COLUMN confirmed_reasoning;
ALTER TABLE jobs DROP COLUMN confirmed_score;
ALTER TABLE jobs DROP COLUMN triage_scored_at;
ALTER TABLE jobs DROP COLUMN triage_model;
ALTER TABLE jobs DROP COLUMN triage_reasoning;
ALTER TABLE jobs DROP COLUMN triage_score;

-- +goose Down
ALTER TABLE jobs ADD COLUMN triage_score INTEGER;
ALTER TABLE jobs ADD COLUMN triage_reasoning TEXT;
ALTER TABLE jobs ADD COLUMN triage_model TEXT;
ALTER TABLE jobs ADD COLUMN triage_scored_at TEXT;
ALTER TABLE jobs ADD COLUMN confirmed_score INTEGER;
ALTER TABLE jobs ADD COLUMN confirmed_reasoning TEXT;
ALTER TABLE jobs ADD COLUMN confirmed_model TEXT;
ALTER TABLE jobs ADD COLUMN confirmed_scored_at TEXT;
ALTER TABLE jobs ADD COLUMN confirm_pending INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_jobs_confirm_pending ON jobs (user_id, confirm_pending) WHERE confirm_pending = 1;
