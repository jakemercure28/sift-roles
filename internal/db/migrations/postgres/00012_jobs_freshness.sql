-- +goose Up
--
-- Freshness signal for the listing itself (distinct from the `ghosted` stage,
-- which is about an applied job's employer going silent). `freshness` holds a
-- deterministic verdict (verified | fresh | stale | '') recomputed on the
-- maintenance pass from source-ATS trust + posting age; it drives the board
-- badge and the optional "hide stale listings" filter. `last_verified_open_at`
-- is stamped by the closed-job checker when the source ATS still lists the req,
-- which is what earns a listing the "verified" verdict.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS freshness TEXT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS last_verified_open_at TEXT;

-- +goose Down
ALTER TABLE jobs DROP COLUMN IF EXISTS freshness;
ALTER TABLE jobs DROP COLUMN IF EXISTS last_verified_open_at;
