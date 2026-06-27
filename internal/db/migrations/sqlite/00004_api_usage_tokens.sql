-- +goose Up
--
-- The legacy SQLite table kept PRIMARY KEY(date, model) even after 00002 added
-- user_id plus a unique index. That meant the reserved __host__ usage row could
-- collide with the tenant row for the same day/model. Rebuild the table so
-- (user_id, date, model) is the real key, then add token totals from Gemini
-- usageMetadata.
CREATE TABLE api_usage_new (
  user_id              TEXT NOT NULL DEFAULT 'local',
  date                 TEXT NOT NULL,
  model                TEXT NOT NULL,
  call_count           INTEGER DEFAULT 0,
  prompt_tokens        INTEGER NOT NULL DEFAULT 0,
  cached_prompt_tokens INTEGER NOT NULL DEFAULT 0,
  candidate_tokens     INTEGER NOT NULL DEFAULT 0,
  total_tokens         INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, date, model)
);

INSERT INTO api_usage_new (user_id, date, model, call_count)
SELECT COALESCE(NULLIF(user_id, ''), 'local'), date, model, call_count
FROM api_usage;

DROP TABLE api_usage;
ALTER TABLE api_usage_new RENAME TO api_usage;

-- +goose Down
CREATE TABLE api_usage_old (
  date       TEXT NOT NULL,
  model      TEXT NOT NULL,
  call_count INTEGER DEFAULT 0,
  user_id    TEXT NOT NULL DEFAULT 'local',
  PRIMARY KEY (date, model)
);

-- Collapse multi-user rows back into the old single-key shape. Down migration is
-- only for local rollback; the app never relies on this lossy shape going forward.
INSERT INTO api_usage_old (date, model, call_count, user_id)
SELECT date, model, SUM(call_count), 'local'
FROM api_usage
GROUP BY date, model;

DROP TABLE api_usage;
ALTER TABLE api_usage_old RENAME TO api_usage;
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_usage_user_date_model
  ON api_usage(user_id, date, model);
