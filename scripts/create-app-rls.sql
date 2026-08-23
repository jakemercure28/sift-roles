-- Create the restricted `app_rls` role used by the dashboard serving path when
-- RLS_ENFORCE=true (see docs/DEPLOY-thinkpad.md section 5 and dashboardRepo() in
-- cmd/server/main.go). The role is NOBYPASSRLS and gets DML only, never DDL, so it
-- cannot run migrations; the service role (DATABASE_URL) keeps doing that.
--
-- Run as the table owner / superuser (e.g. `postgres`). Pass the password as a psql
-- variable so it is never hard-coded here:
--
--   docker exec -i supabase-db-prod \
--     psql -U postgres -d postgres -v ON_ERROR_STOP=1 -v pw="$APP_RLS_PW" \
--     -f - < scripts/create-app-rls.sql
--
-- On self-hosted (co-located) Postgres the matching DSN is:
--   RLS_DATABASE_URL=postgresql://app_rls:<pw>@supabase-db-prod:5432/postgres?sslmode=disable
-- which keeps the dashboard read path on the local Docker network (no pooler egress).

CREATE ROLE app_rls LOGIN PASSWORD :'pw' NOBYPASSRLS;
GRANT USAGE ON SCHEMA public TO app_rls;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_rls;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_rls;
-- so future migrations' tables/sequences are covered without re-granting:
ALTER DEFAULT PRIVILEGES IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_rls;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO app_rls;
