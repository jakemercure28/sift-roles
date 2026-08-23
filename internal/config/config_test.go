package config

import (
	"testing"
	"time"
)

func TestLoadRedisQueueDefaults(t *testing.T) {
	t.Setenv("REDIS_ADDR", "")
	t.Setenv("REDIS_PASSWORD", "")
	t.Setenv("GO_QUEUE_CONCURRENCY", "")

	cfg := Load()

	if cfg.RedisAddr != "localhost:6379" {
		t.Fatalf("RedisAddr = %q, want localhost:6379", cfg.RedisAddr)
	}
	if cfg.RedisPassword != "" {
		t.Fatalf("RedisPassword = %q, want empty", cfg.RedisPassword)
	}
	if cfg.GoQueueConcurrency != 2 {
		t.Fatalf("GoQueueConcurrency = %d, want 2", cfg.GoQueueConcurrency)
	}
}

func TestLoadRedisQueueOverrides(t *testing.T) {
	t.Setenv("REDIS_ADDR", "redis:6379")
	t.Setenv("REDIS_PASSWORD", "secret")
	t.Setenv("GO_QUEUE_CONCURRENCY", "7")

	cfg := Load()

	if cfg.RedisAddr != "redis:6379" {
		t.Fatalf("RedisAddr = %q, want redis:6379", cfg.RedisAddr)
	}
	if cfg.RedisPassword != "secret" {
		t.Fatalf("RedisPassword = %q, want secret", cfg.RedisPassword)
	}
	if cfg.GoQueueConcurrency != 7 {
		t.Fatalf("GoQueueConcurrency = %d, want 7", cfg.GoQueueConcurrency)
	}
}

func TestLoadDiscoveryConfig(t *testing.T) {
	t.Setenv("DISCOVER_TTL_HOURS", "2.5")
	t.Setenv("DISCOVER_CANDIDATE_COUNT", "12")

	cfg := Load()

	if cfg.DiscoveryTTLHours != 2.5 {
		t.Fatalf("DiscoveryTTLHours = %v, want 2.5", cfg.DiscoveryTTLHours)
	}
	if cfg.DiscoveryCandidateCount != 12 {
		t.Fatalf("DiscoveryCandidateCount = %d, want 12", cfg.DiscoveryCandidateCount)
	}
}

func TestLoadScoringDefaults(t *testing.T) {
	t.Setenv("SCORING_BATCH_SIZE", "")
	t.Setenv("SCORING_RESCORE_THRESHOLD", "")

	cfg := Load()

	if cfg.ScoringBatchSize != 1 {
		t.Fatalf("ScoringBatchSize = %d, want 1", cfg.ScoringBatchSize)
	}
	if cfg.ScoringRescoreThreshold != 7 {
		t.Fatalf("ScoringRescoreThreshold = %d, want 7", cfg.ScoringRescoreThreshold)
	}
}

func TestLoadGeminiOverflowKey(t *testing.T) {
	t.Setenv("GEMINI_API_KEY_OVERFLOW", "paid-key")

	cfg := Load()

	if cfg.GeminiOverflowAPIKey != "paid-key" {
		t.Fatalf("GeminiOverflowAPIKey = %q, want %q", cfg.GeminiOverflowAPIKey, "paid-key")
	}
}

func TestLoadPostgresPoolConfig(t *testing.T) {
	t.Setenv("DB_MAX_OPEN_CONNS", "8")
	t.Setenv("DB_MAX_IDLE_CONNS", "4")
	t.Setenv("DB_CONN_MAX_LIFETIME_MS", "120000")
	t.Setenv("DB_CONN_MAX_IDLE_TIME_MS", "45000")

	cfg := Load()

	if cfg.DBMaxOpenConns != 8 {
		t.Fatalf("DBMaxOpenConns = %d, want 8", cfg.DBMaxOpenConns)
	}
	if cfg.DBMaxIdleConns != 4 {
		t.Fatalf("DBMaxIdleConns = %d, want 4", cfg.DBMaxIdleConns)
	}
	if cfg.DBConnMaxLifetime.Milliseconds() != 120000 {
		t.Fatalf("DBConnMaxLifetime = %s, want 120000ms", cfg.DBConnMaxLifetime)
	}
	if cfg.DBConnMaxIdleTime.Milliseconds() != 45000 {
		t.Fatalf("DBConnMaxIdleTime = %s, want 45000ms", cfg.DBConnMaxIdleTime)
	}
}

// The billed overflow key is paced independently of the free key: raising the
// free-key delay must not slow the paid key, and setting the overflow var must
// win over the 1s default. This keeps the free-tier guardrail from bleeding into
// the paid path (and vice versa).
func TestLoadOverflowRateDelayIndependentOfFree(t *testing.T) {
	t.Setenv("GEMINI_RATE_DELAY_MS", "8000")
	t.Setenv("GEMINI_OVERFLOW_RATE_DELAY_MS", "")

	cfg := Load()
	if cfg.GeminiRateDelay != 8*time.Second {
		t.Fatalf("GeminiRateDelay = %v, want 8s", cfg.GeminiRateDelay)
	}
	if cfg.GeminiOverflowRateDelay != time.Second {
		t.Fatalf("GeminiOverflowRateDelay = %v, want 1s (must not inherit the free-key delay)", cfg.GeminiOverflowRateDelay)
	}

	t.Setenv("GEMINI_OVERFLOW_RATE_DELAY_MS", "250")
	if cfg := Load(); cfg.GeminiOverflowRateDelay != 250*time.Millisecond {
		t.Fatalf("GeminiOverflowRateDelay = %v, want 250ms when set explicitly", cfg.GeminiOverflowRateDelay)
	}
}

func TestLoadOnboardingFastLaneDefaults(t *testing.T) {
	for _, k := range []string{
		"SCORE_AFTER_SCRAPE", "ONBOARD_BURST_LIMIT", "ONBOARD_BURST_PREFER_OVERFLOW",
		"GLOBAL_JOB_SEED_MAX_AGE_DAYS", "GEMINI_OVERFLOW_RATE_DELAY_MS", "GEMINI_RATE_DELAY_MS",
		"DISCOVER_BOOTSTRAP_PASSES", "HARVEST_PROBE_LIMIT", "HARVEST_ADOPT_LIMIT",
		"HARVEST_BOOTSTRAP_PROBE", "HARVEST_BOOTSTRAP_ADOPT",
	} {
		t.Setenv(k, "")
	}

	cfg := Load()

	if !cfg.ScoreAfterScrape {
		t.Fatal("ScoreAfterScrape default should be true")
	}
	if cfg.OnboardBurstLimit != 25 {
		t.Fatalf("OnboardBurstLimit = %d, want 25", cfg.OnboardBurstLimit)
	}
	if cfg.OnboardBurstPreferOverflow {
		t.Fatal("OnboardBurstPreferOverflow default should be false")
	}
	if cfg.GlobalJobSeedMaxAgeDays != 21 {
		t.Fatalf("GlobalJobSeedMaxAgeDays = %d, want 21", cfg.GlobalJobSeedMaxAgeDays)
	}
	if cfg.GeminiOverflowRateDelay != time.Second {
		t.Fatalf("GeminiOverflowRateDelay = %v, want 1s (paid key paced faster than free by default)", cfg.GeminiOverflowRateDelay)
	}
	if cfg.GeminiOverflowRateDelay >= cfg.GeminiRateDelay {
		t.Fatalf("GeminiOverflowRateDelay %v should be faster than the free-key GeminiRateDelay %v", cfg.GeminiOverflowRateDelay, cfg.GeminiRateDelay)
	}
	if cfg.DiscoveryBootstrapPasses != 2 {
		t.Fatalf("DiscoveryBootstrapPasses = %d, want 2", cfg.DiscoveryBootstrapPasses)
	}
	if cfg.HarvestProbeLimit != 60 || cfg.HarvestAdoptLimit != 15 {
		t.Fatalf("steady harvest limits = %d/%d, want 60/15", cfg.HarvestProbeLimit, cfg.HarvestAdoptLimit)
	}
	if cfg.HarvestBootstrapProbe != 200 || cfg.HarvestBootstrapAdopt != 50 {
		t.Fatalf("bootstrap harvest limits = %d/%d, want 200/50", cfg.HarvestBootstrapProbe, cfg.HarvestBootstrapAdopt)
	}
}

func TestLoadOnboardingFastLaneZeroDisables(t *testing.T) {
	// These knobs treat an explicit 0 as "disabled", not "unset": the plain
	// getenvInt would silently fall back to the default.
	t.Setenv("ONBOARD_BURST_LIMIT", "0")
	t.Setenv("GLOBAL_JOB_SEED_MAX_AGE_DAYS", "0")

	cfg := Load()

	if cfg.OnboardBurstLimit != 0 {
		t.Fatalf("OnboardBurstLimit = %d, want 0 (explicit disable)", cfg.OnboardBurstLimit)
	}
	if cfg.GlobalJobSeedMaxAgeDays != 0 {
		t.Fatalf("GlobalJobSeedMaxAgeDays = %d, want 0 (explicit disable)", cfg.GlobalJobSeedMaxAgeDays)
	}
}
