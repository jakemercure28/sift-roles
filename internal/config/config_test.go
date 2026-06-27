package config

import "testing"

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

	if cfg.ScoringBatchSize != 10 {
		t.Fatalf("ScoringBatchSize = %d, want 10", cfg.ScoringBatchSize)
	}
	if cfg.ScoringRescoreThreshold != 7 {
		t.Fatalf("ScoringRescoreThreshold = %d, want 7", cfg.ScoringRescoreThreshold)
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
