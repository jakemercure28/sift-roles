// Package config loads the Go backend's runtime configuration from the
// environment, mirroring the path conventions in config/paths.js so the Go
// service and the Node app resolve the same jobs.db.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the resolved runtime configuration.
type Config struct {
	// ScraperURL is the base URL of the TypeScript scraper worker.
	ScraperURL string
	// ScrapeTimeout bounds a single /scrape request to the worker and a
	// per-tenant triggered scrape (SCRAPE_TIMEOUT_MINUTES, default 10).
	ScrapeTimeout time.Duration
	// GlobalScrapeTimeout bounds a full scrape cycle (empty-user trigger or
	// startup scrape): the cross-tenant board union in global mode can span
	// hundreds of boards at polite pacing, so it needs a far larger budget
	// than one worker request (GLOBAL_SCRAPE_TIMEOUT_MINUTES, default 60).
	GlobalScrapeTimeout time.Duration
	// GlobalDistributeReserve is how much of GlobalScrapeTimeout is held back
	// from the board scrape so per-tenant distribution/scoring always runs. The
	// board scrape and distribution share one cycle context; a bloated union
	// (folded registry) can otherwise consume the whole budget and leave
	// distribution an already-expired context, starving every tenant's queue
	// (GLOBAL_DISTRIBUTE_RESERVE_MINUTES, default 15). 0 disables the reserve.
	GlobalDistributeReserve time.Duration
	// DataDir mirrors DATA_DIR in config/paths.js (baseDir).
	DataDir string
	// ContextDir is the .context directory mounted for generated assistant
	// context files.
	ContextDir string
	// DBDir mirrors DB_DIR (falls back to DataDir).
	DBDir string
	// DBPath is DBDir/jobs.db — the shared SQLite database.
	DBPath string
	// DBType selects the storage backend: "sqlite" (default, self-host) or
	// "postgres" (hosted multi-tenant SaaS). Set via DATABASE_TYPE.
	DBType string
	// DatabaseURL is the Postgres DSN used when DBType is "postgres"
	// (DATABASE_URL, e.g. postgres://user:pass@host:6543/db?sslmode=require).
	DatabaseURL string
	// Timezone is the IANA name (APP_TIMEZONE, e.g. "America/Los_Angeles") used to
	// resolve "local day" boundaries on the Postgres backend — notably the Gemini
	// daily-quota window, which Google resets at midnight Pacific. Empty means "use
	// the backend default" (UTC on Supabase), which is the pre-fix behavior. Only a
	// time.LoadLocation-valid name is ever stored here, since it is interpolated
	// into SQL by the dialect rewriter.
	Timezone string
	// DBMaxOpenConns / DBMaxIdleConns / DBConnMaxLifetime / DBConnMaxIdleTime
	// tune the Postgres connection pool (DB_MAX_OPEN_CONNS, DB_MAX_IDLE_CONNS,
	// DB_CONN_MAX_LIFETIME_MS, DB_CONN_MAX_IDLE_TIME_MS). Defaults are sized for
	// a Supavisor/PgBouncer transaction pooler on :6543; ignored by SQLite.
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxLifetime time.Duration
	DBConnMaxIdleTime time.Duration
	// SupabaseURL is the hosted Supabase project URL (SUPABASE_URL). It backs the
	// JWKS endpoint and expected issuer for JWT verification, and is handed to the
	// browser so supabase-js can run Google OAuth. Empty disables hosted auth.
	SupabaseURL string
	// SupabaseAnonKey is the Supabase anon (publishable) key (SUPABASE_ANON_KEY),
	// injected into the dashboard page for supabase-js. Public by design.
	SupabaseAnonKey string
	// RLSEnforce activates Postgres row-level security as defense-in-depth on top
	// of the app-layer user_id scoping (RLS_ENFORCE, default false). When on, every
	// query runs inside a transaction that sets the app.user_id GUC the dormant
	// policies in 00002_rls.sql key on, so an accidental unscoped query fails closed
	// (returns zero rows) instead of leaking cross-tenant data. Requires a restricted
	// (non-BYPASSRLS) Postgres role; see RLSDatabaseURL. Postgres-only; SQLite ignores it.
	RLSEnforce bool
	// RLSDatabaseURL is the restricted-role Postgres DSN used when RLSEnforce is on
	// (RLS_DATABASE_URL). It must connect as a role that is NOT BYPASSRLS/superuser so
	// the policies actually apply. Empty falls back to DatabaseURL (useful for tests
	// where the test role is already restricted).
	RLSDatabaseURL string
	// ScrapeSchedule is the cron expression for the scheduled scrape.
	ScrapeSchedule string
	// ScrapeOnStart runs one scrape cycle immediately on startup (so a fresh
	// deploy populates without waiting for the first cron tick).
	ScrapeOnStart bool
	// TriggerAddr is the listen address for the on-demand scrape trigger server
	// (e.g. ":8090"). The dashboard posts to it to kick off a scrape manually.
	TriggerAddr string
	// MetricsAddr is the listen address for the Prometheus /metrics endpoint
	// (e.g. ":9090"). It is a dedicated listener kept off the dashboard mux so
	// metrics are never exposed through the public tunnel; Prometheus scrapes it
	// over a loopback-published port.
	MetricsAddr string

	// RedisAddr is the shared Redis endpoint for queue infrastructure.
	RedisAddr string
	// RedisPassword authenticates Redis queue clients.
	RedisPassword string
	// GoQueueConcurrency is the maximum number of Asynq tasks processed at once.
	GoQueueConcurrency int

	// GeminiAPIKey authenticates Gemini scoring requests (GEMINI_API_KEY).
	GeminiAPIKey string
	// GeminiOverflowAPIKey is an optional second, billed Gemini key
	// (GEMINI_API_KEY_OVERFLOW). GEMINI_API_KEY stays primary so its free-tier
	// daily quota is always spent first; when it answers 429 the client fails over
	// to this key, so the paid key only covers the spillover. When set, raise
	// GEMINI_HOST_DAILY_LIMIT to the total daily call budget across BOTH keys,
	// otherwise the app-side gate stops scoring before the overflow key is reached.
	GeminiOverflowAPIKey string
	// GeminiRateDelay is the minimum spacing between Gemini calls
	// (GEMINI_RATE_DELAY_MS, default 5s ≈ 12 RPM, safely below the 15 RPM free tier).
	GeminiRateDelay time.Duration
	// GeminiDailyLimit caps Gemini calls attempted per day (GEMINI_DAILY_LIMIT).
	GeminiDailyLimit int
	// GeminiHostDailyLimit caps total Gemini calls per day on the shared host key
	// across all tenants (GEMINI_HOST_DAILY_LIMIT). It bounds the host key's real
	// provider quota so one tenant on the host key can't exhaust it for everyone.
	// Defaults to GeminiDailyLimit.
	GeminiHostDailyLimit int
	// GeminiHostPerTenantDailyLimit caps host-key Gemini calls a SINGLE tenant may
	// make per day (GEMINI_HOST_PER_TENANT_DAILY_LIMIT). It is the public-signup cost
	// control: it stops one public user from draining the shared host budget. Every
	// tenant scores on the shared host key, so this is the per-user fairness cap.
	// Default 0 disables it, leaving self-host and single-tenant behavior unchanged.
	GeminiHostPerTenantDailyLimit int
	// GeminiHostInactiveDailyLimit caps host-key Gemini calls per day for a tenant
	// the activity gate marked inactive (GEMINI_HOST_INACTIVE_DAILY_LIMIT). It is
	// the daily companion to ScoringInactiveTopN, which only caps a single pass:
	// with many passes a day, top-N alone let idle tenants spend the whole shared
	// key before an active tenant was reached. Roughly one keep-warm pass a day
	// per idle tenant is the intent, so set it near the top-N, not far above it.
	// The usual quota reserve still applies, so the effective budget is this
	// number minus 10. 0 disables it (inactive tenants fall back to the ordinary
	// per-tenant cap), leaving self-host behavior unchanged.
	GeminiHostInactiveDailyLimit int
	// ScoringConcurrency is how many jobs are scored in flight at once
	// (SCORING_CONCURRENCY). The rate limiter still bounds throughput.
	ScoringConcurrency int
	// AutoArchiveThreshold archives scored jobs at or below this score
	// (AUTO_ARCHIVE_THRESHOLD).
	AutoArchiveThreshold int
	// ScoringBatchSize is how many jobs are scored per Gemini call
	// (SCORING_BATCH_SIZE). The default is 1 (single-job scoring, full model
	// attention): multi-job prompts make the model lose track and hallucinate
	// scores starting around 2-5 jobs per call, and no prompt fixes that. Batching
	// remains available (> 1) for free-tier-only installs that need call savings
	// badly enough to trade accuracy; pairing GEMINI_API_KEY_OVERFLOW with size 1
	// is the accurate, still-cheap configuration.
	ScoringBatchSize int
	// ScoringRescoreThreshold turns on two-stage scoring: a job whose batch score
	// lands at or above this value is re-scored individually (single-job prompt,
	// full model attention) to catch the high-score inflation a crowded batch
	// produces (SCORING_RESCORE_THRESHOLD). 0 disables the second pass. Only the
	// high scorers pay the extra call, so the cost stays bounded.
	ScoringRescoreThreshold int
	// PrefilterLocation archives jobs that fail the tenant's saved location
	// prefs (location.json) BEFORE any Gemini call is spent on them
	// (PREFILTER_LOCATION, default off). Buried rows carry a prefilter_*
	// archive_reason so they stay attributable and recoverable in the Archive
	// view. Tenants with no saved prefs are unaffected: default prefs pass
	// every job.
	PrefilterLocation bool
	// ExtractEnabled runs extract-once before each tenant's scoring pass
	// (EXTRACT_ENABLED, default off): one Gemini call per DISTINCT global job
	// materializes the rubric's STEP 1 facts into global_job_extracts, shared
	// across every tenant. This pass only builds the cache; scoring consumes it
	// behind its own flag, so extraction can soak safely on its own.
	ExtractEnabled bool
	// ExtractDailyLimit caps extract-once calls per UTC day across the whole
	// instance (GEMINI_EXTRACT_DAILY_LIMIT, default 500). The ledger is the
	// global extracts table itself, so the cap holds regardless of which
	// tenant's pass triggered the work.
	ExtractDailyLimit int
	// ScoringUseExtracts scores jobs against their cached global_job_extracts
	// facts instead of the full description when a current extract exists
	// (SCORING_USE_EXTRACTS, default off). Cuts the per-job prompt payload
	// roughly tenfold; jobs without an extract keep the description path.
	// Requires ExtractEnabled to be useful.
	ScoringUseExtracts bool
	// ScoringExtractBatchSize is the per-call batch size for extract-backed
	// jobs (SCORING_EXTRACT_BATCH_SIZE). Extract payloads are ~10x smaller, so
	// they can tolerate more jobs per call than descriptions, but batch
	// crowding still degrades score quality; raise this only after comparing
	// score distributions on staging. Default 0 falls back to
	// SCORING_BATCH_SIZE.
	ScoringExtractBatchSize int
	// ScoringEnabled gates the scheduled Go scorer. It defaults OFF so the Go
	// service can leave scheduled scoring off for local/manual modes.
	ScoringEnabled bool
	// ScoreSchedule is the cron expression for scheduled scoring when enabled.
	ScoreSchedule string
	// ScoreOnStart runs one scoring pass immediately on startup (after the
	// optional startup scrape), so a fresh deploy scores its backlog without
	// waiting for the first scoring cron tick. Requires ScoringEnabled.
	ScoreOnStart bool
	// ScoreAfterScrape chains a budgeted scoring pass for a tenant right after
	// that tenant's scrape completes (SCORE_AFTER_SCRAPE, default on), so fresh
	// listings get scores the same cycle instead of waiting for the next
	// ScoreSchedule tick. The scoring cron stays on as the sweeper for anything
	// a chained pass could not finish. Requires ScoringEnabled.
	ScoreAfterScrape bool
	// OnboardBurstLimit is how many of a brand-new tenant's best-matching pending
	// jobs are scored immediately at onboarding (ONBOARD_BURST_LIMIT, default 25,
	// 0 disables). The burst runs concurrently with the first scrape so scores
	// appear while it is still working, and it deliberately bypasses the
	// per-tenant fairness cap (never the host-global cap) so a signup is never
	// zero-scored by quota bookkeeping. Requires ScoringEnabled.
	OnboardBurstLimit int
	// OnboardBurstPreferOverflow forces the onboarding burst onto the billed
	// overflow key (ONBOARD_BURST_PREFER_OVERFLOW, default off). Off, the burst is
	// free-key-first and only shifts to the overflow key when the free key is
	// already stalled (429 cooldown or host daily budget spent).
	OnboardBurstPreferOverflow bool
	// GeminiOverflowRateDelay paces calls on the billed overflow key
	// (GEMINI_OVERFLOW_RATE_DELAY_MS, default 1s ≈ 60 RPM), independent of the
	// free key's GEMINI_RATE_DELAY_MS. The paid tier has RPM headroom the free
	// tier lacks, so the overflow key is paced faster by default and may be set
	// lower still to make overflow traffic, including onboarding bursts, faster.
	GeminiOverflowRateDelay time.Duration

	// MaintenanceSchedule is the cron expression for the DB-maintenance pass
	// (dedup + auto-ghost + follow-up maintenance). Default is every 6 hours,
	// aligned with the scrape cycle since most of the work is downstream of a
	// scrape. The pass fans out per tenant and runs closed-check, discovery and
	// slug-health, each a remote round trip (ATS HTTP / Supabase), so a tighter
	// cadence multiplied that per-tenant load for work that is not latency
	// sensitive. Nothing here needs Gemini except discovery, which self-throttles
	// on its own 6h TTL regardless of this cadence.
	MaintenanceSchedule string
	// RejectionSyncSchedule is the cron expression for the Gmail rejection-email
	// sweep plus the context update that persists its stage changes
	// (REJECTION_SYNC_SCHEDULE, default every 30 minutes). Deliberately separate
	// from MaintenanceSchedule: the sweep is IMAP + regex with no Gemini calls and
	// no job-table scans, and riding the 6-hour maintenance tick meant a rejection
	// email could sit unreflected in the pipeline stages for most of a day.
	RejectionSyncSchedule string
	// MarketResearchSchedule is the cron expression for warming the Market Research
	// report (MARKET_RESEARCH_SCHEDULE, default once a day). It is deliberately
	// separate from MaintenanceSchedule because each refresh reads every job's
	// description out of Postgres; running it on the 30-minute maintenance tick was
	// the dominant source of Supabase egress.
	MarketResearchSchedule string
	// MarketAllTimeWindowDays bounds the Market Research all-time job scan to the
	// last N days (MARKET_ALLTIME_WINDOW_DAYS, default 180) so the description read
	// does not grow unbounded as the table accumulates years of listings. 0
	// disables the bound (full all-time scope).
	MarketAllTimeWindowDays int
	// MetricsRefreshInterval is how often the per-tenant Grafana gauges are
	// refreshed (METRICS_REFRESH_INTERVAL_MS, default 15 min). Each tick fans out
	// per tenant over the pooler, so longer is cheaper; the gauges only need to be
	// roughly live.
	MetricsRefreshInterval time.Duration
	// MarketWarmInterval is how often the Market Research body cache is re-warmed
	// per tenant (MARKET_WARM_INTERVAL_MS, default 30 min). The page also rebuilds
	// lazily on view, so this only saves the occasional cold visit; longer cuts the
	// idle per-tenant pooler chatter.
	MarketWarmInterval time.Duration
	// GhostedAfterDays is how many days an applied job sits with no progress
	// before auto-ghost marks it ghosted (GHOSTED_AFTER_DAYS).
	GhostedAfterDays int
	// ScoringStaleUnscoredDays is how old (by posted_at, falling back to
	// first_seen_at) a pending unscored job may get before maintenance archives
	// it instead of ever spending a Gemini call on it
	// (SCORING_STALE_UNSCORED_DAYS, 0 disables).
	ScoringStaleUnscoredDays int
	// ScoringThinkingLevel is the Gemini thinkingConfig.thinkingLevel pinned on
	// scoring calls (SCORING_THINKING_LEVEL, default empty = omit the field).
	// flash-lite does NOT think unless asked: prod telemetry shows
	// total == prompt + candidates exactly with the field omitted, while
	// setting "low" ENABLED thinking (~380 thought tokens/job billed as
	// output) and, because thoughts count against maxOutputTokens, truncated
	// the visible reasoning. Set only to experiment on a model that defaults
	// to thinking.
	ScoringThinkingLevel string
	// ScoringActiveWindowDays gates full scoring on dashboard activity: tenants
	// not seen within this many days get only a keep-warm top-N per pass
	// (SCORING_ACTIVE_WINDOW_DAYS, 0 disables the gate). Never-seen tenants with
	// no scores yet count as active so onboarding is not starved.
	ScoringActiveWindowDays int
	// ScoringInactiveTopN is the keep-warm cap per scoring pass for inactive
	// tenants (SCORING_INACTIVE_TOP_N). With newest-first ordering the cap IS
	// the freshest N. 0 disables the gate.
	ScoringInactiveTopN int
	// ScoringBatchAPIEnabled turns on the Gemini Batch API backlog drain
	// (SCORING_BATCH_API_ENABLED, default false). Requires the paid overflow
	// key (the free tier has no batch quota); silently inert without it.
	ScoringBatchAPIEnabled bool
	// BatchAPIMaxOpenBatches bounds cross-tenant in-flight batches
	// (BATCH_API_MAX_OPEN_BATCHES). 4 x 500 jobs stays under the Tier-1
	// enqueued-token cap.
	BatchAPIMaxOpenBatches int
	// BatchAPIDailyJobLimit bounds jobs submitted per UTC day across all
	// tenants (BATCH_API_DAILY_JOB_LIMIT); the paid-key spend throttle.
	BatchAPIDailyJobLimit int
	// BatchAPIMaxJobsPerSubmit bounds one batch's size
	// (BATCH_API_MAX_JOBS_PER_SUBMIT); ~500 prompts is ~5.5MB inline, under
	// the API's 20MB request cap.
	BatchAPIMaxJobsPerSubmit int
	// BatchAPIPollMinutes is the batch loop's tick interval
	// (BATCH_API_POLL_MINUTES). Turnaround target is ~24h, so a slow tick
	// costs nothing; it just needs to beat the ~48h expiry comfortably.
	BatchAPIPollMinutes int
	// BatchAPIQuotaCooldownMinutes is how long the submit side of the batch
	// loop pauses after a quota-exhausted (429) submit
	// (BATCH_API_QUOTA_COOLDOWN_MINUTES). Poll/apply are unaffected. The
	// cooldown is in-memory; a restart clears it.
	BatchAPIQuotaCooldownMinutes int
	// CanonicalizeSchedule is the cron expression for alternate ATS
	// canonicalization (CANONICALIZE_SCHEDULE).
	CanonicalizeSchedule string
	// ATSConcurrency is the number of alternate ATS rows resolved concurrently
	// during canonicalization (ATS_CONCURRENCY).
	ATSConcurrency int
	// LogDir is the component-log root (LOG_DIR, default logs).
	LogDir string
	// LogLevel is the minimum structured-log level emitted to stdout
	// (LOG_LEVEL: debug|info|warn|error, default info).
	LogLevel string
	// LogRetentionDays is the dated component log retention window
	// (LOG_RETENTION_DAYS).
	LogRetentionDays int
	// JDHealthPath is the JSON output written by the description-quality check.
	JDHealthPath string
	// SlugHealthPath is the JSON output written by ATS slug validation.
	SlugHealthPath string
	// DiscoveryTTLHours is how long suggested-companies.json suppresses another
	// company-discovery run (DISCOVER_TTL_HOURS).
	DiscoveryTTLHours float64
	// DiscoveryCandidateCount is how many companies Gemini should propose per
	// discovery pass (DISCOVER_CANDIDATE_COUNT).
	DiscoveryCandidateCount int
	// DiscoveryBootstrapPasses is how many Gemini discovery passes a bootstrap
	// tenant (fewer than the bootstrap threshold of verified boards) runs
	// back-to-back (DISCOVER_BOOTSTRAP_PASSES, default 2). The shared registry
	// plus the LLM-free harvest now carry breadth, so Gemini only fills niche
	// gaps and needs fewer passes than it once did.
	DiscoveryBootstrapPasses int
	// HarvestProbeLimit / HarvestAdoptLimit cap the steady-state registry harvest
	// per tenant per pass: at most HarvestProbeLimit boards are live-probed and at
	// most HarvestAdoptLimit adopted into the tenant's scrape list
	// (HARVEST_PROBE_LIMIT default 60, HARVEST_ADOPT_LIMIT default 15).
	HarvestProbeLimit int
	// HarvestAdoptLimit is documented with HarvestProbeLimit above.
	HarvestAdoptLimit int
	// HarvestBootstrapProbe / HarvestBootstrapAdopt are the boosted harvest caps
	// used while a tenant is still in discovery bootstrap, so a brand-new signup
	// pulls a useful slice of the shared registry on its first pass instead of
	// trickling 15 boards per cycle (HARVEST_BOOTSTRAP_PROBE default 200,
	// HARVEST_BOOTSTRAP_ADOPT default 50).
	HarvestBootstrapProbe int
	// HarvestBootstrapAdopt is documented with HarvestBootstrapProbe above.
	HarvestBootstrapAdopt int
	// RegistryURLHarvest, when true (default), scans each tenant's job apply URLs
	// during maintenance and harvests the embedded ATS boards into the shared
	// company_registry so other tenants can scrape them directly. It costs no
	// Gemini calls; set REGISTRY_URL_HARVEST=0 to disable.
	RegistryURLHarvest bool
	// RegistrySeedSchedule is the cron expression for the Common Crawl registry
	// seed, which hunts new ATS board slugs from the latest monthly crawl and
	// upserts the live ones into the global registry (REGISTRY_SEED_SCHEDULE). An
	// empty value disables the scheduled seed.
	RegistrySeedSchedule string
	// RegistrySeedCrawls is how many of the most-recent monthly Common Crawl
	// indexes each scheduled seed sweeps (REGISTRY_SEED_CRAWLS, default 1). Only
	// the newest crawl is new each month, so 1 is right for the cron; the
	// seed-registry subcommand takes a larger count to backfill history.
	RegistrySeedCrawls int
	// GlobalJobSeedLimit bounds how many recent global job descriptions are copied
	// into a tenant's pending queue before a user-triggered scrape
	// (GLOBAL_JOB_SEED_LIMIT). This gives new tenants immediate jobs to rank while
	// the slower discovery/scrape bootstrap catches up. 0 disables seeding.
	GlobalJobSeedLimit int
	// GlobalJobSeedMaxAgeDays drops global jobs older than this many days from the
	// tenant seed (GLOBAL_JOB_SEED_MAX_AGE_DAYS, default 21, 0 disables). Seeded
	// jobs are the first thing a new tenant sees scored, so they should be
	// listings that are plausibly still open.
	GlobalJobSeedMaxAgeDays int
	// GlobalScrapeEnabled switches the scrape cron to global-first
	// (GLOBAL_SCRAPE_ENABLED, default off): the cross-tenant board union is
	// scraped once per cycle in board mode, every lead is harvested into
	// global_jobs, and each tenant's queue is fed by distribution (watchlist
	// company OR search-term match) instead of a per-tenant board scrape. The
	// per-tenant scrape keeps covering the aggregator platforms; the
	// user-triggered first-run path is unchanged.
	GlobalScrapeEnabled bool
	// GlobalScrapeBoardBatch is how many boards each board-mode /scrape request
	// carries (GLOBAL_SCRAPE_BOARD_BATCH, default 150), bounding worker response
	// size on big unions.
	GlobalScrapeBoardBatch int
	// GlobalDistributeLimit caps global rows scanned per tenant per distribution
	// pass (GLOBAL_DISTRIBUTE_LIMIT, default 500).
	GlobalDistributeLimit int
	// GlobalDistributeGraceHours is how far below its cursor each distribution
	// pass re-sweeps to recover rows orphaned by the within-pass write-order race
	// (GLOBAL_DISTRIBUTE_GRACE_HOURS, default 6). Must exceed one scrape pass's
	// duration; 0 disables the backfill.
	GlobalDistributeGraceHours int
	// GlobalCloseGraceHours is how long a posting must stay missing from
	// complete board fetches before the closed-by-diff pass closes it
	// (GLOBAL_CLOSE_GRACE_HOURS, default 24). 0 disables the grace window
	// (close on a single missed cycle).
	GlobalCloseGraceHours int
	// GlobalCloseVerify probes each Workday diff-close candidate against its
	// board's search endpoint before closing it (GLOBAL_CLOSE_VERIFY, default
	// true), catching truncated listings that read as complete.
	GlobalCloseVerify bool
	// GlobalWorkdayRegistryBatch caps how many registry Workday boards fold into
	// the scrape union per cycle (GLOBAL_WORKDAY_REGISTRY_BATCH, default 150).
	// Workday fetches are full (no etag) and 429-prone, so a persisted cursor
	// rotates through the ~5k registry boards a slice at a time instead of
	// pulling them all every cycle. The default is kept modest because each
	// Workday board burns retry time on 429s and never caches, so a large slice
	// eats the board-scrape budget the etag-able platforms need. 0 disables it.
	GlobalWorkdayRegistryBatch int
	// GmailEmail/GmailAppPassword authenticate the Gmail rejection-email sync.
	GmailEmail string
	// GmailAppPassword is the Gmail app password for rejection-email sync.
	GmailAppPassword string
	// RejectionEmailSyncDisabled gates the Gmail rejection-email sync.
	RejectionEmailSyncDisabled bool
	// RejectionEmailLookbackDays is the IMAP search window for rejection sync.
	RejectionEmailLookbackDays int
	// RejectionEmailMaxMessages bounds messages fetched per mailbox sweep.
	RejectionEmailMaxMessages int
	// RejectionEmailSkipTrash disables the overlapping Gmail Trash sweep.
	RejectionEmailSkipTrash bool

	// DashboardAddr is the listen address for the Go dashboard. It defaults to
	// :3131 (the port the Node dashboard used) now that Go serves every route.
	DashboardAddr string
	// PublicDir is the directory of static assets (public/) served natively.
	PublicDir string
	// ScrapeTriggerURL is the Go scrape trigger server the dashboard's "Scrape
	// now" button posts to (SCRAPE_TRIGGER_URL, e.g. http://go-backend:8090).
	ScrapeTriggerURL string
	// RateLimitPerMinute throttles expensive authenticated endpoints (market
	// research, ask, scrape-now, profile refresh/extract) per tenant
	// (RATE_LIMIT_PER_MINUTE). It bounds request rate, complementing the daily Gemini
	// quota which bounds call volume. Default 20; 0 disables it. Self-host (single
	// tenant) is never throttled.
	RateLimitPerMinute int
	// RateLimitBurst is the per-tenant burst allowance on top of RateLimitPerMinute
	// (RATE_LIMIT_BURST, default 5).
	RateLimitBurst int
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getenvTimezone reads an IANA timezone name and returns it only if it is a real
// zone (time.LoadLocation succeeds). An empty or unrecognized value yields the
// fallback, so a typo can never reach the SQL string the dialect builds from it.
func getenvTimezone(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	if _, err := time.LoadLocation(v); err != nil {
		return fallback
	}
	return v
}

func getenvBool(key string, fallback bool) bool {
	switch os.Getenv(key) {
	case "1", "true", "TRUE", "yes":
		return true
	case "0", "false", "FALSE", "no":
		return false
	default:
		return fallback
	}
}

func getenvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// getenvIntAllowZero is getenvInt but treats an explicit 0 as a valid value, for
// knobs where 0 means "disabled" rather than "unset".
func getenvIntAllowZero(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return fallback
}

func getenvNonNegativeFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 {
			return n
		}
	}
	return fallback
}

// getenvDurationMs reads an integer count of milliseconds (matching the Node
// *_MS env conventions) and returns it as a Duration.
func getenvDurationMs(key string, fallbackMs int) time.Duration {
	return time.Duration(getenvInt(key, fallbackMs)) * time.Millisecond
}

// getenvDurationMinutes reads an integer count of minutes and returns it as a
// Duration. For coarse operational budgets where milliseconds would be noise.
func getenvDurationMinutes(key string, fallbackMin int) time.Duration {
	return time.Duration(getenvInt(key, fallbackMin)) * time.Minute
}

// Load reads configuration from the environment, applying defaults that match
// the Node app's local (non-Docker) conventions.
func Load() Config {
	dataDir := getenv("DATA_DIR", "data")
	dbDir := getenv("DB_DIR", dataDir)

	return Config{
		ScraperURL:              getenv("SCRAPER_URL", "http://localhost:4040"),
		ScrapeTimeout:           getenvDurationMinutes("SCRAPE_TIMEOUT_MINUTES", 10),
		GlobalScrapeTimeout:     getenvDurationMinutes("GLOBAL_SCRAPE_TIMEOUT_MINUTES", 60),
		GlobalDistributeReserve: getenvDurationMinutes("GLOBAL_DISTRIBUTE_RESERVE_MINUTES", 15),
		DataDir:                 dataDir,
		ContextDir:              getenv("CONTEXT_DIR", ".context"),
		DBDir:                   dbDir,
		DBPath:                  filepath.Join(dbDir, "jobs.db"),
		DBType:                  getenv("DATABASE_TYPE", "sqlite"),
		DatabaseURL:             os.Getenv("DATABASE_URL"),
		Timezone:                getenvTimezone("APP_TIMEZONE", ""),
		DBMaxOpenConns:          getenvInt("DB_MAX_OPEN_CONNS", 10),
		DBMaxIdleConns:          getenvInt("DB_MAX_IDLE_CONNS", 5),
		// 30 min (was 5 min): pooled connections get reused instead of recycled
		// every 5 minutes, which slashed the constant pgbouncer get_auth +
		// goose_db_version + set_config/begin/commit setup round trips that the
		// per-tenant fan-out loops drove over the remote pooler. Client-side only;
		// safe with Supabase transaction-mode pooling.
		DBConnMaxLifetime: getenvDurationMs("DB_CONN_MAX_LIFETIME_MS", 1800000),
		DBConnMaxIdleTime: getenvDurationMs("DB_CONN_MAX_IDLE_TIME_MS", 1800000),
		SupabaseURL:       os.Getenv("SUPABASE_URL"),
		SupabaseAnonKey:   os.Getenv("SUPABASE_ANON_KEY"),
		RLSEnforce:        getenvBool("RLS_ENFORCE", false),
		RLSDatabaseURL:    os.Getenv("RLS_DATABASE_URL"),
		ScrapeSchedule:    getenv("SCRAPE_SCHEDULE", "0 */3 * * *"),
		ScrapeOnStart:     getenvBool("SCRAPE_ON_START", false),
		TriggerAddr:       getenv("TRIGGER_ADDR", ":8090"),
		MetricsAddr:       getenv("METRICS_ADDR", ":9090"),

		RedisAddr:          getenv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:      os.Getenv("REDIS_PASSWORD"),
		GoQueueConcurrency: getenvInt("GO_QUEUE_CONCURRENCY", 2),

		GeminiAPIKey:                  os.Getenv("GEMINI_API_KEY"),
		GeminiOverflowAPIKey:          os.Getenv("GEMINI_API_KEY_OVERFLOW"),
		GeminiRateDelay:               getenvDurationMs("GEMINI_RATE_DELAY_MS", 5000),
		GeminiDailyLimit:              getenvInt("GEMINI_DAILY_LIMIT", 500),
		GeminiHostDailyLimit:          getenvInt("GEMINI_HOST_DAILY_LIMIT", getenvInt("GEMINI_DAILY_LIMIT", 500)),
		GeminiHostPerTenantDailyLimit: getenvInt("GEMINI_HOST_PER_TENANT_DAILY_LIMIT", 0),
		GeminiHostInactiveDailyLimit:  getenvInt("GEMINI_HOST_INACTIVE_DAILY_LIMIT", 0),
		ScoringConcurrency:            getenvInt("SCORING_CONCURRENCY", 3),
		AutoArchiveThreshold:          getenvInt("AUTO_ARCHIVE_THRESHOLD", 4),
		ScoringBatchSize:              getenvInt("SCORING_BATCH_SIZE", 1),
		ScoringRescoreThreshold:       getenvInt("SCORING_RESCORE_THRESHOLD", 7),
		PrefilterLocation:             getenvBool("PREFILTER_LOCATION", false),
		ExtractEnabled:                getenvBool("EXTRACT_ENABLED", false),
		ExtractDailyLimit:             getenvInt("GEMINI_EXTRACT_DAILY_LIMIT", 500),
		ScoringUseExtracts:            getenvBool("SCORING_USE_EXTRACTS", false),
		ScoringExtractBatchSize:       getenvIntAllowZero("SCORING_EXTRACT_BATCH_SIZE", 0),
		ScoringEnabled:                getenvBool("SCORING_ENABLED", false),
		ScoreSchedule:                 getenv("SCORE_SCHEDULE", "15 */6 * * *"),
		ScoreOnStart:                  getenvBool("SCORE_ON_START", false),
		ScoreAfterScrape:              getenvBool("SCORE_AFTER_SCRAPE", true),
		OnboardBurstLimit:             getenvIntAllowZero("ONBOARD_BURST_LIMIT", 25),
		OnboardBurstPreferOverflow:    getenvBool("ONBOARD_BURST_PREFER_OVERFLOW", false),
		GeminiOverflowRateDelay:       getenvDurationMs("GEMINI_OVERFLOW_RATE_DELAY_MS", 1000),
		MaintenanceSchedule:           getenv("MAINTENANCE_SCHEDULE", "0 */6 * * *"),
		RejectionSyncSchedule:         getenv("REJECTION_SYNC_SCHEDULE", "*/30 * * * *"),
		MarketResearchSchedule:        getenv("MARKET_RESEARCH_SCHEDULE", "0 7 * * *"),
		MarketAllTimeWindowDays:       getenvInt("MARKET_ALLTIME_WINDOW_DAYS", 180),
		MetricsRefreshInterval:        getenvDurationMs("METRICS_REFRESH_INTERVAL_MS", 900000),
		MarketWarmInterval:            getenvDurationMs("MARKET_WARM_INTERVAL_MS", 1800000),
		GhostedAfterDays:              getenvInt("GHOSTED_AFTER_DAYS", 14),
		ScoringStaleUnscoredDays:      getenvInt("SCORING_STALE_UNSCORED_DAYS", 30),
		ScoringThinkingLevel:          getenv("SCORING_THINKING_LEVEL", ""),
		ScoringActiveWindowDays:       getenvInt("SCORING_ACTIVE_WINDOW_DAYS", 7),
		ScoringInactiveTopN:           getenvInt("SCORING_INACTIVE_TOP_N", 100),
		ScoringBatchAPIEnabled:        getenvBool("SCORING_BATCH_API_ENABLED", false),
		BatchAPIMaxOpenBatches:        getenvInt("BATCH_API_MAX_OPEN_BATCHES", 4),
		BatchAPIDailyJobLimit:         getenvInt("BATCH_API_DAILY_JOB_LIMIT", 6000),
		BatchAPIMaxJobsPerSubmit:      getenvInt("BATCH_API_MAX_JOBS_PER_SUBMIT", 500),
		BatchAPIPollMinutes:           getenvInt("BATCH_API_POLL_MINUTES", 20),
		BatchAPIQuotaCooldownMinutes:  getenvInt("BATCH_API_QUOTA_COOLDOWN_MINUTES", 120),
		CanonicalizeSchedule:          getenv("CANONICALIZE_SCHEDULE", "0 */6 * * *"),
		ATSConcurrency:                getenvInt("ATS_CONCURRENCY", 5),
		LogDir:                        getenv("LOG_DIR", "logs"),
		LogLevel:                      getenv("LOG_LEVEL", "info"),
		LogRetentionDays:              getenvInt("LOG_RETENTION_DAYS", 30),
		JDHealthPath:                  getenv("JD_HEALTH_PATH", "jd-health.json"),
		SlugHealthPath:                getenv("SLUG_HEALTH_PATH", "slug-health.json"),
		DiscoveryTTLHours:             getenvNonNegativeFloat("DISCOVER_TTL_HOURS", 4),
		DiscoveryCandidateCount:       getenvInt("DISCOVER_CANDIDATE_COUNT", 150),
		DiscoveryBootstrapPasses:      getenvInt("DISCOVER_BOOTSTRAP_PASSES", 2),
		HarvestProbeLimit:             getenvInt("HARVEST_PROBE_LIMIT", 60),
		HarvestAdoptLimit:             getenvInt("HARVEST_ADOPT_LIMIT", 15),
		HarvestBootstrapProbe:         getenvInt("HARVEST_BOOTSTRAP_PROBE", 200),
		HarvestBootstrapAdopt:         getenvInt("HARVEST_BOOTSTRAP_ADOPT", 50),
		RegistryURLHarvest:            getenvBool("REGISTRY_URL_HARVEST", true),
		RegistrySeedSchedule:          getenv("REGISTRY_SEED_SCHEDULE", "0 4 5 * *"),
		RegistrySeedCrawls:            getenvInt("REGISTRY_SEED_CRAWLS", 1),
		GlobalJobSeedLimit:            getenvInt("GLOBAL_JOB_SEED_LIMIT", 100),
		GlobalJobSeedMaxAgeDays:       getenvIntAllowZero("GLOBAL_JOB_SEED_MAX_AGE_DAYS", 21),
		GlobalScrapeEnabled:           getenvBool("GLOBAL_SCRAPE_ENABLED", false),
		GlobalScrapeBoardBatch:        getenvInt("GLOBAL_SCRAPE_BOARD_BATCH", 150),
		GlobalDistributeLimit:         getenvInt("GLOBAL_DISTRIBUTE_LIMIT", 500),
		GlobalDistributeGraceHours:    getenvInt("GLOBAL_DISTRIBUTE_GRACE_HOURS", 6),
		GlobalCloseGraceHours:         getenvIntAllowZero("GLOBAL_CLOSE_GRACE_HOURS", 24),
		GlobalCloseVerify:             getenvBool("GLOBAL_CLOSE_VERIFY", true),
		GlobalWorkdayRegistryBatch:    getenvIntAllowZero("GLOBAL_WORKDAY_REGISTRY_BATCH", 150),
		GmailEmail:                    os.Getenv("GMAIL_EMAIL"),
		GmailAppPassword:              os.Getenv("GMAIL_APP_PASSWORD"),
		RejectionEmailSyncDisabled: getenvBool(
			"REJECTION_EMAIL_SYNC_DISABLED", false,
		),
		RejectionEmailLookbackDays: getenvInt("REJECTION_EMAIL_LOOKBACK_DAYS", 30),
		RejectionEmailMaxMessages:  getenvInt("REJECTION_EMAIL_MAX_MESSAGES", 300),
		RejectionEmailSkipTrash:    getenvBool("REJECTION_EMAIL_SKIP_TRASH", false),

		DashboardAddr:      getenv("DASHBOARD_ADDR", ":3131"),
		PublicDir:          getenv("PUBLIC_DIR", "public"),
		ScrapeTriggerURL:   os.Getenv("SCRAPE_TRIGGER_URL"),
		RateLimitPerMinute: getenvInt("RATE_LIMIT_PER_MINUTE", 20),
		RateLimitBurst:     getenvInt("RATE_LIMIT_BURST", 5),
	}
}
