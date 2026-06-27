-- +goose Up
--
-- Daily Gemini call counts already live in api_usage. Add token totals from
-- Gemini usageMetadata so cost can be measured per tenant/day/model without
-- changing the existing call-quota behavior.
ALTER TABLE api_usage ADD COLUMN IF NOT EXISTS prompt_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE api_usage ADD COLUMN IF NOT EXISTS cached_prompt_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE api_usage ADD COLUMN IF NOT EXISTS candidate_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE api_usage ADD COLUMN IF NOT EXISTS total_tokens INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE api_usage DROP COLUMN IF EXISTS total_tokens;
ALTER TABLE api_usage DROP COLUMN IF EXISTS candidate_tokens;
ALTER TABLE api_usage DROP COLUMN IF EXISTS cached_prompt_tokens;
ALTER TABLE api_usage DROP COLUMN IF EXISTS prompt_tokens;
