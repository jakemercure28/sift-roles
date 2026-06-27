-- +goose Up
--
-- company_registry is a GLOBAL, cross-tenant cache of verified ATS boards. A
-- verified board ("acme is on greenhouse at slug acme") is a tenant-independent
-- fact, so unlike every other table it has NO user_id column and is deliberately
-- excluded from 00002_rls.sql. Discovery harvests freshly verified boards into it
-- and trusts recent rows to skip redundant HTTP verification across tenants.
CREATE TABLE IF NOT EXISTS company_registry (
  platform         TEXT NOT NULL,
  registry_key     TEXT NOT NULL,
  slug             TEXT,
  board_url        TEXT,
  label            TEXT,
  wd               INTEGER,
  board            TEXT,
  first_seen_by    TEXT,
  verified_at      TEXT DEFAULT (to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS')),
  last_verified_at TEXT DEFAULT (to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS')),
  PRIMARY KEY (platform, registry_key)
);
CREATE INDEX IF NOT EXISTS idx_company_registry_verified ON company_registry(last_verified_at);

-- Grant DML to the restricted RLS role so the RLS-enforced request/discovery path
-- can read and harvest into this (non-RLS) global table. ALTER DEFAULT PRIVILEGES
-- in the deploy runbook should already cover future tables, but this explicit,
-- role-guarded grant is belt-and-suspenders: a missing grant would silently break
-- discovery once RLS_ENFORCE is on. No-op where the role does not exist (fresh DB,
-- scratch test instance) so the migration still applies everywhere.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_rls') THEN
    GRANT SELECT, INSERT, UPDATE ON company_registry TO app_rls;
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS company_registry;
