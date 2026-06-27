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

	geminiRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jsa_gemini_requests_total",
		Help: "Gemini generation attempts, labeled by operation, result, and HTTP status.",
	}, []string{"operation", "result", "status"})

	geminiRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "jsa_gemini_request_duration_seconds",
		Help:    "Wall-clock latency of one Gemini generation attempt.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
	}, []string{"operation", "result", "status"})

	geminiTokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jsa_gemini_tokens_total",
		Help: "Gemini token usage from usageMetadata, labeled by operation and token kind.",
	}, []string{"operation", "kind"})

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
		Help: "Pending jobs awaiting a score, per tenant (the scoring backlog).",
	}, []string{"user"})

	onboardedTenantsJobless = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "jsa_onboarded_tenants_jobless",
		Help: "Onboarded tenants (profile complete on disk) that own zero job rows. Invariant: should settle to 0 once the self-healing fan-out bootstraps them; a sustained nonzero means a tenant is stranded with no jobs (the new-tenant bootstrap deadlock).",
	})

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
		httpRequestDuration,
		dashboardFragmentDuration,
	)
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
// such as "200", "429", or "transport".
func ObserveGeminiRequest(operation, result, status string, d time.Duration) {
	operation = metricLabel(operation)
	result = metricLabel(result)
	status = metricLabel(status)
	geminiRequests.WithLabelValues(operation, result, status).Inc()
	geminiRequestDuration.WithLabelValues(operation, result, status).Observe(d.Seconds())
}

// ObserveGeminiTokens records successful Gemini usageMetadata token counts.
func ObserveGeminiTokens(operation string, prompt, cachedPrompt, candidates, total int) {
	operation = metricLabel(operation)
	addToken := func(kind string, n int) {
		if n > 0 {
			geminiTokens.WithLabelValues(operation, kind).Add(float64(n))
		}
	}
	addToken("prompt", prompt)
	addToken("cached_prompt", cachedPrompt)
	addToken("candidates", candidates)
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

// SetJobsPending records a tenant's current unscored-job backlog.
func SetJobsPending(user string, n int) {
	jobsPendingUnscored.WithLabelValues(user).Set(float64(n))
}

// SetOnboardedTenantsJobless publishes how many onboarded tenants currently own
// zero job rows. The background metrics loop recomputes it each tick.
func SetOnboardedTenantsJobless(n int) {
	onboardedTenantsJobless.Set(float64(n))
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
