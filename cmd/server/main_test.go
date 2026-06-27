package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

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
