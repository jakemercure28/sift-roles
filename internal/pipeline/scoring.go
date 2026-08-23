// Package pipeline orchestrates the post-scrape work that the Node pipeline used
// to own. This file ports the scoring phase (lib/pipeline/scoring.js): it scores
// unscored jobs up to the remaining daily Gemini quota, with bounded concurrency,
// auto-archiving low scores and draining early when the quota is exhausted.
package pipeline

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"job-search-automation/internal/db"
	"job-search-automation/internal/jobfilter"
	"job-search-automation/internal/metrics"
	"job-search-automation/internal/scorer"
)

// quotaReserve is held back from the daily limit for the run summary and retries,
// matching the "- 10" in lib/pipeline/scoring.js.
const quotaReserve = 10

// quotaRe detects a quota/billing exhaustion message so the loop can stop
// spending calls, mirroring QUOTA_RE in lib/pipeline/scoring.js.
var quotaRe = regexp.MustCompile(`(?i)exceeded your current quota|RESOURCE_EXHAUSTED|quota.*exceeded|billing.*quota`)

// HostQuotaAvailable reports whether the shared host key has daily budget left
// for a NON-scoring caller (the market research cron) to spend. It mirrors the
// ceiling ScoreUnscored enforces at scoring.go's hostRemaining check, including
// quotaReserve, so a secondary consumer cannot quietly spend the key past the
// provider quota and leave scoring to rediscover the wall through 429 retries.
//
// hostUsed is returned for logging even when the call is allowed. A hostDailyLimit
// of <= 0 means the host ceiling is unconfigured (self-host, where the per-tenant
// DailyLimit governs instead), so the caller is allowed through unchanged.
func HostQuotaAvailable(repo *db.Repository, hostDailyLimit int) (bool, int, error) {
	if hostDailyLimit <= 0 {
		return true, 0, nil
	}
	hostUsed, err := repo.HostDailyAPIUsage(scorer.Model)
	if err != nil {
		return false, 0, err
	}
	metrics.SetGeminiHostUsage(hostUsed)
	return hostDailyLimit-hostUsed-quotaReserve > 0, hostUsed, nil
}

// JobScorer is the scoring capability ScoreUnscored needs (scorer.Scorer
// satisfies it). Kept as an interface so the orchestrator is testable with a fake.
// ScoreJobs scores a whole batch in one call and returns results in input order;
// a returned error means the entire batch failed.
type JobScorer interface {
	ScoreJobs(ctx context.Context, jobs []scorer.Job) ([]scorer.Result, error)
}

// PrefilterFunc is a deterministic pre-score gate: it sees a job's location and
// title and reports whether the job is worth a Gemini call. A false result
// carries a jobfilter reason ("non_us"/"location") and the job is archived with
// the matching prefilter archive_reason instead of scored.
type PrefilterFunc func(location, title string) (ok bool, reason string)

// ScoreConfig carries the scoring knobs (from config.Config).
type ScoreConfig struct {
	DailyLimit       int
	Concurrency      int
	ArchiveThreshold int
	// BatchSize is how many jobs are scored per Gemini call. A value < 1 is
	// treated as 1 (one job per call).
	BatchSize int
	// HostKey is true when this run scores on the shared host key. When set, the
	// run's budget is also capped by the host key's global daily ceiling
	// (HostDailyLimit) so one tenant can't exhaust the shared key.
	HostKey        bool
	HostDailyLimit int
	// Prefilter, when non-nil, archives location-ineligible jobs before any
	// Gemini call is spent on them. Nil (the default) scores everything.
	Prefilter PrefilterFunc
	// UseExtracts scores jobs against their cached global_job_extracts facts
	// instead of the full description when a current extract exists. Jobs
	// without one always fall back to the description path, so an extraction
	// backlog never blocks scoring.
	UseExtracts bool
	// ExtractBatchSize is the per-call batch size for extract-backed jobs; the
	// ~10x smaller per-job payload can tolerate more jobs per call than the
	// description path. A value < 1 falls back to BatchSize.
	ExtractBatchSize int
	// ResumeHash is stamped onto each scored row so a later resume edit can
	// invalidate exactly the scores computed against the old resume. Empty
	// skips stamping.
	ResumeHash string
	// HostPerTenantDailyLimit caps how many host-key calls a SINGLE tenant may make
	// per day, applied only on host-key runs. It is the public-signup cost control:
	// without it one public user could drain the whole shared HostDailyLimit and
	// starve everyone else. A value <= 0 disables it (self-host default), so the
	// single-tenant path is unchanged.
	HostPerTenantDailyLimit int
	// MaxJobs caps how many jobs this pass may pull regardless of remaining
	// quota. The activity gate sets it for inactive tenants (keep-warm top-N):
	// with GetUnscoredJobs ordering newest-first, the cap IS the top-N. A value
	// <= 0 disables it (full scoring).
	MaxJobs int
	// InactiveDailyLimit is the DAILY host-key call budget for a tenant the
	// activity gate marked inactive; it replaces HostPerTenantDailyLimit for that
	// tenant when tighter. MaxJobs alone cannot bound keep-warm spend, because it
	// caps a single pass and the pipeline runs many passes a day: at 8 scrape
	// cycles, a top-N of 100 lets ONE idle tenant spend 800 calls, so a handful of
	// them exhaust a shared key before an active tenant is reached. This is the
	// daily bound that makes keep-warm actually cheap. A value <= 0 disables it,
	// leaving self-host and single-tenant behavior unchanged.
	InactiveDailyLimit int
}

// ScoreResult summarizes a scoring pass for the run-summary log.
type ScoreResult struct {
	Scored       int
	ScoredOK     int
	ScoredFailed int
	// Prefiltered counts jobs the location prefilter archived without a call.
	Prefiltered int
}

// ScoreUnscored scores pending/unscored jobs up to the remaining daily quota.
// Quota is recomputed each call, so it is safe to invoke once per pipeline run.
func ScoreUnscored(ctx context.Context, repo *db.Repository, sc JobScorer, cfg ScoreConfig, log *slog.Logger) (ScoreResult, error) {
	usedToday, err := repo.DailyAPIUsage(scorer.Model)
	if err != nil {
		return ScoreResult{}, err
	}
	// Publish today's tenant usage even when we bail early below, so the quota
	// gauge stays fresh between scoring passes.
	metrics.SetGeminiUsage(repo.UserID(), usedToday)

	remaining := 0
	if !cfg.HostKey {
		remaining = cfg.DailyLimit - usedToday - quotaReserve
		if remaining <= 0 {
			log.Info("daily quota exhausted before scoring", "used_today", usedToday, "limit", cfg.DailyLimit)
			return ScoreResult{}, nil
		}
	} else {
		// On the shared host key, clamp to the host key's global remaining so this
		// tenant can't push the shared key past its provider quota. The generic
		// DailyLimit is the self-host/local cap; hosted shared-key runs use the
		// explicit host-key limits below instead.
		hostUsed, err := repo.HostDailyAPIUsage(scorer.Model)
		if err != nil {
			return ScoreResult{}, err
		}
		metrics.SetGeminiHostUsage(hostUsed)
		hostRemaining := cfg.HostDailyLimit - hostUsed - quotaReserve
		if hostRemaining <= 0 {
			log.Info("host-key daily quota exhausted before scoring", "host_used_today", hostUsed, "host_limit", cfg.HostDailyLimit)
			return ScoreResult{}, nil
		}
		remaining = hostRemaining

		// Per-tenant host-key cap: bound THIS tenant's share of the shared key so one
		// public user can't drain the global host budget. usedToday is already this
		// tenant's own tally (DailyAPIUsage is user_id-scoped), so the same number
		// gates each tenant independently. Disabled (<= 0) on self-host.
		//
		// An inactive tenant gets the tighter keep-warm daily budget, so idle
		// accounts cannot spend the shared key out from under the active ones.
		perTenantLimit, limitKind := cfg.HostPerTenantDailyLimit, "per_tenant"
		if cfg.InactiveDailyLimit > 0 && (perTenantLimit <= 0 || cfg.InactiveDailyLimit < perTenantLimit) {
			perTenantLimit, limitKind = cfg.InactiveDailyLimit, "inactive_keep_warm"
		}
		if perTenantLimit > 0 {
			tenantRemaining := perTenantLimit - usedToday - quotaReserve
			if tenantRemaining <= 0 {
				log.Info("per-tenant host-key quota exhausted before scoring",
					"used_today", usedToday, "per_tenant_limit", perTenantLimit, "limit_kind", limitKind)
				return ScoreResult{}, nil
			}
			if tenantRemaining < remaining {
				remaining = tenantRemaining
			}
		}
	}

	batchSize := cfg.BatchSize
	if batchSize < 1 {
		batchSize = 1
	}

	// remaining is a budget of calls; each batch is roughly one call, so we may
	// pull up to remaining*batchSize jobs for this run.
	jobLimit := remaining * batchSize
	if cfg.MaxJobs > 0 && cfg.MaxJobs < jobLimit {
		jobLimit = cfg.MaxJobs
	}

	jobs, err := repo.GetUnscoredJobs(jobLimit)
	if err != nil {
		return ScoreResult{}, err
	}
	if len(jobs) == 0 {
		return ScoreResult{}, nil
	}

	return scoreJobList(ctx, repo, sc, jobs, cfg, log,
		"used_today", usedToday, "remaining_quota", remaining), nil
}

// applyPrefilter archives location-ineligible jobs (attributably, via
// archive_reason) before any model spend and returns the survivors plus the
// archived count. Shared by the sync scorer and the batch submitter so neither
// path spends quota (or batch slots) on a job the tenant's saved prefs would
// never surface. A nil prefilter keeps every job.
func applyPrefilter(repo *db.Repository, jobs []db.UnscoredJob, prefilter PrefilterFunc, log *slog.Logger) ([]db.UnscoredJob, int) {
	if prefilter == nil {
		return jobs, 0
	}
	prefiltered := 0
	kept := jobs[:0:0]
	for _, j := range jobs {
		ok, reason := prefilter(j.Location, j.Title)
		if ok {
			kept = append(kept, j)
			continue
		}
		archiveReason := db.ArchiveReasonPrefilterLocation
		if reason == jobfilter.ReasonNonUS {
			archiveReason = db.ArchiveReasonPrefilterNonUS
		}
		archived, err := repo.ArchivePrefiltered(j.ID, archiveReason)
		if err != nil {
			log.Warn("prefilter archive failed", "id", j.ID, "error", err)
			kept = append(kept, j)
			continue
		}
		// The row's guards declined the archive, so it is a user-touched job
		// the prefilter has no say over. Keep it: dropping it here would strand
		// it unscored in every future pass, since the prefilter verdict on its
		// location never changes.
		if !archived {
			kept = append(kept, j)
			continue
		}
		metrics.IncJobsPrefiltered(reason)
		prefiltered++
	}
	return kept, prefiltered
}

// splitNoDescription partitions out listings whose description is empty: a
// bare title inflates the score (a lone "DevOps" title reads as an 8-9 with no
// body to check), so both scoring paths resolve these deterministically
// instead of sending them to the model.
func splitNoDescription(jobs []db.UnscoredJob) (scorable, noDesc []db.UnscoredJob) {
	scorable = jobs[:0:0]
	for _, j := range jobs {
		if strings.TrimSpace(j.Description) == "" {
			noDesc = append(noDesc, j)
			continue
		}
		scorable = append(scorable, j)
	}
	return scorable, noDesc
}

// noDescriptionSkipMsg is the deterministic resolution both paths stamp on a
// description-less listing (kept unscored for a later scrape to fill the body).
const noDescriptionSkipMsg = "Skipped: no job description available to score."

// scoreJobList runs the scoring loop over an already-selected job list: it
// partitions out description-less rows, batches the rest, scores with bounded
// concurrency, records results, auto-archives low scores, and drains early on
// quota exhaustion or context cancellation. Callers own budget math and job
// selection. extraLogAttrs are appended to the "scoring started" line.
func scoreJobList(ctx context.Context, repo *db.Repository, sc JobScorer, jobs []db.UnscoredJob, cfg ScoreConfig, log *slog.Logger, extraLogAttrs ...any) ScoreResult {
	start := time.Now()
	batchSize := cfg.BatchSize
	if batchSize < 1 {
		batchSize = 1
	}

	// Prefilter first: a location the tenant's saved prefs would never surface is
	// archived here (attributably, via archive_reason) instead of spending a
	// Gemini call scoring a job the user cannot see anyway.
	jobs, prefiltered := applyPrefilter(repo, jobs, cfg.Prefilter, log)

	// Partition out listings with no description: scoring them on the title alone
	// inflates the score (a bare DevOps title scores 8-9 with no body to check). They
	// are resolved deterministically below without spending a Gemini call.
	scorable, noDesc := splitNoDescription(jobs)

	// Extract-backed jobs swap the ~4k-token description for the cached facts
	// sheet and batch at their own (typically larger) size. Jobs without a
	// current extract stay on the description path, so an extraction backlog
	// never blocks scoring.
	facts := map[string]string{}
	if cfg.UseExtracts && len(scorable) > 0 {
		ids := make([]string, len(scorable))
		for i, j := range scorable {
			ids[i] = j.ID
		}
		var err error
		if facts, err = repo.ExtractFactsFor(ids, scorer.ExtractVersion); err != nil {
			log.Warn("extract facts lookup failed; scoring on descriptions", "error", err)
			facts = map[string]string{}
		}
	}
	withFacts := scorable[:0:0]
	withoutFacts := scorable[:0:0]
	for _, j := range scorable {
		if _, ok := facts[j.ID]; ok {
			withFacts = append(withFacts, j)
			continue
		}
		withoutFacts = append(withoutFacts, j)
	}
	extractBatchSize := cfg.ExtractBatchSize
	if extractBatchSize < 1 {
		extractBatchSize = batchSize
	}

	concurrency := cfg.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	threshold := cfg.ArchiveThreshold

	batches := append(chunkJobs(withFacts, extractBatchSize), chunkJobs(withoutFacts, batchSize)...)
	startAttrs := []any{"total", len(scorable), "skipped_no_description", len(noDesc),
		"prefiltered", prefiltered, "extract_backed", len(withFacts),
		"batches", len(batches), "batch_size", batchSize}
	log.Info("scoring started", append(startAttrs, extraLogAttrs...)...)

	var (
		wg             sync.WaitGroup
		mu             sync.Mutex
		scoredOK       int
		scoredFailed   int
		quotaExhausted atomic.Bool
		sem            = make(chan struct{}, concurrency)
	)

	fail := func(id, msg string) {
		if err := repo.MarkScoreFailure(id, msg); err != nil {
			log.Warn("mark score failure failed", "id", id, "error", err)
		}
		mu.Lock()
		scoredFailed++
		mu.Unlock()
		if quotaRe.MatchString(msg) {
			quotaExhausted.Store(true)
		}
	}

	// record applies one job's result: a nil score is a failure, otherwise the
	// score is stored and low scores auto-archived.
	record := func(job db.UnscoredJob, res scorer.Result) {
		if res.Score == nil {
			msg := res.Reasoning
			if msg == "" {
				msg = "Gemini returned an unparsable score."
			}
			fail(job.ID, msg)
			log.Error("scoring failed", "title", job.Title, "error", msg)
			return
		}
		if err := repo.UpdateJobScoreForResume(job.ID, *res.Score, res.Reasoning, cfg.ResumeHash); err != nil {
			log.Warn("update score failed", "id", job.ID, "error", err)
			return
		}
		if *res.Score <= threshold {
			if err := repo.AutoArchiveLowScore(job.ID, threshold); err != nil {
				log.Warn("auto-archive failed", "id", job.ID, "error", err)
			}
		}
		mu.Lock()
		scoredOK++
		mu.Unlock()
	}

	// A listing with no description cannot be evaluated; mark it skipped (no API
	// call) so it is not scored on its title alone, leaving it unscored for a later
	// scrape that fills the body to score properly.
	for _, j := range noDesc {
		fail(j.ID, noDescriptionSkipMsg)
	}

	for _, batch := range batches {
		// Stop dispatching new work once the quota is gone or the ctx is done.
		if quotaExhausted.Load() {
			break
		}
		select {
		case <-ctx.Done():
			quotaExhausted.Store(true)
		default:
		}
		if quotaExhausted.Load() {
			break
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(batch []db.UnscoredJob) {
			defer wg.Done()
			defer func() { <-sem }()

			if quotaExhausted.Load() {
				return
			}
			jobsArg := make([]scorer.Job, len(batch))
			for i, job := range batch {
				jobsArg[i] = scorer.Job{
					Title:        job.Title,
					Company:      job.Company,
					Location:     job.Location,
					Description:  job.Description,
					ExtractFacts: facts[job.ID],
				}
				if err := repo.MarkScoreAttempt(job.ID); err != nil {
					log.Warn("mark score attempt failed", "id", job.ID, "error", err)
				}
			}

			results, err := sc.ScoreJobs(ctx, jobsArg)
			if err != nil {
				// A batch-level error (network/quota) fails every job in the chunk.
				for _, job := range batch {
					fail(job.ID, err.Error())
				}
				log.Error("scoring batch failed", "size", len(batch), "error", err)
				return
			}
			for i, job := range batch {
				if i >= len(results) {
					fail(job.ID, "Gemini returned no result for this job.")
					continue
				}
				record(job, results[i])
			}
		}(batch)
	}

	wg.Wait()

	if quotaExhausted.Load() {
		log.Warn("scoring stopped early (quota exhausted or context cancelled)",
			"scored_ok", scoredOK, "scored_failed", scoredFailed)
	}
	log.Info("scoring complete", "scored_ok", scoredOK, "scored_failed", scoredFailed)

	metrics.ObserveScoringPass(scoredOK, scoredFailed, time.Since(start))
	return ScoreResult{
		Scored:       scoredOK + scoredFailed,
		ScoredOK:     scoredOK,
		ScoredFailed: scoredFailed,
		Prefiltered:  prefiltered,
	}
}

// chunkJobs splits jobs into batches of at most size.
func chunkJobs(jobs []db.UnscoredJob, size int) [][]db.UnscoredJob {
	if size < 1 {
		size = 1
	}
	batches := make([][]db.UnscoredJob, 0, (len(jobs)+size-1)/size)
	for i := 0; i < len(jobs); i += size {
		end := i + size
		if end > len(jobs) {
			end = len(jobs)
		}
		batches = append(batches, jobs[i:end])
	}
	return batches
}
