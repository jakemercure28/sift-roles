package pipeline

import (
	"context"
	"log/slog"

	"job-search-automation/internal/db"
	"job-search-automation/internal/scorer"
)

// BurstConfig carries the onboarding-burst knobs.
type BurstConfig struct {
	// Limit is how many jobs the burst may score. It is the burst's whole budget:
	// the per-tenant fairness cap is deliberately not applied (see
	// ScoreOnboardingBurst), so Limit is also the most a single signup can spend.
	Limit int
}

// ScoreOnboardingBurst scores a brand-new tenant's best-matching pending jobs
// immediately, so the first thing they see after onboarding is scored roles
// rather than a blank queue waiting for the scoring cron.
//
// Budget semantics differ from ScoreUnscored on purpose:
//   - The per-tenant fairness cap (HostPerTenantDailyLimit) and the quotaReserve
//     are skipped. They exist to stop an established tenant from draining the
//     shared key over a full day; applied here they could zero-score a signup,
//     which is the one moment the product must not look empty. Limit bounds the
//     spend instead.
//   - The host-global ceiling (HostDailyLimit) still applies: it tracks the real
//     provider quota across both keys and must never be crossed.
//
// keywords are the tenant's resume-derived target-title tokens; the burst scores
// the strongest title matches first so the likeliest 7-9s land earliest. Usage
// accounting is unchanged: the scorer's client records every call against the
// tenant and host ledgers, so later cron passes see the burst's spend.
func ScoreOnboardingBurst(ctx context.Context, repo *db.Repository, sc JobScorer, keywords []string, cfg ScoreConfig, burst BurstConfig, log *slog.Logger) (ScoreResult, error) {
	budget := burst.Limit
	if budget <= 0 {
		return ScoreResult{}, nil
	}
	if cfg.HostKey {
		hostUsed, err := repo.HostDailyAPIUsage(scorer.Model)
		if err != nil {
			return ScoreResult{}, err
		}
		if hostRemaining := cfg.HostDailyLimit - hostUsed; hostRemaining < budget {
			budget = hostRemaining
		}
		if budget <= 0 {
			log.Warn("onboarding burst skipped: host-key daily quota exhausted",
				"host_used_today", hostUsed, "host_limit", cfg.HostDailyLimit)
			return ScoreResult{}, nil
		}
	}

	jobs, err := repo.TopPendingByKeywords(budget, keywords)
	if err != nil {
		return ScoreResult{}, err
	}
	if len(jobs) == 0 {
		log.Info("onboarding burst found no scorable pending jobs")
		return ScoreResult{}, nil
	}

	// One job per call: the burst is small and latency-sensitive, and single-job
	// prompts score more accurately than crowded batches.
	burstCfg := cfg
	burstCfg.BatchSize = 1

	res := scoreJobList(ctx, repo, sc, jobs, burstCfg, log, "onboarding_burst", true)
	log.Info("onboarding burst complete",
		"scored_ok", res.ScoredOK, "scored_failed", res.ScoredFailed, "budget", budget)
	return res, nil
}
