package db

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Score-batch DB states. "submitted" is the only open state; everything else
// is terminal and set by CloseScoreBatch. These are OUR ledger states, not the
// wire states the Gemini API reports (the orchestrator maps those onto these).
const (
	ScoreBatchSubmitted = "submitted"
	ScoreBatchSucceeded = "succeeded"
	ScoreBatchFailed    = "failed"
	ScoreBatchCancelled = "cancelled"
	ScoreBatchExpired   = "expired"
)

// ScoreBatch is one submitted Gemini batch job's ledger row.
type ScoreBatch struct {
	UserID     string
	BatchName  string
	State      string
	Model      string
	ResumeHash string
	JobCount   int
	Error      string
	CreatedAt  string
}

// CreateScoreBatch records a freshly submitted batch and its member jobs in
// one transaction, so a crash between the API submit and this write is the
// only window where a batch can exist upstream without a ledger row (such an
// orphan costs nothing: its results are simply never applied, and the jobs
// stay in the normal unscored pool).
func (r *Repository) CreateScoreBatch(batchName, model, resumeHash string, jobIDs []string) error {
	if batchName == "" || len(jobIDs) == 0 {
		return errors.New("score batch needs a name and at least one job")
	}
	return r.inTx(func(ex execer) error {
		if _, err := ex.Exec(`
			INSERT INTO score_batches (user_id, batch_name, state, model, resume_hash, job_count)
			VALUES (?, ?, ?, ?, ?, ?)`,
			r.userID, batchName, ScoreBatchSubmitted, model, resumeHash, len(jobIDs)); err != nil {
			return err
		}
		for _, id := range jobIDs {
			if _, err := ex.Exec(`
				INSERT INTO score_batch_jobs (user_id, batch_name, job_id)
				VALUES (?, ?, ?)`, r.userID, batchName, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// OpenScoreBatches returns every batch still awaiting results, across ALL
// tenants, oldest first. It powers the poll loop, which runs on the
// service-role connection (cross-tenant, like the other crons); each returned
// row carries its UserID so results are applied through a tenant-scoped
// repository.
func (r *Repository) OpenScoreBatches() ([]ScoreBatch, error) {
	rows, err := r.query(`
		SELECT user_id, batch_name, state, model, COALESCE(resume_hash, ''), job_count, COALESCE(error, ''), COALESCE(created_at, '')
		FROM score_batches
		WHERE state = ?
		ORDER BY created_at ASC`, ScoreBatchSubmitted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ScoreBatch
	for rows.Next() {
		var b ScoreBatch
		if err := rows.Scan(&b.UserID, &b.BatchName, &b.State, &b.Model, &b.ResumeHash, &b.JobCount, &b.Error, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CountOpenScoreBatches returns how many batches are awaiting results across
// all tenants (the BATCH_API_MAX_OPEN_BATCHES gate). Batches older than
// OpenBatchExclusionMax are not counted: their results can no longer arrive,
// and the poll loop evicts them anyway, so a wedged row must never be able to
// hold a submit slot hostage even if that eviction never runs.
func (r *Repository) CountOpenScoreBatches() (int, error) {
	cutoff := time.Now().UTC().Add(-OpenBatchExclusionMax).Format("2006-01-02 15:04:05")
	var n int
	err := r.queryRow(`
		SELECT COUNT(*) FROM score_batches
		WHERE state = ? AND created_at > ?`, ScoreBatchSubmitted, cutoff).Scan(&n)
	return n, err
}

// OldestOpenScoreBatchCreatedAt returns the created_at of the oldest batch
// still awaiting results across all tenants ("" when none are open). Unlike
// CountOpenScoreBatches it is deliberately NOT age-bounded: the age gauge it
// feeds must keep growing past the eviction bound if the poll loop is dead,
// because that growth is exactly the alert signal.
func (r *Repository) OldestOpenScoreBatchCreatedAt() (string, error) {
	var created sql.NullString
	err := r.queryRow(`
		SELECT MIN(created_at) FROM score_batches
		WHERE state = ?`, ScoreBatchSubmitted).Scan(&created)
	return created.String, err
}

// RecentScoreBatches returns every open batch plus any batch created at or
// after sinceUTC ("YYYY-MM-DD HH:MM:SS"), newest first, across all tenants:
// the `server batch list` admin view. Open rows are always included however
// old, because a wedged old row is exactly what the operator is looking for.
func (r *Repository) RecentScoreBatches(sinceUTC string) ([]ScoreBatch, error) {
	rows, err := r.query(`
		SELECT user_id, batch_name, state, model, COALESCE(resume_hash, ''), job_count, COALESCE(error, ''), COALESCE(created_at, '')
		FROM score_batches
		WHERE state = ? OR created_at >= ?
		ORDER BY created_at DESC`, ScoreBatchSubmitted, sinceUTC)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ScoreBatch
	for rows.Next() {
		var b ScoreBatch
		if err := rows.Scan(&b.UserID, &b.BatchName, &b.State, &b.Model, &b.ResumeHash, &b.JobCount, &b.Error, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetScoreBatchByName looks up one batch across all tenants (the admin CLI's
// lookup; the returned UserID is what a tenant-scoped mutation then re-scopes
// on). found=false means no such batch.
func (r *Repository) GetScoreBatchByName(batchName string) (ScoreBatch, bool, error) {
	var b ScoreBatch
	err := r.queryRow(`
		SELECT user_id, batch_name, state, model, COALESCE(resume_hash, ''), job_count, COALESCE(error, ''), COALESCE(created_at, '')
		FROM score_batches
		WHERE batch_name = ?`, batchName).
		Scan(&b.UserID, &b.BatchName, &b.State, &b.Model, &b.ResumeHash, &b.JobCount, &b.Error, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ScoreBatch{}, false, nil
	}
	if err != nil {
		return ScoreBatch{}, false, err
	}
	return b, true, nil
}

// SumBatchJobsSubmittedSince totals job_count over every batch (any state)
// created at or after sinceUTC ("YYYY-MM-DD HH:MM:SS"), across all tenants:
// the BATCH_API_DAILY_JOB_LIMIT gate. Terminal batches still count; the
// tokens were enqueued regardless of how the batch ended.
func (r *Repository) SumBatchJobsSubmittedSince(sinceUTC string) (int, error) {
	var n int
	err := r.queryRow(`
		SELECT COALESCE(SUM(job_count), 0) FROM score_batches
		WHERE created_at >= ?`, sinceUTC).Scan(&n)
	return n, err
}

// CloseScoreBatch marks a batch terminal and deletes its member rows. The
// member delete is what returns not-yet-scored jobs to the sync unscored pool
// (GetUnscoredJobs only excludes members of open batches, but pruning members
// keeps the join table from growing forever). Call it AFTER applying whatever
// results the batch produced.
func (r *Repository) CloseScoreBatch(batchName, state, errMsg string) error {
	return r.inTx(func(ex execer) error {
		res, err := ex.Exec(`
			UPDATE score_batches
			SET state = ?, error = ?, updated_at = datetime('now')
			WHERE batch_name = ? AND user_id = ? AND state = ?`,
			state, nullable(errMsg), batchName, r.userID, ScoreBatchSubmitted)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			// Already closed (e.g. two pollers raced); leave members alone,
			// the earlier closer handled them.
			return nil
		}
		_, err = ex.Exec(`
			DELETE FROM score_batch_jobs WHERE batch_name = ? AND user_id = ?`,
			batchName, r.userID)
		return err
	})
}

// BatchScoreWrite is one parsed batch item's pending write: a calibrated score
// to apply, or (ErrMsg set) a per-item failure to stamp.
type BatchScoreWrite struct {
	JobID     string
	Score     int
	Reasoning string
	// ErrMsg non-empty stamps score_error instead of applying a score.
	ErrMsg string
}

// BatchUsageTotals is one finished batch's aggregated api_usage delta: Calls
// counts items that consumed model work, Tokens sums their usage metadata.
type BatchUsageTotals struct {
	Calls  int
	Tokens APITokenUsage
}

// jobBriefsChunk bounds the id list of one IN query (JobBriefs), keeping
// placeholder counts dialect-safe.
const jobBriefsChunk = 200

// JobBriefs loads scoring fields for many jobs at once, keyed by id, in
// chunked IN queries: one round trip per ~200 ids instead of one per job (the
// poll pass reads every member of a finished batch). Ids that don't exist, or
// that belong to another tenant, are simply absent from the map.
func (r *Repository) JobBriefs(ids []string) (map[string]JobBrief, error) {
	out := make(map[string]JobBrief, len(ids))
	for start := 0; start < len(ids); start += jobBriefsChunk {
		part := ids[start:min(start+jobBriefsChunk, len(ids))]
		args := make([]any, 0, len(part)+1)
		args = append(args, r.userID)
		for _, id := range part {
			args = append(args, id)
		}
		err := func() error {
			rows, err := r.query(`
				SELECT id, title, company, location, description FROM jobs
				WHERE user_id = ? AND id IN (?`+strings.Repeat(",?", len(part)-1)+`)`, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id string
				var title, company, location, desc sql.NullString
				if err := rows.Scan(&id, &title, &company, &location, &desc); err != nil {
					return err
				}
				out[id] = JobBrief{Title: title.String, Company: company.String, Location: location.String, Description: desc.String}
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ApplyBatchScoreResults lands one SUCCEEDED batch in a single transaction: it
// claims the batch row (submitted -> succeeded), deletes membership, runs
// every guarded write, auto-archives low scores, and adds the aggregated
// api_usage ledger rows (tenant + host). One transaction instead of six or
// seven per item keeps the RLS per-statement transaction overhead out of the
// poll pass, and makes the whole apply atomic: on error nothing landed
// (ledger included) and the still-open batch simply retries next tick, which
// the guards make idempotent and the atomic ledger makes double-count-proof.
//
// The score guard means a write can NEVER clobber anything that happened
// during the batch's in-flight window: it only lands on a row that is still
// pending and still unscored. A user action (archive/apply), a sync-path
// score, or a resume-change requeue in the meantime all make the guard miss,
// and the stale batch result is dropped. Error stamps carry the same guard,
// plus last_score_attempt_at so the sync path's attempt ordering sees the try.
//
// claimed=false means another poller already closed the batch (it handled the
// results) and nothing was written.
func (r *Repository) ApplyBatchScoreResults(batchName string, writes []BatchScoreWrite, resumeHash string, archiveThreshold int, model string, usage BatchUsageTotals) (applied int, claimed bool, err error) {
	err = r.inTx(func(ex execer) error {
		res, err := ex.Exec(`
			UPDATE score_batches
			SET state = ?, error = NULL, updated_at = datetime('now')
			WHERE batch_name = ? AND user_id = ? AND state = ?`,
			ScoreBatchSucceeded, batchName, r.userID, ScoreBatchSubmitted)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return nil
		}
		claimed = true
		if _, err := ex.Exec(`
			DELETE FROM score_batch_jobs WHERE batch_name = ? AND user_id = ?`,
			batchName, r.userID); err != nil {
			return err
		}

		for _, w := range writes {
			if w.ErrMsg != "" {
				if _, err := ex.Exec(`
					UPDATE jobs
					SET score_error = ?, last_score_attempt_at = datetime('now'), updated_at = datetime('now')
					WHERE id = ? AND user_id = ? AND status = 'pending' AND score IS NULL`,
					w.ErrMsg, w.JobID, r.userID); err != nil {
					return err
				}
				continue
			}
			res, err := ex.Exec(`
				UPDATE jobs
				SET score = ?, reasoning = ?, resume_hash = ?, score_error = NULL, updated_at = datetime('now')
				WHERE id = ? AND user_id = ? AND status = 'pending' AND score IS NULL`,
				w.Score, w.Reasoning, nullable(resumeHash), w.JobID, r.userID)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n == 0 {
				continue
			}
			applied++
			if w.Score <= archiveThreshold {
				if _, err := ex.Exec(`
					UPDATE jobs
					SET status = 'archived', archive_reason = ?, updated_at = datetime('now')
					WHERE id = ? AND score <= ? AND user_id = ?`,
					ArchiveReasonLowScore, w.JobID, archiveThreshold, r.userID); err != nil {
					return err
				}
			}
		}

		// Batch spend is keyed by the batch model so the sync path's per-model
		// daily quotas are untouched; the host row keeps the shared-key
		// aggregate honest. Same row shapes as recordAPICall/recordAPITokens,
		// added in one upsert per ledger row instead of four per item.
		if usage.Calls > 0 {
			for _, uid := range []string{r.userID, HostKeyUser} {
				if _, err := ex.Exec(`
					INSERT INTO api_usage
					  (user_id, date, model, call_count, prompt_tokens, cached_prompt_tokens, candidate_tokens, total_tokens)
					VALUES (?, date('now','localtime'), ?, ?, ?, ?, ?, ?)
					ON CONFLICT(user_id, date, model) DO UPDATE SET
					  call_count = api_usage.call_count + excluded.call_count,
					  prompt_tokens = api_usage.prompt_tokens + excluded.prompt_tokens,
					  cached_prompt_tokens = api_usage.cached_prompt_tokens + excluded.cached_prompt_tokens,
					  candidate_tokens = api_usage.candidate_tokens + excluded.candidate_tokens,
					  total_tokens = api_usage.total_tokens + excluded.total_tokens`,
					uid, model, usage.Calls, usage.Tokens.Prompt, usage.Tokens.CachedPrompt, usage.Tokens.Candidates, usage.Tokens.Total); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return applied, claimed, nil
}
