-- +goose Up
--
-- Two-stage scoring cascade (Gemma triage -> Flash confirm). Mirrors the Postgres
-- track. The cheap triage model scores every job; only jobs it rates highly (or a
-- title-signal rescue) are re-scored by the authoritative confirm model. Each
-- stage's score/reasoning/model is kept separately for later threshold tuning.
-- score/reasoning stay the FINAL authoritative values so existing queries and the
-- dashboard are unchanged.
ALTER TABLE jobs ADD COLUMN triage_score INTEGER;
ALTER TABLE jobs ADD COLUMN triage_reasoning TEXT;
ALTER TABLE jobs ADD COLUMN triage_model TEXT;
ALTER TABLE jobs ADD COLUMN triage_scored_at TEXT;
ALTER TABLE jobs ADD COLUMN confirmed_score INTEGER;
ALTER TABLE jobs ADD COLUMN confirmed_reasoning TEXT;
ALTER TABLE jobs ADD COLUMN confirmed_model TEXT;
ALTER TABLE jobs ADD COLUMN confirmed_scored_at TEXT;
-- confirm_pending = 1 marks a job triage promoted but the confirm model has not
-- scored yet, so a crashed run resumes the confirm pass without re-triaging.
ALTER TABLE jobs ADD COLUMN confirm_pending INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_jobs_confirm_pending ON jobs (user_id, confirm_pending) WHERE confirm_pending = 1;

-- +goose Down
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
