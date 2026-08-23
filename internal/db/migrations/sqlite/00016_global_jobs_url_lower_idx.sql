-- +goose Up
--
-- The closed-by-diff pass must tolerate board-path casing drift on top of id
-- drift: registry-harvested lowercase Workday entries replaced labeled ones
-- (2026-07-05), so the same posting lives under /en-US/Jobs/ and /en-US/jobs/
-- URLs and a case-sensitive URL match read the stale-cased twin as vanished,
-- closing live jobs. The diff and the reopen pass now match URLs with
-- LOWER(), which needs this expression index.
CREATE INDEX IF NOT EXISTS idx_global_jobs_url_lower ON global_jobs (LOWER(url));
