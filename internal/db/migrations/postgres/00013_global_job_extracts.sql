-- +goose Up
--
-- GLOBAL, cross-tenant cache of LLM-extracted job facts. The scoring rubric's
-- STEP 1 (extract the role's field/function, core work, credentials, skills,
-- min years) is tenant-independent, so it is paid for once per global job here
-- instead of once per tenant per job inside every scoring call. facts is a JSON
-- blob of career-agnostic fields only; all fit judgment stays in the per-tenant
-- scoring call. description_hash guards staleness: when global_jobs gets a
-- richer description the hash stops matching and the row is re-extracted.
-- extract_version lets a prompt/schema change invalidate old extracts in bulk.
CREATE TABLE IF NOT EXISTS global_job_extracts (
  global_job_id    TEXT PRIMARY KEY,
  description_hash TEXT,
  extract_version  INTEGER NOT NULL DEFAULT 1,
  facts            TEXT,
  model            TEXT,
  extracted_at     TEXT DEFAULT (to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS'))
);
-- extracted_at keys the daily extraction budget (rows created today).
CREATE INDEX IF NOT EXISTS idx_global_job_extracts_at
  ON global_job_extracts(extracted_at DESC);

-- Grant DML to the restricted RLS role. Like global_jobs, this table
-- deliberately has no RLS policy: extracted job facts are shared market data.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_rls') THEN
    GRANT SELECT, INSERT, UPDATE ON global_job_extracts TO app_rls;
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS global_job_extracts;
