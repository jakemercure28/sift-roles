-- +goose Up
--
-- Row-level security policies for the hosted multi-tenant backend.
--
-- These policies are intentionally DORMANT for now. The backend connects to
-- Postgres as a single privileged role (the Supabase service role, which has
-- BYPASSRLS), so ENABLE-only leaves every current query unaffected while the
-- policies sit ready in the schema. The actual tenant guarantee today is the
-- application layer: every query is scoped with `WHERE user_id = ?` (Phase 2,
-- proven by the two-user isolation test).
--
-- Note ENABLE, not FORCE: FORCE would apply RLS even to the table owner/service
-- role and, with `app.user_id` unset, would return zero rows and reject every
-- insert — breaking the app. Enforcement is flipped on deliberately by setting
-- RLS_ENFORCE=true plus RLS_DATABASE_URL (a restricted, non-BYPASSRLS role) in the
-- environment: the dashboard then serves each request through that role inside a
-- transaction that runs `set_config('app.user_id', '<sub>', true)` first (see
-- Repository.exec/query/queryRow/inTx and dashboardRepo in cmd/server). The
-- service-role connection the crons use stays BYPASSRLS so cross-tenant fan-out
-- and migrations keep working. See docs/DEPLOY-thinkpad.md for the role + flip
-- runbook. At that point each policy below matches the row's user_id against the GUC.
--
-- `current_setting('app.user_id', true)` uses the missing_ok=true form so it
-- returns NULL (rather than erroring) when the GUC is unset, which is the case
-- on the bypass-capable connection used today.

-- +goose StatementBegin
DO $$
DECLARE
  t text;
BEGIN
  FOREACH t IN ARRAY ARRAY[
    'jobs', 'metadata', 'api_usage', 'company_notes', 'events',
    'rejection_email_log', 'job_aliases', 'status_snapshots',
    'ats_resolution_cache'
  ]
  LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON %I '
      || 'USING (user_id = current_setting(''app.user_id'', true)) '
      || 'WITH CHECK (user_id = current_setting(''app.user_id'', true))', t);
  END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
DECLARE
  t text;
BEGIN
  FOREACH t IN ARRAY ARRAY[
    'jobs', 'metadata', 'api_usage', 'company_notes', 'events',
    'rejection_email_log', 'job_aliases', 'status_snapshots',
    'ats_resolution_cache'
  ]
  LOOP
    EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', t);
    EXECUTE format('ALTER TABLE %I DISABLE ROW LEVEL SECURITY', t);
  END LOOP;
END
$$;
-- +goose StatementEnd
