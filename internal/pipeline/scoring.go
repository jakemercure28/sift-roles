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
	"job-search-automation/internal/metrics"
	"job-search-automation/internal/scorer"
)

// quotaReserve is held back from the daily limit for the run summary and retries,
// matching the "- 10" in lib/pipeline/scoring.js.
const quotaReserve = 10

// quotaRe detects a quota/billing exhaustion message so the loop can stop
// spending calls, mirroring QUOTA_RE in lib/pipeline/scoring.js.
var quotaRe = regexp.MustCompile(`(?i)exceeded your current quota|RESOURCE_EXHAUSTED|quota.*exceeded|billing.*quota`)

// JobScorer is the scoring capability ScoreUnscored needs (scorer.Scorer
// satisfies it). Kept as an interface so the orchestrator is testable with a fake.
// ScoreJobs scores a whole batch in one call and returns results in input order;
// a returned error means the entire batch failed.
type JobScorer interface {
	ScoreJobs(ctx context.Context, jobs []scorer.Job) ([]scorer.Result, error)
}

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
	// HostPerTenantDailyLimit caps how many host-key calls a SINGLE tenant may make
	// per day, applied only on host-key runs. It is the public-signup cost control:
	// without it one public user could drain the whole shared HostDailyLimit and
	// starve everyone else. A value <= 0 disables it (self-host default), so the
	// single-tenant path is unchanged.
	HostPerTenantDailyLimit int
}

// ScoreResult summarizes a scoring pass for the run-summary log.
type ScoreResult struct {
	Scored       int
	ScoredOK     int
	ScoredFailed int
}

// ScoreUnscored scores pending/unscored jobs up to the remaining daily quota.
// Quota is recomputed each call, so it is safe to invoke once per pipeline run.
func ScoreUnscored(ctx context.Context, repo *db.Repository, sc JobScorer, cfg ScoreConfig, log *slog.Logger) (ScoreResult, error) {
	start := time.Now()
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
		if cfg.HostPerTenantDailyLimit > 0 {
			tenantRemaining := cfg.HostPerTenantDailyLimit - usedToday - quotaReserve
			if tenantRemaining <= 0 {
				log.Info("per-tenant host-key quota exhausted before scoring", "used_today", usedToday, "per_tenant_limit", cfg.HostPerTenantDailyLimit)
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

	jobs, err := repo.GetUnscoredJobs(jobLimit)
	if err != nil {
		return ScoreResult{}, err
	}
	if len(jobs) == 0 {
		return ScoreResult{}, nil
	}

	// Partition out listings with no description: scoring them on the title alone
	// inflates the score (a bare DevOps title scores 8-9 with no body to check). They
	// are resolved deterministically below without spending a Gemini call.
	scorable := jobs[:0:0]
	var noDesc []db.UnscoredJob
	for _, j := range jobs {
		if strings.TrimSpace(j.Description) == "" {
			noDesc = append(noDesc, j)
			continue
		}
		scorable = append(scorable, j)
	}

	concurrency := cfg.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	threshold := cfg.ArchiveThreshold

	batches := chunkJobs(scorable, batchSize)
	log.Info("scoring started", "total", len(scorable), "skipped_no_description", len(noDesc),
		"batches", len(batches), "batch_size", batchSize, "used_today", usedToday, "remaining_quota", remaining)

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
		if err := repo.UpdateJobScore(job.ID, *res.Score, res.Reasoning); err != nil {
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
		fail(j.ID, "Skipped: no job description available to score.")
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
					Title:       job.Title,
					Company:     job.Company,
					Location:    job.Location,
					Description: job.Description,
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
	}, nil
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
