-- +goose Up
--
-- Listing-freshness signal. Mirrors the Postgres track. `freshness` holds a
-- deterministic verdict (verified | fresh | stale | '') recomputed on the
-- maintenance pass; `last_verified_open_at` is stamped by the closed-job checker
-- when the source ATS still lists the req. Distinct from the `ghosted` stage,
-- which is about an applied job's employer going silent.
ALTER TABLE jobs ADD COLUMN freshness TEXT;
ALTER TABLE jobs ADD COLUMN last_verified_open_at TEXT;

-- +goose Down
ALTER TABLE jobs DROP COLUMN freshness;
ALTER TABLE jobs DROP COLUMN last_verified_open_at;
