-- +goose Up
--
-- Incremental board fetching for the global board pass. The worker sends the
-- stored ETag as If-None-Match and hashes the board response body; a 304 or an
-- unchanged hash lets the pass skip parsing and harvesting a board whose
-- content did not move since the last cycle. State lives on the registry row
-- (global, like the boards themselves); boards not present in the registry
-- simply have no state and are always fully fetched.
ALTER TABLE company_registry ADD COLUMN etag TEXT;
ALTER TABLE company_registry ADD COLUMN last_content_hash TEXT;
ALTER TABLE company_registry ADD COLUMN last_fetched_at TEXT;
ALTER TABLE company_registry ADD COLUMN last_fetch_status TEXT;

-- +goose Down
ALTER TABLE company_registry DROP COLUMN etag;
ALTER TABLE company_registry DROP COLUMN last_content_hash;
ALTER TABLE company_registry DROP COLUMN last_fetched_at;
ALTER TABLE company_registry DROP COLUMN last_fetch_status;
