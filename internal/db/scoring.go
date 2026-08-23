package db

import (
	"database/sql"
	"strings"
	"time"
)

// OpenBatchExclusionMax bounds how long an open score batch may hide its
// member jobs from the sync unscored queue. Upstream Gemini batches expire
// ~48h after creation, so past 72h a still-"submitted" batch row can only
// mean the poll loop stopped (feature disabled or overflow key removed
// mid-flight) and its results will never arrive; handing the jobs back to
// sync scoring is then strictly correct. The pending-and-unscored guard in
// ApplyBatchScore makes any theoretical late overlap harmless. The poll loop
// uses the same bound as its eviction age, and CountOpenScoreBatches as its
// gate bound, so a batch past it stops counting everywhere at once.
const OpenBatchExclusionMax = 72 * time.Hour

// scorableStatusSQL is the status predicate for "worth spending a scoring call
// on": still pending, or advanced by the user into a live pipeline stage. The
// stage list mirrors the dashboard's terminal buckets (see listFilters in
// joblist.go) so a job the user closed, was rejected from, or marked ghosted
// never costs a call. Interpolated, not parameterized: it is a constant with no
// caller input in it.
const scorableStatusSQL = `status = 'pending' OR (status IN ('applied', 'responded') ` +
	`AND COALESCE(stage, '') NOT IN ('closed', 'rejected', 'ghosted'))`

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

// GetUnscoredJobs returns unscored jobs the scorer should spend budget on:
// user-touched rows first, then never-attempted, then newest posting first.
// Fresh listings are worth far more than backlog (an old posting is likely
// filled, and maintenance archives it at the stale window anyway), so under a
// daily quota the newest jobs must win the budget. Within retried rows, the
// least-recently-attempted goes first for retry fairness.
//
// "User-touched" means a row the user advanced into an active pipeline stage
// (applied/responded, not yet closed/rejected/ghosted). Those rows leave
// status='pending' the moment the user marks them applied, which used to hide
// them from the scorer forever: a job manually added and taken straight to a
// phone screen could never get a score. They are few, bounded by how many jobs
// a human can actually advance, and the ones whose blank score is most visible,
// so they jump the line rather than competing with the backlog on recency.
// Terminal stages are deliberately NOT included: scoring a closed or rejected
// job spends quota on a decision that is already made.
// posted_at is scraper-sourced TEXT that can hold junk like "Posted 6 Days
// Ago"; the substr shape-guard (works on both dialects) keeps junk from
// lexically outsorting real dates by falling back to created_at.
// Rows with an empty description are excluded in SQL: neither scoring path
// can evaluate one (a bare title inflates the score), so they wait as plain
// pending rows until a rescrape or descriptions-once fills the body, instead
// of occupying the head of every capped fetch. One-arg TRIM strips only ASCII
// spaces on both dialects, so callers keep a TrimSpace guard for other
// whitespace. limit <= 0 means no limit.
//
// This intentionally diverges from the retired lib/db.js oldest-first order.
func (r *Repository) GetUnscoredJobs(limit int) ([]UnscoredJob, error) {
	// The NOT IN subquery excludes members of open score batches: the Batch
	// API will score those jobs within ~24h at half price, so sync quota
	// spent on them is pure waste. Members are deleted when a batch closes in
	// any terminal state, so the exclusion self-heals if a batch fails or
	// expires. The created_at bound makes that self-healing unconditional:
	// even if the poll loop that closes batches never runs again, member
	// jobs return to the sync pool once the batch is old enough that its
	// results can no longer arrive (see OpenBatchExclusionMax). Both
	// dialects store created_at as UTC "YYYY-MM-DD HH:MM:SS" text, so the
	// lexical comparison is a correct time comparison.
	exclusionCutoff := time.Now().UTC().Add(-OpenBatchExclusionMax).Format("2006-01-02 15:04:05")
	q := `
		SELECT id, title, company, location, description
		FROM jobs
		WHERE score IS NULL AND user_id = ?
		  AND (` + scorableStatusSQL + `)
		  AND TRIM(COALESCE(description, '')) <> ''
		  AND id NOT IN (
			SELECT sbj.job_id FROM score_batch_jobs sbj
			JOIN score_batches sb ON sb.user_id = sbj.user_id AND sb.batch_name = sbj.batch_name
			WHERE sbj.user_id = ? AND sb.state = 'submitted' AND sb.created_at > ?
		  )
		ORDER BY
			CASE WHEN status = 'pending' THEN 1 ELSE 0 END,
			CASE WHEN last_score_attempt_at IS NULL THEN 0 ELSE 1 END,
			last_score_attempt_at ASC,
			COALESCE(CASE WHEN substr(posted_at, 1, 2) = '20' THEN posted_at END, created_at) DESC`
	args := []any{r.userID, r.userID, exclusionCutoff}
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

// TopPendingByKeywords returns pending, unscored jobs ranked best-first for the
// onboarding burst: rows whose title matches more of the tenant's target-title
// keywords sort ahead, ties broken by newest posting. Rows with no description
// are excluded outright since the scoring pipeline would deterministically fail
// them without an API call, and a burst slot must never be spent on one. Empty
// keywords fall back to GetUnscoredJobs ordering (which already excludes
// description-less rows in SQL).
func (r *Repository) TopPendingByKeywords(limit int, keywords []string) ([]UnscoredJob, error) {
	kw := normalizeSeedKeywords(keywords)
	if len(kw) == 0 {
		return r.GetUnscoredJobs(limit)
	}

	hits := make([]string, len(kw))
	args := []any{r.userID}
	for i, k := range kw {
		hits[i] = "(CASE WHEN LOWER(title) LIKE ? THEN 1 ELSE 0 END)"
		args = append(args, "%"+k+"%")
	}
	q := `
		SELECT id, title, company, location, description
		FROM jobs
		WHERE score IS NULL AND status = 'pending' AND user_id = ?
		  AND COALESCE(description, '') <> ''
		ORDER BY (` + strings.Join(hits, " + ") + `) DESC,
			COALESCE(NULLIF(posted_at, ''), created_at) DESC`
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

// UpdateJobScoreForResume is UpdateJobScore plus the resume-hash stamp that
// ties the score to the resume version it was computed against, so a later
// resume edit can invalidate exactly the scores that went stale.
func (r *Repository) UpdateJobScoreForResume(id string, score int, reasoning, resumeHash string) error {
	if resumeHash == "" {
		return r.UpdateJobScore(id, score, reasoning)
	}
	_, err := r.exec(`
		UPDATE jobs
		SET score = ?, reasoning = ?, resume_hash = ?, score_error = NULL, updated_at = datetime('now')
		WHERE id = ? AND user_id = ?`, score, reasoning, resumeHash, id, r.userID)
	return err
}

// RequeueStaleHighScores un-scores only the pending rows scored >= minScore
// against a different resume than currentHash, sending that small set back
// through the unscored queue for a re-match under the new resume. Everything
// below the band KEEPS its stale score: users tweak their resume constantly,
// and nulling the whole backlog on every upload re-spent the entire scoring
// budget each time (and once wiped a tenant's 3k scores overnight). The
// mismatched hash left in place IS the staleness marker (CountStaleScores);
// low scores are near-certain to stay low under a resume tweak, so the spend
// is pointed at the rows a user actually acts on. Scoped like before: only
// status='pending' moves, applied/closed/archived history keeps its score,
// and NULL-hash rows (scored before stamping) are never touched.
func (r *Repository) RequeueStaleHighScores(currentHash string, minScore int) (int64, error) {
	if currentHash == "" {
		return 0, nil
	}
	res, err := r.exec(`
		UPDATE jobs
		SET score = NULL, reasoning = NULL, resume_hash = NULL, score_error = NULL, updated_at = datetime('now')
		WHERE user_id = ? AND status = 'pending' AND score IS NOT NULL AND score >= ?
		  AND resume_hash IS NOT NULL AND resume_hash <> ?`, r.userID, minScore, currentHash)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountStaleScores counts pending rows whose kept score was computed against a
// different resume than currentHash (the banner's "N scores reflect an older
// resume" figure). NULL-hash rows predate stamping and are not counted: their
// provenance is unknown, so flagging them would permanently show a banner the
// user can't clear.
func (r *Repository) CountStaleScores(currentHash string) (int, error) {
	if currentHash == "" {
		return 0, nil
	}
	var n int
	err := r.queryRow(`
		SELECT COUNT(*) FROM jobs
		WHERE user_id = ? AND status = 'pending' AND score IS NOT NULL
		  AND resume_hash IS NOT NULL AND resume_hash <> ?`, r.userID, currentHash).Scan(&n)
	return n, err
}

// AutoArchiveLowScore archives a job whose score is at or below threshold,
// mirroring the autoArchive statement in lib/pipeline/scoring.js.
//
// The status guard is load-bearing now that GetUnscoredJobs also returns
// user-touched rows: without it, scoring a job the user is actively
// interviewing for would archive it out of their pipeline the moment the model
// returned a low number. A user who advanced a job has already overruled the
// score, so triage never gets to take it back.
func (r *Repository) AutoArchiveLowScore(id string, threshold int) error {
	_, err := r.exec(`
		UPDATE jobs
		SET status = 'archived', archive_reason = ?, updated_at = datetime('now')
		WHERE id = ? AND score <= ? AND user_id = ? AND status = 'pending'`,
		ArchiveReasonLowScore, id, threshold, r.userID)
	return err
}

// ArchivePrefiltered archives an unscored pending job the pre-score location
// prefilter rejected. The score/status guards make it a no-op on a row a user
// or another pass already touched, so the prefilter can never clobber a scored
// or user-managed job. It reports whether the archive actually landed, so a
// caller can tell "filtered out" from "guards declined it" and keep the job
// instead of silently dropping it from the pass.
func (r *Repository) ArchivePrefiltered(id, reason string) (bool, error) {
	res, err := r.exec(`
		UPDATE jobs
		SET status = 'archived', archive_reason = ?, updated_at = datetime('now')
		WHERE id = ? AND user_id = ? AND score IS NULL AND status = 'pending'`, reason, id, r.userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
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

// CountUnscored returns how many pending jobs await a score (the scoring
// backlog). Used to publish the jsa_jobs_pending_unscored gauge. Unlike
// GetUnscoredJobs it deliberately does NOT subtract members of open score
// batches: those jobs are still unscored, they are just being scored on the
// batch lane, so the gauge reports the true backlog. Expect it to exceed
// what sync scoring alone can drain while batches are in flight.
func (r *Repository) CountUnscored() (int, error) {
	var n int
	err := r.queryRow("SELECT COUNT(*) FROM jobs WHERE score IS NULL AND status = 'pending' AND user_id = ?", r.userID).Scan(&n)
	return n, err
}

// CountScoredJobs counts this tenant's rows that have ever been scored. The
// activity gate uses it to tell a brand-new signup (never seen AND nothing
// scored: keep full scoring so onboarding isn't starved) from a tenant who
// simply has not visited since the last-seen stamp shipped.
func (r *Repository) CountScoredJobs() (int, error) {
	var n int
	err := r.queryRow("SELECT COUNT(*) FROM jobs WHERE score IS NOT NULL AND user_id = ?", r.userID).Scan(&n)
	return n, err
}
