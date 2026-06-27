-- +goose Up
--
-- Squashed baseline: the full schema produced by the Node in-code migration
-- system (lib/db/schema.js, BASE_SCHEMA_SQL + ensureCurrentSchema through v27).
-- Every statement uses IF NOT EXISTS so applying this to an already-initialized
-- database is a no-op. On a live DB (already at schema_version >= 27) the Go
-- migrator stamps this version as applied without replay (see migrate.go); fresh
-- databases run it to build the whole schema.

CREATE TABLE IF NOT EXISTS jobs (
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
  created_at            TEXT DEFAULT (datetime('now')),
  updated_at            TEXT DEFAULT (datetime('now')),
  first_seen_at         TEXT DEFAULT (datetime('now')),
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
CREATE INDEX IF NOT EXISTS idx_jobs_status      ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_jobs_score       ON jobs(score);
CREATE INDEX IF NOT EXISTS idx_jobs_stage       ON jobs(stage);
CREATE INDEX IF NOT EXISTS idx_jobs_applied_at  ON jobs(applied_at);
CREATE INDEX IF NOT EXISTS idx_jobs_created_at  ON jobs(created_at);
CREATE INDEX IF NOT EXISTS idx_jobs_rejected_at ON jobs(rejected_at);

CREATE TABLE IF NOT EXISTS metadata (
  key        TEXT PRIMARY KEY,
  value      TEXT,
  updated_at TEXT DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS api_usage (
  date       TEXT NOT NULL,
  model      TEXT NOT NULL,
  call_count INTEGER DEFAULT 0,
  PRIMARY KEY (date, model)
);

CREATE TABLE IF NOT EXISTS company_notes (
  company    TEXT PRIMARY KEY,
  tags       TEXT,
  notes      TEXT,
  updated_at TEXT DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS events (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id     TEXT NOT NULL,
  event_type TEXT NOT NULL,
  from_value TEXT,
  to_value   TEXT,
  created_at TEXT DEFAULT (datetime('now')),
  FOREIGN KEY (job_id) REFERENCES jobs(id)
);
CREATE INDEX IF NOT EXISTS idx_events_job     ON events(job_id);
CREATE INDEX IF NOT EXISTS idx_events_type    ON events(event_type);
CREATE INDEX IF NOT EXISTS idx_events_created ON events(created_at);

CREATE TABLE IF NOT EXISTS rejection_email_log (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
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
  created_at       TEXT DEFAULT (datetime('now')),
  FOREIGN KEY (matched_job_id) REFERENCES jobs(id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_rejection_email_mailbox_uid
  ON rejection_email_log(mailbox, uid_validity, uid);
CREATE INDEX IF NOT EXISTS idx_rejection_email_status
  ON rejection_email_log(match_status);
CREATE INDEX IF NOT EXISTS idx_rejection_email_job
  ON rejection_email_log(matched_job_id);

CREATE TABLE IF NOT EXISTS job_aliases (
  alternate_job_id  TEXT PRIMARY KEY,
  canonical_job_id  TEXT,
  original_platform TEXT,
  original_url      TEXT,
  resolved_platform TEXT,
  resolved_url      TEXT,
  status            TEXT NOT NULL,
  confidence        REAL,
  evidence_json     TEXT,
  created_at        TEXT DEFAULT (datetime('now')),
  updated_at        TEXT DEFAULT (datetime('now')),
  FOREIGN KEY (alternate_job_id) REFERENCES jobs(id),
  FOREIGN KEY (canonical_job_id) REFERENCES jobs(id)
);
CREATE INDEX IF NOT EXISTS idx_job_aliases_canonical ON job_aliases(canonical_job_id);
CREATE INDEX IF NOT EXISTS idx_job_aliases_status    ON job_aliases(status);

CREATE TABLE IF NOT EXISTS status_snapshots (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  recorded_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
  pending      INTEGER NOT NULL DEFAULT 0,
  applied      INTEGER NOT NULL DEFAULT 0,
  interviewing INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_status_snapshots_recorded_at
  ON status_snapshots(recorded_at);

CREATE TABLE IF NOT EXISTS ats_resolution_cache (
  job_id    TEXT PRIMARY KEY,
  outcome   TEXT NOT NULL,
  cached_at TEXT DEFAULT (datetime('now'))
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
