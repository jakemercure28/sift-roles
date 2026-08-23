package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"job-search-automation/internal/config"
	"job-search-automation/internal/db"
)

func TestBuildDashboardAuthFailsClosedWhenJWKSSetupFails(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	v := buildDashboardAuth(context.Background(), config.Config{
		DBType:      string(db.Postgres),
		SupabaseURL: "://not-a-url",
	}, logger)

	if v == nil {
		t.Fatal("buildDashboardAuth returned nil; hosted auth would become unauthenticated")
	}
	if uid, err := v.Verify(context.Background(), "token"); err == nil || uid != "" {
		t.Fatalf("rejecting verifier Verify = (%q, %v), want empty uid and error", uid, err)
	}
}

func TestTenantActive(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	fresh := now.AddDate(0, 0, -2).Format("2006-01-02 15:04:05")
	stale := now.AddDate(0, 0, -10).Format("2006-01-02 15:04:05")

	cases := []struct {
		name      string
		lastSeen  string
		hasScores bool
		want      bool
	}{
		{"seen inside window", fresh, true, true},
		{"seen outside window", stale, true, false},
		{"never seen, onboarding (no scores)", "", false, true},
		{"never seen, has scores", "", true, false},
	}
	for _, c := range cases {
		if got := tenantActive(c.lastSeen, c.hasScores, 7, now); got != c.want {
			t.Errorf("%s: tenantActive = %v, want %v", c.name, got, c.want)
		}
	}
}
