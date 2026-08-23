package pipeline

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"job-search-automation/internal/db"
	"job-search-automation/internal/metrics"
	"job-search-automation/internal/scorer"
)

// JobExtractor is the extract-once capability (scorer.Extractor satisfies it).
type JobExtractor interface {
	ExtractJob(ctx context.Context, job scorer.Job) (string, error)
}

// ExtractConfig carries the extract-once knobs.
type ExtractConfig struct {
	// DailyLimit caps extraction calls per UTC day across the whole instance
	// (the ledger is the global extracts table itself, not a per-tenant tally).
	DailyLimit int
	// Concurrency bounds parallel extraction calls; the process-global Gemini
	// pacer still spaces the actual requests.
	Concurrency int
}

// ExtractResult summarizes one extraction pass.
type ExtractResult struct {
	Extracted int
	Failed    int
}

// ExtractPending extracts facts for global jobs backing the calling tenant's
// pending unscored rows, once per global job. It runs before the tenant's
// scoring pass so fresh listings have facts ready by the time they are scored;
// jobs that miss the budget window simply score via the full-description path
// and are picked up by a later pass. Cross-tenant dedup is structural: the
// first tenant to need a posting writes its extract into the global table and
// every later tenant's PendingExtractTargets no longer returns it.
func ExtractPending(ctx context.Context, repo *db.Repository, ex JobExtractor, cfg ExtractConfig, log *slog.Logger) (ExtractResult, error) {
	usedToday, err := repo.CountExtractsToday()
	if err != nil {
		return ExtractResult{}, err
	}
	remaining := cfg.DailyLimit - usedToday
	if remaining <= 0 {
		log.Info("extract budget exhausted before pass", "used_today", usedToday, "limit", cfg.DailyLimit)
		return ExtractResult{}, nil
	}

	targets, err := repo.PendingExtractTargets(remaining, scorer.ExtractVersion)
	if err != nil {
		return ExtractResult{}, err
	}
	if len(targets) == 0 {
		return ExtractResult{}, nil
	}

	concurrency := cfg.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}

	start := time.Now()
	log.Info("extraction started", "targets", len(targets), "used_today", usedToday, "remaining_budget", remaining)

	var (
		wg             sync.WaitGroup
		mu             sync.Mutex
		extracted      int
		failed         int
		quotaExhausted atomic.Bool
		sem            = make(chan struct{}, concurrency)
	)
	for _, t := range targets {
		if quotaExhausted.Load() || ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(t db.ExtractTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			if quotaExhausted.Load() {
				return
			}
			facts, err := ex.ExtractJob(ctx, scorer.Job{
				Title:       t.Title,
				Company:     t.Company,
				Location:    t.Location,
				Description: t.Description,
			})
			if err != nil {
				if quotaRe.MatchString(err.Error()) {
					quotaExhausted.Store(true)
				}
				log.Warn("extract failed", "global_job_id", t.GlobalJobID, "error", err)
				mu.Lock()
				failed++
				mu.Unlock()
				metrics.ObserveExtract(false)
				return
			}
			if err := repo.UpsertGlobalJobExtract(db.GlobalJobExtract{
				GlobalJobID:     t.GlobalJobID,
				DescriptionHash: t.DescriptionHash,
				ExtractVersion:  scorer.ExtractVersion,
				Facts:           facts,
				Model:           scorer.Model,
			}); err != nil {
				log.Warn("extract upsert failed", "global_job_id", t.GlobalJobID, "error", err)
				mu.Lock()
				failed++
				mu.Unlock()
				metrics.ObserveExtract(false)
				return
			}
			mu.Lock()
			extracted++
			mu.Unlock()
			metrics.ObserveExtract(true)
		}(t)
	}
	wg.Wait()

	if quotaExhausted.Load() {
		log.Warn("extraction stopped early (quota exhausted)", "extracted", extracted, "failed", failed)
	}
	log.Info("extraction complete", "extracted", extracted, "failed", failed, "took", time.Since(start).Round(time.Second))
	return ExtractResult{Extracted: extracted, Failed: failed}, nil
}
