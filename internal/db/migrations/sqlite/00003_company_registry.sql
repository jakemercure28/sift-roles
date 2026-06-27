-- +goose Up
--
-- Global, cross-tenant cache of verified ATS boards. See the postgres track's
-- 00004_company_registry.sql for the rationale; on self-host SQLite there is a
-- single tenant, so this simply memoizes verification within that one profile.
CREATE TABLE IF NOT EXISTS company_registry (
  platform         TEXT NOT NULL,
  registry_key     TEXT NOT NULL,
  slug             TEXT,
  board_url        TEXT,
  label            TEXT,
  wd               INTEGER,
  board            TEXT,
  first_seen_by    TEXT,
  verified_at      TEXT DEFAULT (datetime('now')),
  last_verified_at TEXT DEFAULT (datetime('now')),
  PRIMARY KEY (platform, registry_key)
);
CREATE INDEX IF NOT EXISTS idx_company_registry_verified ON company_registry(last_verified_at);

-- +goose Down
DROP TABLE IF EXISTS company_registry;
