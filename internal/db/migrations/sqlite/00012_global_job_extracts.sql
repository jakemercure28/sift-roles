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
  extracted_at     TEXT DEFAULT (strftime('%Y-%m-%d %H:%M:%S', 'now'))
);
-- extracted_at keys the daily extraction budget (rows created today).
CREATE INDEX IF NOT EXISTS idx_global_job_extracts_at
  ON global_job_extracts(extracted_at DESC);

-- +goose Down
DROP TABLE IF EXISTS global_job_extracts;
