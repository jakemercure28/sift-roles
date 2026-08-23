// Package metrics is the single source of truth for the Go backend's Prometheus
// instrumentation. It owns one private registry (so test binaries and the default
// global registry stay clean), registers the standard Go runtime and process
// collectors, and exposes a handful of typed helpers the rest of the codebase
// calls to emit business metrics: scrape cycles, scoring throughput, Gemini quota
// usage, the unscored backlog, and HTTP request latency.
//
// All series are prefixed jsa_ so they group cleanly in Prometheus/Grafana and
// never collide with exporter-provided metrics (node, cadvisor, go_*, process_*).
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// registry holds every collector this package exports. It is private so callers
// go through the typed helpers below rather than registering ad-hoc series.
var registry = prometheus.NewRegistry()

var (
	scrapeCycles = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jsa_scrape_cycles_total",
		Help: "Scrape-insert cycles completed, labeled by result (ok|error).",
	}, []string{"result"})

	scrapeCycleDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "jsa_scrape_cycle_duration_seconds",
		Help:    "Wall-clock duration of one tenant's scrape-insert cycle.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 12), // 1s .. ~68m
	})

	scrapedJobs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jsa_scraped_jobs_total",
		Help: "New scraped jobs inserted, labeled by source platform.",
	}, []string{"platform"})

	atsResolutions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jsa_ats_resolutions_total",
		Help: "Alternate ATS resolution outcomes, labeled by action and platform transition.",
	}, []string{"action", "from_platform", "to_platform"})

	jobsScored = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jsa_jobs_scored_total",
		Help: "Jobs processed by the scorer, labeled by result (ok|failed).",
	}, []string{"result"})

	jobsPrefiltered = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jsa_jobs_prefiltered_total",
		Help: "Jobs archived by the pre-score location prefilter (no Gemini call spent), labeled by reason (non_us|location).",
	}, []string{"reason"})

	jobExtracts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jsa_job_extracts_total",
		Help: "Extract-once results: one LLM fact extraction per distinct global job, labeled by result (ok|failed).",
	}, []string{"result"})

	globalBoardsScraped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jsa_global_boards_scraped_total",
		Help: "Boards fetched by the global-first board pass (the cross-tenant union, each board once per cycle).",
	})

	globalLeadsHarvested = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jsa_global_leads_harvested_total",
		Help: "Leads harvested into global_jobs by the global board pass.",
	})

	jobsDistributed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jsa_jobs_distributed_total",
		Help: "Tenant job rows inserted by distribution from the global cache (instead of a per-tenant scrape).",
	})

	jobsClosedByDiff = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jsa_jobs_closed_board_diff_total",
		Help: "Tenant job rows marked closed because their posting vanished from a successfully fetched board.",
	})

	boardsSkippedUnchanged = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jsa_boards_skipped_unchanged_total",
		Help: "Boards the global pass skipped because their ETag/content hash showed no change since the last cycle.",
	})

	globalBoardUnionSize = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "jsa_global_board_union_size",
		Help: "Boards in the distinct-board union the last global pass set out to scrape (the coverage target).",
	})

	globalBoardCompanies = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jsa_global_board_companies",
		Help: "Distinct companies that returned at least one lead in the last global pass, per platform. A sharp drop vs the registry size flags a starved/blocked platform.",
	}, []string{"platform"})

	globalBoardIncomplete = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jsa_global_board_incomplete",
		Help: "Companies whose board fetch the last global pass reported incomplete (blocked/429/truncated), per platform.",
	}, []string{"platform"})

	globalBoardPassBoards = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jsa_global_board_pass_boards",
		Help: "Boards the last global pass fetched, by outcome (ok|unchanged|failed), per platform. Unlike jsa_global_board_companies this counts boards that answered even when the incremental fetch found no change, so it stays high on a healthy platform whose boards are mostly cached; a collapse means the platform is starved out of the union or its adapter is blocked.",
	}, []string{"platform", "status"})

	globalBoardScrapeTruncated = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jsa_global_board_scrape_truncated_total",
		Help: "Global board passes that ended early because the board-scrape sub-budget (cycle deadline minus distribution reserve) expired before every board was fetched.",
	})

	tenantDistributionInserted = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jsa_tenant_distribution_inserted",
		Help: "Rows the most recent distribution inserted into each tenant's queue.",
	}, []string{"user"})

	tenantLastDistribution = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jsa_tenant_last_distribution_timestamp_seconds",
		Help: "Unix time of each tenant's last completed distribution. time() minus this is staleness: a growing gap means that tenant's queue is being starved.",
	}, []string{"user"})

	geminiRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jsa_gemini_requests_total",
		Help: "Gemini generation attempts, labeled by operation, result, HTTP status, and which API key served it (primary|overflow).",
	}, []string{"operation", "result", "status", "key"})

	geminiRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "jsa_gemini_request_duration_seconds",
		Help:    "Wall-clock latency of one Gemini generation attempt.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
	}, []string{"operation", "result", "status", "key"})

	geminiTokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jsa_gemini_tokens_total",
		Help: "Gemini token usage from usageMetadata, labeled by operation, token kind, and which API key served it (primary|overflow).",
	}, []string{"operation", "kind", "key"})

	scoringPassDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "jsa_scoring_pass_duration_seconds",
		Help:    "Wall-clock duration of one ScoreUnscored pass.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 12),
	})

	geminiDailyUsage = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jsa_gemini_daily_usage",
		Help: "Gemini calls made today by a single tenant (local-date keyed).",
	}, []string{"user"})

	geminiHostDailyUsage = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "jsa_gemini_host_daily_usage",
		Help: "Gemini calls made today across all tenants on the shared host key.",
	})

	geminiDailyTokens = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jsa_gemini_daily_tokens",
		Help: "Gemini tokens used today by a single tenant, labeled by token kind.",
	}, []string{"user", "kind"})

	geminiHostDailyTokens = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jsa_gemini_host_daily_tokens",
		Help: "Gemini tokens used today across all tenants on the shared host key, labeled by token kind.",
	}, []string{"kind"})

	jobsPendingUnscored = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jsa_jobs_pending_unscored",
		Help: "Pending jobs awaiting a score, per tenant (the scoring backlog). activity mirrors the scoring activity gate: only active tenants' backlog is drained by paid batch scoring, inactive tenants stay on the free keep-warm path.",
	}, []string{"user", "activity"})

	onboardedTenantsJobless = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "jsa_onboarded_tenants_jobless",
		Help: "Onboarded tenants (profile complete on disk) that own zero job rows. Invariant: should settle to 0 once the self-healing fan-out bootstraps them; a sustained nonzero means a tenant is stranded with no jobs (the new-tenant bootstrap deadlock).",
	})

	globalJobsTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "jsa_global_jobs_total",
		Help: "Total listings in the cross-tenant global_jobs cache (public direct-apply ATS leads; LinkedIn/manual excluded).",
	})

	globalJobsCompanies = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "jsa_global_jobs_companies",
		Help: "Distinct companies represented in the cross-tenant global_jobs cache.",
	})

	registryBoards = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jsa_registry_boards",
		Help: "Verified ATS boards in the global company_registry, per platform. This is the upstream inventory the per-tenant harvest adopts from; distinct from global_jobs, which holds listings.",
	}, []string{"platform"})

	geminiBatchDailyUsage = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "jsa_gemini_batch_daily_usage",
		Help: "Gemini Batch API scoring calls ledgered today (local-date keyed, cross-tenant; api_usage model key gemini-*-batch). Kept out of jsa_gemini_daily_usage / jsa_gemini_host_daily_usage, which back the sync-quota panels; add the two series for total spend.",
	})

	geminiBatchDailyTokens = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jsa_gemini_batch_daily_tokens",
		Help: "Gemini Batch API tokens ledgered today by kind (cross-tenant, billed at the 50%-off batch rate). Separate from the sync-keyed token gauges so quota panels stay honest; add the series for total token spend.",
	}, []string{"kind"})

	scoreBatchesOpen = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "jsa_score_batches_open",
		Help: "Gemini Batch API scoring batches currently awaiting results (cross-tenant), excluding rows older than the eviction bound (~72h) since those no longer hold a submit slot. Bounded by BATCH_API_MAX_OPEN_BATCHES; stuck near the cap with no applies means the poller is failing or batches are expiring.",
	})

	scoreBatchJobsToday = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "jsa_score_batch_jobs_today",
		Help: "Jobs submitted to the Gemini Batch API this UTC day (cross-tenant, any batch state). Bounded by BATCH_API_DAILY_JOB_LIMIT; the paid-key batch spend throttle.",
	})

	scoreBatchOldestOpenAge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "jsa_score_batch_oldest_open_age_seconds",
		Help: "Age of the oldest score batch still awaiting results, 0 when none are open. Deliberately not age-bounded: sustained growth past the ~48h upstream expiry means the poll loop is failing to close batches, and past 72h the evictor should have fired; inspect with `server batch list`.",
	})

	scoreBatchQuotaTrips = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jsa_score_batch_quota_trips_total",
		Help: "Batch submit passes aborted by a quota-exhausted (429) submit, each starting a BATCH_API_QUOTA_COOLDOWN_MINUTES submit pause. Any increase means the paid key is out of quota or billing credit; a steady climb means it never recovered.",
	})

	activeUsers = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jsa_active_users",
		Help: "Distinct tenants who opened their dashboard within the labeled trailing window (from metadata.dashboard_last_seen). Real engagement signal: counts anyone who loaded the dashboard, not only those who moved a job through a stage.",
	}, []string{"window"})

	httpRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "jsa_http_request_duration_seconds",
		Help:    "Dashboard HTTP request latency, labeled by route pattern, method, and status code.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
	}, []string{"route", "method", "code"})

	dashboardFragmentDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "jsa_dashboard_fragment_duration_seconds",
		Help:    "Dashboard fragment build latency, labeled by dashboard filter, build phase, and cache status.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
	}, []string{"filter", "phase", "cache"})

	opsAgentRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jsa_opsagent_runs_total",
		Help: "Self-healing agent investigations, labeled by trigger (alert|sweep) and verdict (remediated|escalated|escalation_failed|refused|error).",
	}, []string{"trigger", "verdict"})

	opsAgentRunDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "jsa_opsagent_run_duration_seconds",
		Help:    "Wall-clock duration of one self-healing agent investigation.",
		Buckets: []float64{5, 15, 30, 60, 120, 300, 600, 1200},
	})

	opsAgentActions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jsa_opsagent_actions_total",
		Help: "Remediation actions the agent considered, labeled by action, namespace, and outcome (executed|dry_run|denied|failed). Denials and dry runs are counted deliberately: what the agent wanted to do is as informative as what it did.",
	}, []string{"action", "namespace", "outcome"})

	opsAgentEscalations = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jsa_opsagent_escalations_total",
		Help: "Investigations handed to a human via a GitHub issue because no allowlisted remediation applied.",
	})

	opsAgentTokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jsa_opsagent_tokens_total",
		Help: "Claude tokens consumed by the self-healing agent, labeled by direction (input|output).",
	}, []string{"direction"})
)

func init() {
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		scrapeCycles,
		scrapeCycleDuration,
		scrapedJobs,
		atsResolutions,
		jobsScored,
		jobsPrefiltered,
		jobExtracts,
		globalBoardsScraped,
		globalLeadsHarvested,
		jobsDistributed,
		jobsClosedByDiff,
		boardsSkippedUnchanged,
		globalBoardUnionSize,
		globalBoardCompanies,
		globalBoardIncomplete,
		globalBoardPassBoards,
		globalBoardScrapeTruncated,
		tenantDistributionInserted,
		tenantLastDistribution,
		geminiRequests,
		geminiRequestDuration,
		geminiTokens,
		scoringPassDuration,
		geminiDailyUsage,
		geminiHostDailyUsage,
		geminiDailyTokens,
		geminiHostDailyTokens,
		jobsPendingUnscored,
		onboardedTenantsJobless,
		globalJobsTotal,
		globalJobsCompanies,
		registryBoards,
		geminiBatchDailyUsage,
		geminiBatchDailyTokens,
		scoreBatchesOpen,
		scoreBatchJobsToday,
		scoreBatchOldestOpenAge,
		scoreBatchQuotaTrips,
		activeUsers,
		httpRequestDuration,
		dashboardFragmentDuration,
		opsAgentRuns,
		opsAgentRunDuration,
		opsAgentActions,
		opsAgentEscalations,
		opsAgentTokens,
	)
}

// ObserveOpsAgentRun records one completed self-healing investigation.
func ObserveOpsAgentRun(trigger, verdict string, d time.Duration) {
	opsAgentRuns.WithLabelValues(metricLabel(trigger), metricLabel(verdict)).Inc()
	opsAgentRunDuration.Observe(d.Seconds())
}

// ObserveOpsAgentAction records one remediation the agent considered, including
// the ones it was not allowed to perform.
func ObserveOpsAgentAction(action, namespace, outcome string) {
	if namespace == "" {
		namespace = "none"
	}
	opsAgentActions.WithLabelValues(metricLabel(action), metricLabel(namespace), metricLabel(outcome)).Inc()
}

// ObserveOpsAgentEscalation records one investigation handed to a human.
func ObserveOpsAgentEscalation() {
	opsAgentEscalations.Inc()
}

// ObserveOpsAgentTokens records the token cost of one investigation.
func ObserveOpsAgentTokens(input, output int64) {
	opsAgentTokens.WithLabelValues("input").Add(float64(input))
	opsAgentTokens.WithLabelValues("output").Add(float64(output))
}

// Handler returns the /metrics HTTP handler bound to this package's registry.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}

// ObserveScrapeCycle records one completed scrape-insert cycle: its duration and
// whether it succeeded.
func ObserveScrapeCycle(ok bool, d time.Duration) {
	scrapeCycleDuration.Observe(d.Seconds())
	if ok {
		scrapeCycles.WithLabelValues("ok").Inc()
	} else {
		scrapeCycles.WithLabelValues("error").Inc()
	}
}

func metricLabel(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// ObserveScrapedJob records one newly inserted scraped job. The platform label
// is intentionally low-cardinality (Greenhouse, Workday, Built In, etc.).
func ObserveScrapedJob(platform string) {
	scrapedJobs.WithLabelValues(metricLabel(platform)).Inc()
}

// ObserveATSResolution records one ATS canonicalization outcome that used to
// appear in the Activity Log. Labels are limited to outcome/platform names.
func ObserveATSResolution(action, fromPlatform, toPlatform string) {
	atsResolutions.WithLabelValues(metricLabel(action), metricLabel(fromPlatform), metricLabel(toPlatform)).Inc()
}

// ObserveExtract counts one extract-once attempt.
func ObserveExtract(ok bool) {
	if ok {
		jobExtracts.WithLabelValues("ok").Inc()
		return
	}
	jobExtracts.WithLabelValues("failed").Inc()
}

// IncJobsPrefiltered counts one job the pre-score prefilter archived.
func IncJobsPrefiltered(reason string) {
	jobsPrefiltered.WithLabelValues(reason).Inc()
}

// ObserveGlobalScrape records one global board pass.
func ObserveGlobalScrape(boards, harvested int) {
	globalBoardsScraped.Add(float64(boards))
	globalLeadsHarvested.Add(float64(harvested))
}

// ObserveDistribution records one tenant's distribution step.
func ObserveDistribution(inserted, closedByDiff int) {
	jobsDistributed.Add(float64(inserted))
	jobsClosedByDiff.Add(float64(closedByDiff))
}

// SetGlobalBoardUnionSize records how many boards the last global pass targeted.
func SetGlobalBoardUnionSize(boards int) {
	globalBoardUnionSize.Set(float64(boards))
}

// SetGlobalBoardCoverage records, per platform, how many distinct companies
// returned leads and how many had an incomplete fetch in the last global pass.
// Reset first so a platform that returns nothing this cycle reads as 0, not a
// stale value.
func SetGlobalBoardCoverage(companies, incomplete map[string]int) {
	globalBoardCompanies.Reset()
	for platform, n := range companies {
		globalBoardCompanies.WithLabelValues(metricLabel(platform)).Set(float64(n))
	}
	globalBoardIncomplete.Reset()
	for platform, n := range incomplete {
		globalBoardIncomplete.WithLabelValues(metricLabel(platform)).Set(float64(n))
	}
}

// SetGlobalBoardPassOutcomes records, per platform, how many boards each fetch
// outcome (ok|unchanged|failed) covered in the last global pass. Reset first so
// a platform absent from this cycle's union reads as no series, not a stale
// value.
func SetGlobalBoardPassOutcomes(outcomes map[string]map[string]int) {
	globalBoardPassBoards.Reset()
	for platform, byStatus := range outcomes {
		for status, n := range byStatus {
			globalBoardPassBoards.WithLabelValues(metricLabel(platform), status).Set(float64(n))
		}
	}
}

// IncGlobalBoardScrapeTruncated counts one global pass cut short by its board
// sub-budget before every board was fetched.
func IncGlobalBoardScrapeTruncated() {
	globalBoardScrapeTruncated.Inc()
}

// SetTenantDistribution records a tenant's most recent distribution: how many
// rows it inserted and when it completed (for staleness alerting).
func SetTenantDistribution(user string, inserted int, at time.Time) {
	tenantDistributionInserted.WithLabelValues(metricLabel(user)).Set(float64(inserted))
	tenantLastDistribution.WithLabelValues(metricLabel(user)).Set(float64(at.Unix()))
}

// AddBoardsSkippedUnchanged counts boards skipped by the incremental fetch.
func AddBoardsSkippedUnchanged(n int) {
	boardsSkippedUnchanged.Add(float64(n))
}

// ObserveScoringPass records one ScoreUnscored pass: its duration and the
// ok/failed job tallies.
func ObserveScoringPass(scoredOK, scoredFailed int, d time.Duration) {
	scoringPassDuration.Observe(d.Seconds())
	if scoredOK > 0 {
		jobsScored.WithLabelValues("ok").Add(float64(scoredOK))
	}
	if scoredFailed > 0 {
		jobsScored.WithLabelValues("failed").Add(float64(scoredFailed))
	}
}

// ObserveGeminiRequest records one Gemini HTTP attempt. status is a bounded label
// such as "200", "429", or "transport"; key is "primary" or "overflow".
func ObserveGeminiRequest(operation, result, status, key string, d time.Duration) {
	operation = metricLabel(operation)
	result = metricLabel(result)
	status = metricLabel(status)
	key = metricLabel(key)
	geminiRequests.WithLabelValues(operation, result, status, key).Inc()
	geminiRequestDuration.WithLabelValues(operation, result, status, key).Observe(d.Seconds())
}

// ObserveGeminiTokens records successful Gemini usageMetadata token counts.
// key is "primary" or "overflow", so paid-spillover token spend is visible.
func ObserveGeminiTokens(operation, key string, prompt, cachedPrompt, candidates, thoughts, total int) {
	operation = metricLabel(operation)
	key = metricLabel(key)
	addToken := func(kind string, n int) {
		if n > 0 {
			geminiTokens.WithLabelValues(operation, kind, key).Add(float64(n))
		}
	}
	addToken("prompt", prompt)
	addToken("cached_prompt", cachedPrompt)
	addToken("candidates", candidates)
	// Thought tokens are billed as output and included in total but NOT in
	// candidates; tracked as their own kind so hidden reasoning spend is
	// visible on the token panels.
	addToken("thoughts", thoughts)
	addToken("total", total)
}

// SetGeminiUsage records a tenant's Gemini call count for today.
func SetGeminiUsage(user string, used int) {
	geminiDailyUsage.WithLabelValues(user).Set(float64(used))
}

// SetGeminiHostUsage records today's global host-key Gemini call count.
func SetGeminiHostUsage(used int) {
	geminiHostDailyUsage.Set(float64(used))
}

// SetGeminiTokenUsage records a tenant's daily token totals.
func SetGeminiTokenUsage(user string, prompt, cachedPrompt, candidates, total int) {
	setTokenGauges(geminiDailyTokens.WithLabelValues, user, prompt, cachedPrompt, candidates, total)
}

// SetGeminiHostTokenUsage records the shared host key's daily token totals.
func SetGeminiHostTokenUsage(prompt, cachedPrompt, candidates, total int) {
	setHostTokenGauges(prompt, cachedPrompt, candidates, total)
}

func setTokenGauges(with func(lvs ...string) prometheus.Gauge, user string, prompt, cachedPrompt, candidates, total int) {
	setGauge := func(kind string, n int) {
		with(user, kind).Set(float64(n))
	}
	setGauge("prompt", prompt)
	setGauge("cached_prompt", cachedPrompt)
	setGauge("candidates", candidates)
	setGauge("total", total)
}

func setHostTokenGauges(prompt, cachedPrompt, candidates, total int) {
	geminiHostDailyTokens.WithLabelValues("prompt").Set(float64(prompt))
	geminiHostDailyTokens.WithLabelValues("cached_prompt").Set(float64(cachedPrompt))
	geminiHostDailyTokens.WithLabelValues("candidates").Set(float64(candidates))
	geminiHostDailyTokens.WithLabelValues("total").Set(float64(total))
}

// SetJobsPending records a tenant's current unscored-job backlog, labeled by
// the scoring activity gate's verdict. A tenant flipping between active and
// inactive must not leave a stale series behind under the old label, so the
// opposite activity series is deleted on every set.
func SetJobsPending(user string, n int, active bool) {
	act, stale := "active", "inactive"
	if !active {
		act, stale = "inactive", "active"
	}
	jobsPendingUnscored.WithLabelValues(user, act).Set(float64(n))
	jobsPendingUnscored.DeleteLabelValues(user, stale)
}

// SetOnboardedTenantsJobless publishes how many onboarded tenants currently own
// zero job rows. The background metrics loop recomputes it each tick.
func SetOnboardedTenantsJobless(n int) {
	onboardedTenantsJobless.Set(float64(n))
}

// SetGlobalJobsCache publishes the size of the cross-tenant global_jobs cache:
// total cached listings and the distinct company count. The background metrics
// loop recomputes both once per tick (the table is global, not per tenant).
func SetGlobalJobsCache(rows, companies int) {
	globalJobsTotal.Set(float64(rows))
	globalJobsCompanies.Set(float64(companies))
}

// SetRegistryBoards publishes the per-platform board counts of the global
// company_registry. Reset before set so a platform that drops to zero rows
// does not keep reporting its last nonzero value.
func SetRegistryBoards(counts map[string]int) {
	registryBoards.Reset()
	for platform, n := range counts {
		registryBoards.WithLabelValues(metricLabel(platform)).Set(float64(n))
	}
}

// SetGeminiBatchUsage publishes today's ledgered Batch API call count and
// token totals (cross-tenant; the api_usage rows keyed by the batch model).
// The background metrics loop recomputes them once per tick.
func SetGeminiBatchUsage(used, prompt, cachedPrompt, candidates, total int) {
	geminiBatchDailyUsage.Set(float64(used))
	geminiBatchDailyTokens.WithLabelValues("prompt").Set(float64(prompt))
	geminiBatchDailyTokens.WithLabelValues("cached_prompt").Set(float64(cachedPrompt))
	geminiBatchDailyTokens.WithLabelValues("candidates").Set(float64(candidates))
	geminiBatchDailyTokens.WithLabelValues("total").Set(float64(total))
}

// SetScoreBatchState publishes the batch-scoring loop's gauges: open batches
// awaiting results, jobs submitted this UTC day, and the oldest open batch's
// age in seconds (0 when none are open), all cross-tenant. The background
// metrics loop recomputes them once per tick.
func SetScoreBatchState(open, jobsToday int, oldestOpenAgeSeconds float64) {
	scoreBatchesOpen.Set(float64(open))
	scoreBatchJobsToday.Set(float64(jobsToday))
	scoreBatchOldestOpenAge.Set(oldestOpenAgeSeconds)
}

// IncScoreBatchQuotaTrip counts one submit pass aborted by a quota-exhausted
// (429) batch submit; each trip starts a submit cooldown.
func IncScoreBatchQuotaTrip() {
	scoreBatchQuotaTrips.Inc()
}

// SetActiveUsers publishes the count of distinct tenants seen within one trailing
// window (e.g. "1d", "7d", "30d"). The background metrics loop recomputes each
// window per tick from metadata.dashboard_last_seen.
func SetActiveUsers(window string, n int) {
	activeUsers.WithLabelValues(window).Set(float64(n))
}

// ObserveHTTP records one dashboard request's latency. route is the matched
// ServeMux pattern (low cardinality); callers pass "unmatched" when no route bound.
func ObserveHTTP(route, method, code string, d time.Duration) {
	httpRequestDuration.WithLabelValues(route, method, code).Observe(d.Seconds())
}

// ObserveDashboardFragment records the time spent building one dashboard
// fragment phase. filter is one of the dashboard's bounded filter ids; phase and
// cache are low-cardinality literals supplied by the dashboard package.
func ObserveDashboardFragment(filter, phase, cache string, d time.Duration) {
	dashboardFragmentDuration.WithLabelValues(metricLabel(filter), metricLabel(phase), metricLabel(cache)).Observe(d.Seconds())
}
