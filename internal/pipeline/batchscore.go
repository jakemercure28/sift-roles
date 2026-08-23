package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"job-search-automation/internal/db"
	"job-search-automation/internal/metrics"
	"job-search-automation/internal/scorer"
)

// BatchAPI is the Gemini Batch API surface the orchestrator needs;
// *scorer.BatchClient satisfies it. An interface so tests run on a fake.
type BatchAPI interface {
	SubmitScoreBatch(ctx context.Context, displayName string, items []scorer.BatchItem) (string, error)
	GetScoreBatch(ctx context.Context, name string) (scorer.BatchStatus, error)
}

// BatchPromptScorer is the per-tenant scoring surface the batch path needs:
// the sync path's exact prompt build and parse+calibrate. *scorer.Scorer
// satisfies it.
type BatchPromptScorer interface {
	ScorePrompt(job scorer.Job) (string, error)
	ParseResult(job scorer.Job, text string) scorer.Result
}

// BatchScoreConfig carries the batch orchestrator knobs (from config.Config).
// The open-batch and daily-job gates are GLOBAL (cross-tenant): they bound the
// paid key's enqueued-token exposure, not any one tenant's share.
type BatchScoreConfig struct {
	// MaxOpenBatches bounds how many batches may be awaiting results at once
	// (BATCH_API_MAX_OPEN_BATCHES). With MaxJobsPerSubmit=500 the default 4
	// keeps enqueued tokens well under the Tier-1 10M cap.
	MaxOpenBatches int
	// DailyJobLimit bounds jobs submitted per UTC day across all tenants
	// (BATCH_API_DAILY_JOB_LIMIT); the direct spend throttle.
	DailyJobLimit int
	// MaxJobsPerSubmit bounds one batch's size (BATCH_API_MAX_JOBS_PER_SUBMIT).
	// ~500 inline prompts is ~5.5MB, safely under the API's 20MB request cap.
	MaxJobsPerSubmit int
	// ArchiveThreshold mirrors the sync path's AUTO_ARCHIVE_THRESHOLD: an
	// applied batch score at or below it buries the job immediately.
	ArchiveThreshold int
}

// BatchTenant is one tenant's scoring context for the batch path, built by the
// caller (main.go) from the same pieces the sync path uses so the two paths
// cannot drift: same prefilter, same resume hash, same Scorer.
type BatchTenant struct {
	UserID     string
	Repo       *db.Repository
	Scorer     BatchPromptScorer
	Prefilter  PrefilterFunc
	ResumeHash string
}

// PollScoreBatches advances every open batch: still-running ones are left
// alone, finished ones have their results applied (through the same
// parse/calibrate/guarded-write/auto-archive semantics as sync scoring) and
// are closed. All state lives in the DB, so a restart mid-flight loses
// nothing; a transient poll error just leaves the batch for the next tick.
// tenantFor builds the owning tenant's context for each batch (batches can
// belong to tenants that have since gone inactive).
func PollScoreBatches(ctx context.Context, base *db.Repository, api BatchAPI, tenantFor func(userID string) BatchTenant, cfg BatchScoreConfig, log *slog.Logger) (applied int, err error) {
	open, err := base.OpenScoreBatches()
	if err != nil {
		return 0, err
	}
	for _, b := range open {
		if ctx.Err() != nil {
			return applied, ctx.Err()
		}
		// Past the exclusion bound the upstream batch has long expired
		// (~48h), so a still-open row is a wedge, not work in flight. One
		// last poll still runs first so a theoretical late SUCCEEDED is
		// applied rather than discarded; every other outcome evicts.
		stale := batchPastMaxAge(b, time.Now().UTC())
		st, err := api.GetScoreBatch(ctx, b.BatchName)
		if err != nil {
			if stale {
				evictStuckBatch(tenantFor(b.UserID), b, "last poll error: "+err.Error(), log)
				continue
			}
			log.Warn("batch poll failed; will retry next tick", "batch", b.BatchName, "error", err)
			continue
		}
		if !scorer.BatchDone(st.State) {
			if stale {
				evictStuckBatch(tenantFor(b.UserID), b, "still "+st.State+" upstream", log)
			}
			continue
		}

		t := tenantFor(b.UserID)
		if st.State == scorer.BatchStateSucceeded {
			n, err := applyBatchResults(t, b, st.Results, cfg, log)
			if err != nil {
				// The apply is one atomic transaction, so nothing landed;
				// the batch is still open and next tick retries, which the
				// pending-and-unscored guards make idempotent.
				log.Warn("batch apply failed; will retry next tick", "batch", b.BatchName, "error", err)
				continue
			}
			applied += n
			log.Info("score batch applied", "user_id", b.UserID, "batch", b.BatchName,
				"results", len(st.Results), "applied", n)
			continue
		}

		// FAILED / CANCELLED / EXPIRED: closing deletes membership, which
		// returns the jobs to the sync pool and future submissions.
		state := map[string]string{
			scorer.BatchStateFailed:    db.ScoreBatchFailed,
			scorer.BatchStateCancelled: db.ScoreBatchCancelled,
			scorer.BatchStateExpired:   db.ScoreBatchExpired,
		}[st.State]
		if err := t.Repo.CloseScoreBatch(b.BatchName, state, st.Err); err != nil {
			log.Warn("batch close failed", "batch", b.BatchName, "error", err)
			continue
		}
		log.Warn("score batch closed without results; jobs returned to pool",
			"user_id", b.UserID, "batch", b.BatchName, "state", state, "error", st.Err)
	}
	return applied, nil
}

// batchPastMaxAge reports whether a batch row is older than the exclusion
// bound. An unparseable or empty created_at cannot prove staleness, so it
// reports false (the row keeps polling normally).
func batchPastMaxAge(b db.ScoreBatch, now time.Time) bool {
	created, err := time.Parse("2006-01-02 15:04:05", b.CreatedAt)
	if err != nil {
		return false
	}
	return now.Sub(created) > db.OpenBatchExclusionMax
}

// evictStuckBatch closes a wedged batch as expired. Closing deletes
// membership (jobs return to the pool) and stops the row counting against
// the open-batch gate. A batch this old already expired upstream, so no
// Gemini-side cancel is attempted.
func evictStuckBatch(t BatchTenant, b db.ScoreBatch, why string, log *slog.Logger) {
	msg := "evicted: open past max age; " + why
	if err := t.Repo.CloseScoreBatch(b.BatchName, db.ScoreBatchExpired, msg); err != nil {
		log.Warn("stuck batch evict failed", "batch", b.BatchName, "error", err)
		return
	}
	log.Warn("stuck batch evicted; jobs returned to pool",
		"user_id", b.UserID, "batch", b.BatchName, "created_at", b.CreatedAt, "reason", why)
}

// applyBatchResults lands one finished batch through the sync path's semantics
// in three phases: one chunked read of the member jobs, in-memory parse +
// calibrate (stamped with the batch's SUBMIT-time resume hash; correct under
// keep-stale semantics: if the resume changed mid-flight the hash mismatch
// marks the score stale, exactly like any kept score), then a single
// transaction that claims the batch row, applies every guarded write,
// auto-archives at the threshold, and adds the aggregated ledger under the
// batch model key so sync quotas are untouched. An error means nothing was
// written and the still-open batch retries next tick.
func applyBatchResults(t BatchTenant, b db.ScoreBatch, results []scorer.BatchResult, cfg BatchScoreConfig, log *slog.Logger) (int, error) {
	ids := make([]string, 0, len(results))
	for _, r := range results {
		if r.Key != "" {
			ids = append(ids, r.Key)
		}
	}
	briefs, err := t.Repo.JobBriefs(ids)
	if err != nil {
		return 0, err
	}

	var usage db.BatchUsageTotals
	writes := make([]db.BatchScoreWrite, 0, len(results))
	for _, r := range results {
		if r.Key == "" {
			continue
		}
		// Ledger every item that consumed model work, aggregated into one
		// delta the apply transaction adds atomically.
		if r.TotalTokens > 0 || r.Text != "" {
			usage.Calls++
			usage.Tokens.Prompt += r.PromptTokens
			usage.Tokens.CachedPrompt += r.CachedTokens
			usage.Tokens.Candidates += r.CandidateTokens
			usage.Tokens.Total += r.TotalTokens
		}
		if r.Err != "" {
			writes = append(writes, db.BatchScoreWrite{JobID: r.Key, ErrMsg: "Batch item failed: " + r.Err})
			continue
		}
		brief, found := briefs[r.Key]
		if !found {
			// Deleted or another tenant's id (never ours, but fail safe): drop.
			continue
		}
		res := t.Scorer.ParseResult(scorer.Job{
			Title:       brief.Title,
			Company:     brief.Company,
			Location:    brief.Location,
			Description: brief.Description,
		}, r.Text)
		if res.Score == nil {
			msg := res.Reasoning
			if msg == "" {
				msg = "Batch item returned an unparsable response."
			}
			writes = append(writes, db.BatchScoreWrite{JobID: r.Key, ErrMsg: msg})
			continue
		}
		writes = append(writes, db.BatchScoreWrite{JobID: r.Key, Score: *res.Score, Reasoning: res.Reasoning})
	}

	applied, claimed, err := t.Repo.ApplyBatchScoreResults(b.BatchName, writes, b.ResumeHash, cfg.ArchiveThreshold, scorer.BatchModel, usage)
	if err != nil {
		return 0, err
	}
	if !claimed {
		// A racing poller (another process) already closed and handled this
		// batch; nothing was written, so observe no telemetry either.
		log.Info("score batch already closed elsewhere; skipped", "batch", b.BatchName)
		return 0, nil
	}
	for _, r := range results {
		if r.TotalTokens > 0 {
			metrics.ObserveGeminiTokens("score_batch_async", "overflow",
				r.PromptTokens, r.CachedTokens, r.CandidateTokens, r.ThoughtTokens, r.TotalTokens)
		}
	}
	return applied, nil
}

// SubmitScoreBatches fills the open-batch slots with unscored jobs from the
// given tenants (the caller passes ACTIVE tenants only, in round-robin-fair
// order), one batch per tenant per pass, until the global open-batch and
// daily-job gates are hit. Job selection reuses GetUnscoredJobs (newest-first,
// already excluding members of open batches and empty-description rows in
// SQL) plus the sync path's prefilter and a whitespace-only description
// guard, so a batch slot is never wasted on a job sync scoring would have
// resolved for free.
func SubmitScoreBatches(ctx context.Context, base *db.Repository, api BatchAPI, tenants []BatchTenant, cfg BatchScoreConfig, log *slog.Logger) (submitted int, err error) {
	openCount, err := base.CountOpenScoreBatches()
	if err != nil {
		return 0, err
	}
	sinceUTC := time.Now().UTC().Format("2006-01-02") + " 00:00:00"
	usedToday, err := base.SumBatchJobsSubmittedSince(sinceUTC)
	if err != nil {
		return 0, err
	}
	budget := cfg.DailyJobLimit - usedToday

	for _, t := range tenants {
		if ctx.Err() != nil {
			return submitted, ctx.Err()
		}
		if openCount >= cfg.MaxOpenBatches || budget <= 0 {
			break
		}
		limit := cfg.MaxJobsPerSubmit
		if budget < limit {
			limit = budget
		}
		jobs, err := t.Repo.GetUnscoredJobs(limit)
		if err != nil {
			log.Warn("batch job pull failed", "user_id", t.UserID, "error", err)
			continue
		}
		jobs, _ = applyPrefilter(t.Repo, jobs, t.Prefilter, log)
		// GetUnscoredJobs excludes empty descriptions in SQL, but its one-arg
		// TRIM only strips ASCII spaces: keep the Go-side partition as a
		// guard (no stamping) so a "\n" body is left pending, never
		// submitted title-only.
		scorable, _ := splitNoDescription(jobs)
		if len(scorable) == 0 {
			continue
		}

		items := make([]scorer.BatchItem, 0, len(scorable))
		ids := make([]string, 0, len(scorable))
		for _, j := range scorable {
			prompt, err := t.Scorer.ScorePrompt(scorer.Job{
				Title:       j.Title,
				Company:     j.Company,
				Location:    j.Location,
				Description: j.Description,
			})
			if err != nil {
				// Resume unreadable: nothing in this tenant will build.
				log.Warn("batch prompt build failed; skipping tenant", "user_id", t.UserID, "error", err)
				items = nil
				break
			}
			items = append(items, scorer.BatchItem{Key: j.ID, Prompt: prompt})
			ids = append(ids, j.ID)
		}
		if len(items) == 0 {
			continue
		}

		displayName := fmt.Sprintf("jsa-score-%s-%d", shortUID(t.UserID), time.Now().Unix())
		batchName, err := api.SubmitScoreBatch(ctx, displayName, items)
		if err != nil {
			// Quota exhaustion outlives this pass: surface the typed error
			// so the orchestrator can impose a submit cooldown. (Explicit
			// return: the named err is shadowed inside this loop.)
			if errors.Is(err, scorer.ErrQuotaExhausted) {
				log.Warn("batch submit quota exhausted; stopping this pass", "user_id", t.UserID, "error", err)
				return submitted, err
			}
			// Other submission failures (transport, 5xx after retries)
			// likely affect every tenant this pass; stop rather than
			// hammer the API, but stay silent to the caller like before.
			log.Warn("batch submit failed; stopping this pass", "user_id", t.UserID, "error", err)
			break
		}
		if err := t.Repo.CreateScoreBatch(batchName, scorer.BatchModel, t.ResumeHash, ids); err != nil {
			// Orphaned upstream batch: its results are never applied and the
			// jobs remain in the normal pool. Costs one batch of discounted
			// tokens, corrupts nothing.
			log.Error("batch ledger write failed; upstream batch orphaned", "batch", batchName, "error", err)
			continue
		}
		openCount++
		budget -= len(items)
		submitted += len(items)
		log.Info("score batch submitted", "user_id", t.UserID, "batch", batchName, "jobs", len(items))
	}
	return submitted, nil
}

func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
