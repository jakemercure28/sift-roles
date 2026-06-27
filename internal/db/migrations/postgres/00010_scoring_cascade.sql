-- +goose Up
--
-- Two-stage scoring cascade (Gemma triage -> Flash confirm). The cheap triage
-- model scores every job; only jobs it rates highly (or a title-signal rescue)
-- are re-scored by the expensive confirm model, which stays authoritative. These
-- columns keep each stage's score/reasoning/model separately so the two can be
-- compared and the promote threshold tuned on real paired data later.
--
-- score/reasoning remain the FINAL authoritative values (confirmed when a confirm
-- pass ran, else the provisional triage score), so every existing query and the
-- dashboard keep working unchanged.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS triage_score INTEGER;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS triage_reasoning TEXT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS triage_model TEXT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS triage_scored_at TEXT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS confirmed_score INTEGER;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS confirmed_reasoning TEXT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS confirmed_model TEXT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS confirmed_scored_at TEXT;
-- confirm_pending = 1 marks a job that triage promoted but the confirm model has
-- not scored yet, so a crashed run resumes the confirm pass without re-triaging.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS confirm_pending INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_jobs_confirm_pending ON jobs (user_id, confirm_pending) WHERE confirm_pending = 1;

-- +goose Down
DROP INDEX IF EXISTS idx_jobs_confirm_pending;
ALTER TABLE jobs DROP COLUMN IF EXISTS confirm_pending;
ALTER TABLE jobs DROP COLUMN IF EXISTS confirmed_scored_at;
ALTER TABLE jobs DROP COLUMN IF EXISTS confirmed_model;
ALTER TABLE jobs DROP COLUMN IF EXISTS confirmed_reasoning;
ALTER TABLE jobs DROP COLUMN IF EXISTS confirmed_score;
ALTER TABLE jobs DROP COLUMN IF EXISTS triage_scored_at;
ALTER TABLE jobs DROP COLUMN IF EXISTS triage_model;
ALTER TABLE jobs DROP COLUMN IF EXISTS triage_reasoning;
ALTER TABLE jobs DROP COLUMN IF EXISTS triage_score;
