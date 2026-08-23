-- +goose Up
--
-- Which resume version produced a row's score. Stamped at score time; a resume
-- edit then clears scores whose hash no longer matches, so those jobs re-enter
-- the unscored queue and re-match cheaply against cached extracts instead of
-- silently keeping scores computed against an old resume. Rows scored before
-- this column existed keep a NULL hash and are deliberately NOT invalidated,
-- so the first deploy never triggers a mass re-score.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS resume_hash TEXT;

-- +goose Down
ALTER TABLE jobs DROP COLUMN IF EXISTS resume_hash;
