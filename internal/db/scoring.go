package db

import "database/sql"

// UnscoredJob is the subset of a jobs row the scorer needs.
type UnscoredJob struct {
	ID          string
	Title       string
	Company     string
	Location    string
	Description string
}

// APITokenUsage is the daily Gemini token rollup stored alongside api_usage
// call counts.
type APITokenUsage struct {
	Prompt       int
	CachedPrompt int
	Candidates   int
	Total        int
}

// GetUnscoredJobs returns pending, unscored jobs in the same order as
// getUnscoredJobs in lib/db.js: never-attempted first, then oldest attempt, then
// oldest created. limit <= 0 means no limit.
func (r *Repository) GetUnscoredJobs(limit int) ([]UnscoredJob, error) {
	q := `
		SELECT id, title, company, location, description
		FROM jobs
		WHERE score IS NULL AND status = 'pending' AND user_id = ?
		ORDER BY
			CASE WHEN last_score_attempt_at IS NULL THEN 0 ELSE 1 END,
			last_score_attempt_at ASC,
			created_at ASC`
	args := []any{r.userID}
	if limit > 0 {
		q += `
		LIMIT ?`
		args = append(args, limit)
	}

	rows, err := r.query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []UnscoredJob
	for rows.Next() {
		var j UnscoredJob
		var title, company, location, description sql.NullString
		if err := rows.Scan(&j.ID, &title, &company, &location, &description); err != nil {
			return nil, err
		}
		j.Title = title.String
		j.Company = company.String
		j.Location = location.String
		j.Description = description.String
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// MarkScoreAttempt bumps score_attempts and clears any prior error, mirroring
// markJobScoreAttempt in lib/db.js.
func (r *Repository) MarkScoreAttempt(id string) error {
	_, err := r.exec(`
		UPDATE jobs
		SET score_attempts = COALESCE(score_attempts, 0) + 1,
		    last_score_attempt_at = datetime('now'),
		    score_error = NULL,
		    updated_at = datetime('now')
		WHERE id = ? AND user_id = ?`, id, r.userID)
	return err
}

// MarkScoreFailure records a scoring error, mirroring markJobScoreFailure.
func (r *Repository) MarkScoreFailure(id, errMsg string) error {
	_, err := r.exec(`
		UPDATE jobs
		SET score_error = ?, updated_at = datetime('now')
		WHERE id = ? AND user_id = ?`, errMsg, id, r.userID)
	return err
}

// UpdateJobScore stores a score and reasoning and clears any error, mirroring
// updateJobScore.
func (r *Repository) UpdateJobScore(id string, score int, reasoning string) error {
	_, err := r.exec(`
		UPDATE jobs
		SET score = ?, reasoning = ?, score_error = NULL, updated_at = datetime('now')
		WHERE id = ? AND user_id = ?`, score, reasoning, id, r.userID)
	return err
}

// AutoArchiveLowScore archives a job whose score is at or below threshold,
// mirroring the autoArchive statement in lib/pipeline/scoring.js.
func (r *Repository) AutoArchiveLowScore(id string, threshold int) error {
	_, err := r.exec(`
		UPDATE jobs
		SET status = 'archived', archive_reason = ?, updated_at = datetime('now')
		WHERE id = ? AND score <= ? AND user_id = ?`, ArchiveReasonLowScore, id, threshold, r.userID)
	return err
}

// DailyAPIUsage returns today's Gemini call count for model, used to enforce the
// daily quota. "Today" is local-date keyed, matching trackApiCall in lib/gemini.js.
func (r *Repository) DailyAPIUsage(model string) (int, error) {
	var n int
	err := r.queryRow(`
		SELECT COALESCE(SUM(call_count), 0)
		FROM api_usage
		WHERE date = date('now','localtime') AND model = ? AND user_id = ?`, model, r.userID).Scan(&n)
	return n, err
}

// RecordAPICall increments today's api_usage tally for model. It satisfies
// scorer.UsageRecorder and mirrors trackApiCall in lib/gemini.js.
func (r *Repository) RecordAPICall(model string) error {
	return r.recordAPICall(r.userID, model)
}

// RecordHostAPICall increments today's global host-key tally for model, recorded
// under the reserved HostKeyUser so usage on the shared host key is summed across
// all tenants. It satisfies scorer.UsageRecorder and is called for calls made on
// the host key.
func (r *Repository) RecordHostAPICall(model string) error {
	return r.recordAPICall(HostKeyUser, model)
}

func (r *Repository) recordAPICall(userID, model string) error {
	_, err := r.exec(`
		INSERT INTO api_usage (user_id, date, model, call_count)
		VALUES (?, date('now','localtime'), ?, 1)
		ON CONFLICT(user_id, date, model) DO UPDATE SET call_count = api_usage.call_count + 1`, userID, model)
	return err
}

// RecordAPITokens increments today's per-tenant token totals for model. It is
// called only when Gemini returns usageMetadata; zero-valued metadata is a no-op
// apart from ensuring the row exists.
func (r *Repository) RecordAPITokens(model string, promptTokens, cachedPromptTokens, candidateTokens, totalTokens int) error {
	return r.recordAPITokens(r.userID, model, promptTokens, cachedPromptTokens, candidateTokens, totalTokens)
}

// RecordHostAPITokens increments today's global host-key token totals for model.
func (r *Repository) RecordHostAPITokens(model string, promptTokens, cachedPromptTokens, candidateTokens, totalTokens int) error {
	return r.recordAPITokens(HostKeyUser, model, promptTokens, cachedPromptTokens, candidateTokens, totalTokens)
}

func (r *Repository) recordAPITokens(userID, model string, promptTokens, cachedPromptTokens, candidateTokens, totalTokens int) error {
	_, err := r.exec(`
		INSERT INTO api_usage
		  (user_id, date, model, call_count, prompt_tokens, cached_prompt_tokens, candidate_tokens, total_tokens)
		VALUES (?, date('now','localtime'), ?, 0, ?, ?, ?, ?)
		ON CONFLICT(user_id, date, model) DO UPDATE SET
		  prompt_tokens = api_usage.prompt_tokens + excluded.prompt_tokens,
		  cached_prompt_tokens = api_usage.cached_prompt_tokens + excluded.cached_prompt_tokens,
		  candidate_tokens = api_usage.candidate_tokens + excluded.candidate_tokens,
		  total_tokens = api_usage.total_tokens + excluded.total_tokens`,
		userID, model, promptTokens, cachedPromptTokens, candidateTokens, totalTokens)
	return err
}

// DailyAPITokens returns today's per-tenant Gemini token totals for model.
func (r *Repository) DailyAPITokens(model string) (APITokenUsage, error) {
	return r.dailyAPITokens(r.userID, model)
}

// HostDailyAPIUsage returns today's Gemini call count made on the shared host key
// across all tenants (the HostKeyUser tally), used to enforce the host key's global
// daily ceiling for tenants that fall back to it.
func (r *Repository) HostDailyAPIUsage(model string) (int, error) {
	var n int
	err := r.queryRow(`
		SELECT COALESCE(SUM(call_count), 0)
		FROM api_usage
		WHERE date = date('now','localtime') AND model = ? AND user_id = ?`, model, HostKeyUser).Scan(&n)
	return n, err
}

// HostDailyAPITokens returns today's Gemini token totals on the shared host key.
func (r *Repository) HostDailyAPITokens(model string) (APITokenUsage, error) {
	return r.dailyAPITokens(HostKeyUser, model)
}

func (r *Repository) dailyAPITokens(userID, model string) (APITokenUsage, error) {
	var usage APITokenUsage
	err := r.queryRow(`
		SELECT
		  COALESCE(SUM(prompt_tokens), 0),
		  COALESCE(SUM(cached_prompt_tokens), 0),
		  COALESCE(SUM(candidate_tokens), 0),
		  COALESCE(SUM(total_tokens), 0)
		FROM api_usage
		WHERE date = date('now','localtime') AND model = ? AND user_id = ?`, model, userID).Scan(
		&usage.Prompt,
		&usage.CachedPrompt,
		&usage.Candidates,
		&usage.Total,
	)
	return usage, err
}

// CountScored returns how many jobs have a score, used to detect a near-empty
// first run (matches the firstRun check in lib/pipeline/scoring.js).
func (r *Repository) CountScored() (int, error) {
	var n int
	err := r.queryRow("SELECT COUNT(*) FROM jobs WHERE score IS NOT NULL AND user_id = ?", r.userID).Scan(&n)
	return n, err
}

// CountUnscored returns how many pending jobs await a score (the scoring backlog),
// matching the GetUnscoredJobs predicate. Used to publish the jsa_jobs_pending_unscored gauge.
func (r *Repository) CountUnscored() (int, error) {
	var n int
	err := r.queryRow("SELECT COUNT(*) FROM jobs WHERE score IS NULL AND status = 'pending' AND user_id = ?", r.userID).Scan(&n)
	return n, err
}
