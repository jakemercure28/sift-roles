-- +goose Up
--
-- The closed-by-diff pass must tolerate scraper id-scheme drift: the Workday id
-- derivation changed around 2026-06-24, so rows harvested under the old scheme
-- are never re-seen under their id even though the same posting is re-harvested
-- under the new one. The diff now checks liveness by URL (any global row with
-- the same URL seen this cycle), which needs this index.
CREATE INDEX IF NOT EXISTS idx_global_jobs_url ON global_jobs (url);
