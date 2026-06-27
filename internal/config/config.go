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
	// ScrapeTimeout bounds a single /scrape request to the worker.
	ScrapeTimeout time.Duration
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
	// ScoringConcurrency is how many jobs are scored in flight at once
	// (SCORING_CONCURRENCY). The rate limiter still bounds throughput.
	ScoringConcurrency int
	// AutoArchiveThreshold archives scored jobs at or below this score
	// (AUTO_ARCHIVE_THRESHOLD).
	AutoArchiveThreshold int
	// ScoringBatchSize is how many jobs are scored per Gemini call
	// (SCORING_BATCH_SIZE). Batching cuts daily call volume ~N-fold so a small free
	// host-key quota can score many jobs, but a larger batch dilutes the model's
	// per-job attention and inflates scores. The default leans toward call/token
	// savings because high batch scores are re-scored individually by default.
	ScoringBatchSize int
	// ScoringRescoreThreshold turns on two-stage scoring: a job whose batch score
	// lands at or above this value is re-scored individually (single-job prompt,
	// full model attention) to catch the high-score inflation a crowded batch
	// produces (SCORING_RESCORE_THRESHOLD). 0 disables the second pass. Only the
	// high scorers pay the extra call, so the cost stays bounded.
	ScoringRescoreThreshold int
	// ScoringEnabled gates the scheduled Go scorer. It defaults OFF so the Go
	// service can leave scheduled scoring off for local/manual modes.
	ScoringEnabled bool
	// ScoreSchedule is the cron expression for scheduled scoring when enabled.
	ScoreSchedule string
	// ScoreOnStart runs one scoring pass immediately on startup (after the
	// optional startup scrape), so a fresh deploy scores its backlog without
	// waiting for the first scoring cron tick. Requires ScoringEnabled.
	ScoreOnStart bool

	// MaintenanceSchedule is the cron expression for the DB-maintenance pass
	// (dedup + auto-ghost + follow-up maintenance). Default is every 6 hours,
	// aligned with the scrape cycle since most of the work is downstream of a
	// scrape. The pass fans out per tenant and runs closed-check, rejection-sync,
	// context-update, discovery and slug-health, each a remote round trip (IMAP /
	// ATS HTTP / Supabase), so a tighter cadence multiplied that per-tenant load
	// for work that is not latency sensitive. Nothing here needs Gemini except
	// discovery, which self-throttles on its own 6h TTL regardless of this cadence.
	MaintenanceSchedule string
	// MarketResearchSchedule is the cron expression for warming the Market Research
	// report (MARKET_RESEARCH_SCHEDULE, default once a day). It is deliberately
	// separate from MaintenanceSchedule because each refresh reads every job's
	// description out of Postgres; running it on the 30-minute maintenance tick was
	// the dominant source of Supabase egress.
	MarketResearchSchedule string
	// GhostedAfterDays is how many days an applied job sits with no progress
	// before auto-ghost marks it ghosted (GHOSTED_AFTER_DAYS).
	GhostedAfterDays int
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
	// GlobalJobSeedLimit bounds how many recent global job descriptions are copied
	// into a tenant's pending queue before a user-triggered scrape
	// (GLOBAL_JOB_SEED_LIMIT). This gives new tenants immediate jobs to rank while
	// the slower discovery/scrape bootstrap catches up. 0 disables seeding.
	GlobalJobSeedLimit int
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

// Load reads configuration from the environment, applying defaults that match
// the Node app's local (non-Docker) conventions.
func Load() Config {
	dataDir := getenv("DATA_DIR", "data")
	dbDir := getenv("DB_DIR", dataDir)

	return Config{
		ScraperURL:        getenv("SCRAPER_URL", "http://localhost:4040"),
		ScrapeTimeout:     10 * time.Minute,
		DataDir:           dataDir,
		ContextDir:        getenv("CONTEXT_DIR", ".context"),
		DBDir:             dbDir,
		DBPath:            filepath.Join(dbDir, "jobs.db"),
		DBType:            getenv("DATABASE_TYPE", "sqlite"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		Timezone:          getenvTimezone("APP_TIMEZONE", ""),
		DBMaxOpenConns:    getenvInt("DB_MAX_OPEN_CONNS", 10),
		DBMaxIdleConns:    getenvInt("DB_MAX_IDLE_CONNS", 5),
		DBConnMaxLifetime: getenvDurationMs("DB_CONN_MAX_LIFETIME_MS", 300000),
		DBConnMaxIdleTime: getenvDurationMs("DB_CONN_MAX_IDLE_TIME_MS", 300000),
		SupabaseURL:       os.Getenv("SUPABASE_URL"),
		SupabaseAnonKey:   os.Getenv("SUPABASE_ANON_KEY"),
		RLSEnforce:        getenvBool("RLS_ENFORCE", false),
		RLSDatabaseURL:    os.Getenv("RLS_DATABASE_URL"),
		ScrapeSchedule:    getenv("SCRAPE_SCHEDULE", "0 */6 * * *"),
		ScrapeOnStart:     getenvBool("SCRAPE_ON_START", false),
		TriggerAddr:       getenv("TRIGGER_ADDR", ":8090"),
		MetricsAddr:       getenv("METRICS_ADDR", ":9090"),

		RedisAddr:          getenv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:      os.Getenv("REDIS_PASSWORD"),
		GoQueueConcurrency: getenvInt("GO_QUEUE_CONCURRENCY", 2),

		GeminiAPIKey:                  os.Getenv("GEMINI_API_KEY"),
		GeminiRateDelay:               getenvDurationMs("GEMINI_RATE_DELAY_MS", 5000),
		GeminiDailyLimit:              getenvInt("GEMINI_DAILY_LIMIT", 500),
		GeminiHostDailyLimit:          getenvInt("GEMINI_HOST_DAILY_LIMIT", getenvInt("GEMINI_DAILY_LIMIT", 500)),
		GeminiHostPerTenantDailyLimit: getenvInt("GEMINI_HOST_PER_TENANT_DAILY_LIMIT", 0),
		ScoringConcurrency:            getenvInt("SCORING_CONCURRENCY", 3),
		AutoArchiveThreshold:          getenvInt("AUTO_ARCHIVE_THRESHOLD", 4),
		ScoringBatchSize:              getenvInt("SCORING_BATCH_SIZE", 10),
		ScoringRescoreThreshold:       getenvInt("SCORING_RESCORE_THRESHOLD", 7),
		ScoringEnabled:                getenvBool("SCORING_ENABLED", false),
		ScoreSchedule:                 getenv("SCORE_SCHEDULE", "15 */6 * * *"),
		ScoreOnStart:                  getenvBool("SCORE_ON_START", false),
		MaintenanceSchedule:           getenv("MAINTENANCE_SCHEDULE", "0 */6 * * *"),
		MarketResearchSchedule:        getenv("MARKET_RESEARCH_SCHEDULE", "0 7 * * *"),
		GhostedAfterDays:              getenvInt("GHOSTED_AFTER_DAYS", 14),
		CanonicalizeSchedule:          getenv("CANONICALIZE_SCHEDULE", "0 */6 * * *"),
		ATSConcurrency:                getenvInt("ATS_CONCURRENCY", 5),
		LogDir:                        getenv("LOG_DIR", "logs"),
		LogLevel:                      getenv("LOG_LEVEL", "info"),
		LogRetentionDays:              getenvInt("LOG_RETENTION_DAYS", 30),
		JDHealthPath:                  getenv("JD_HEALTH_PATH", "jd-health.json"),
		SlugHealthPath:                getenv("SLUG_HEALTH_PATH", "slug-health.json"),
		DiscoveryTTLHours:             getenvNonNegativeFloat("DISCOVER_TTL_HOURS", 6),
		DiscoveryCandidateCount:       getenvInt("DISCOVER_CANDIDATE_COUNT", 80),
		GlobalJobSeedLimit:            getenvInt("GLOBAL_JOB_SEED_LIMIT", 100),
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
