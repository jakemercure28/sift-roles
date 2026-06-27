-- +goose Up
--
-- Postgres baseline: the Postgres-dialect equivalent of the SQLite baseline
-- (migrations/sqlite/00001_baseline.sql), with multi-tenant user_id columns baked
-- in (hosted Postgres is multi-tenant from the start). Every table carries a
-- user_id; tables whose natural key repeats across users (metadata.key,
-- api_usage(date,model), company_notes.company, rejection_email_log(uid...))
-- fold user_id into their key. Job-scoped tables keep the globally-unique job id
-- as their key and just add user_id for filtering, so foreign keys are unchanged.
--
-- Timestamp columns stay TEXT (not timestamptz) and store the same
-- 'YYYY-MM-DD HH:MM:SS' UTC strings SQLite writes, so the Go layer's string-based
-- timestamp handling is identical across backends. The dialect rewriter
-- (dialect.go) emits matching TEXT expressions for datetime('now') et al.

CREATE TABLE IF NOT EXISTS jobs (
  user_id               TEXT NOT NULL DEFAULT 'local',
  id                    TEXT PRIMARY KEY,
  title                 TEXT,
  company               TEXT,
  url                   TEXT,
  platform              TEXT,
  location              TEXT,
  posted_at             TEXT,
  scraped_at            TEXT,
  description           TEXT,
  score                 INTEGER,
  reasoning             TEXT,
  outreach              TEXT,
  status                TEXT DEFAULT 'pending',
  created_at            TEXT DEFAULT (to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS')),
  updated_at            TEXT DEFAULT (to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS')),
  first_seen_at         TEXT DEFAULT (to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS')),
  applied_at            TEXT,
  stage                 TEXT,
  notes                 TEXT,
  reached_out_at        TEXT,
  interview_notes       TEXT,
  apply_complexity      TEXT,
  rejected_from_stage   TEXT,
  rejected_at           TEXT,
  claude_score          INTEGER,
  claude_reasoning      TEXT,
  rejection_reasoning   TEXT,
  score_attempts        INTEGER DEFAULT 0,
  last_score_attempt_at TEXT,
  score_error           TEXT
);
CREATE INDEX IF NOT EXISTS idx_jobs_user        ON jobs(user_id);
CREATE INDEX IF NOT EXISTS idx_jobs_status      ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_jobs_score       ON jobs(score);
CREATE INDEX IF NOT EXISTS idx_jobs_stage       ON jobs(stage);
CREATE INDEX IF NOT EXISTS idx_jobs_applied_at  ON jobs(applied_at);
CREATE INDEX IF NOT EXISTS idx_jobs_created_at  ON jobs(created_at);
CREATE INDEX IF NOT EXISTS idx_jobs_rejected_at ON jobs(rejected_at);

CREATE TABLE IF NOT EXISTS metadata (
  user_id    TEXT NOT NULL DEFAULT 'local',
  key        TEXT NOT NULL,
  value      TEXT,
  updated_at TEXT DEFAULT (to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS')),
  PRIMARY KEY (user_id, key)
);

CREATE TABLE IF NOT EXISTS api_usage (
  user_id    TEXT NOT NULL DEFAULT 'local',
  date       TEXT NOT NULL,
  model      TEXT NOT NULL,
  call_count INTEGER DEFAULT 0,
  PRIMARY KEY (user_id, date, model)
);

CREATE TABLE IF NOT EXISTS company_notes (
  user_id    TEXT NOT NULL DEFAULT 'local',
  company    TEXT NOT NULL,
  tags       TEXT,
  notes      TEXT,
  updated_at TEXT DEFAULT (to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS')),
  PRIMARY KEY (user_id, company)
);

CREATE TABLE IF NOT EXISTS events (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id    TEXT NOT NULL DEFAULT 'local',
  job_id     TEXT NOT NULL,
  event_type TEXT NOT NULL,
  from_value TEXT,
  to_value   TEXT,
  created_at TEXT DEFAULT (to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS')),
  FOREIGN KEY (job_id) REFERENCES jobs(id)
);
CREATE INDEX IF NOT EXISTS idx_events_user    ON events(user_id);
CREATE INDEX IF NOT EXISTS idx_events_job     ON events(job_id);
CREATE INDEX IF NOT EXISTS idx_events_type    ON events(event_type);
CREATE INDEX IF NOT EXISTS idx_events_created ON events(created_at);

CREATE TABLE IF NOT EXISTS rejection_email_log (
  id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id          TEXT NOT NULL DEFAULT 'local',
  mailbox          TEXT NOT NULL,
  uid_validity     TEXT,
  uid              INTEGER NOT NULL,
  message_id       TEXT,
  received_at      TEXT,
  from_address     TEXT,
  subject          TEXT,
  company_hint     TEXT,
  title_hint       TEXT,
  matched_job_id   TEXT,
  match_confidence TEXT,
  match_status     TEXT NOT NULL,
  reason           TEXT,
  created_at       TEXT DEFAULT (to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS')),
  FOREIGN KEY (matched_job_id) REFERENCES jobs(id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_rejection_email_mailbox_uid
  ON rejection_email_log(user_id, mailbox, uid_validity, uid);
CREATE INDEX IF NOT EXISTS idx_rejection_email_status
  ON rejection_email_log(match_status);
CREATE INDEX IF NOT EXISTS idx_rejection_email_job
  ON rejection_email_log(matched_job_id);

CREATE TABLE IF NOT EXISTS job_aliases (
  user_id           TEXT NOT NULL DEFAULT 'local',
  alternate_job_id  TEXT PRIMARY KEY,
  canonical_job_id  TEXT,
  original_platform TEXT,
  original_url      TEXT,
  resolved_platform TEXT,
  resolved_url      TEXT,
  status            TEXT NOT NULL,
  confidence        DOUBLE PRECISION,
  evidence_json     TEXT,
  created_at        TEXT DEFAULT (to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS')),
  updated_at        TEXT DEFAULT (to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS')),
  FOREIGN KEY (alternate_job_id) REFERENCES jobs(id),
  FOREIGN KEY (canonical_job_id) REFERENCES jobs(id)
);
CREATE INDEX IF NOT EXISTS idx_job_aliases_user      ON job_aliases(user_id);
CREATE INDEX IF NOT EXISTS idx_job_aliases_canonical ON job_aliases(canonical_job_id);
CREATE INDEX IF NOT EXISTS idx_job_aliases_status    ON job_aliases(status);

CREATE TABLE IF NOT EXISTS status_snapshots (
  id           BIGINT   GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id      TEXT     NOT NULL DEFAULT 'local',
  recorded_at  TEXT     NOT NULL DEFAULT (to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD"T"HH24:MI:SS"Z"')),
  pending      INTEGER  NOT NULL DEFAULT 0,
  applied      INTEGER  NOT NULL DEFAULT 0,
  interviewing INTEGER  NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_status_snapshots_user        ON status_snapshots(user_id);
CREATE INDEX IF NOT EXISTS idx_status_snapshots_recorded_at ON status_snapshots(recorded_at);

CREATE TABLE IF NOT EXISTS ats_resolution_cache (
  user_id   TEXT NOT NULL DEFAULT 'local',
  job_id    TEXT PRIMARY KEY,
  outcome   TEXT NOT NULL,
  cached_at TEXT DEFAULT (to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS'))
);

-- +goose Down
DROP TABLE IF EXISTS ats_resolution_cache;
DROP TABLE IF EXISTS status_snapshots;
DROP TABLE IF EXISTS job_aliases;
DROP TABLE IF EXISTS rejection_email_log;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS company_notes;
DROP TABLE IF EXISTS api_usage;
DROP TABLE IF EXISTS metadata;
DROP TABLE IF EXISTS jobs;
