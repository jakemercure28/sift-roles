package db

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"job-search-automation/internal/model"
)

// TestPostgresSmoke drives the Postgres backend end to end: it applies the
// Postgres migration track and exercises a representative repo method for every
// dialect-rewrite branch (datetime/date/julianday, the two interval forms,
// INSERT OR IGNORE -> ON CONFLICT, upserts, window functions, and the
// transaction + canonicalization paths). Its job is to prove the rewriter emits
// valid Postgres, so it fails on any execution error.
//
// Gated on JSA_PG_DSN (e.g. postgres://postgres:spike@localhost:55432/jsa?sslmode=disable).
// Skipped when unset so the default `go test ./...` stays SQLite-only.
func TestPostgresSmoke(t *testing.T) {
	dsn := os.Getenv("JSA_PG_DSN")
	if dsn == "" {
		t.Skip("set JSA_PG_DSN to a throwaway Postgres DSN to run the Postgres smoke test")
	}

	repo, err := OpenPostgres(dsn, DefaultPoolConfig(), "", false)
	if err != nil {
		t.Fatalf("OpenPostgres (migrations): %v", err)
	}
	t.Cleanup(func() { repo.Close() })

	if repo.dl.kind != Postgres {
		t.Fatalf("expected Postgres dialect, got %q", repo.dl.kind)
	}

	// INSERT OR IGNORE + datetime() defaults; second insert is idempotent.
	lead := model.Lead{
		ID: "pg-spike-1",
		JobLead: model.JobLead{
			Title:            "Senior DevOps Engineer",
			Company:          "Acme",
			DirectApplyURL:   "https://acme.example/jobs/1",
			ATSPlatformName:  "greenhouse",
			Location:         "Remote",
			PostedAt:         "2026-06-01",
			ScrapedTimestamp: "2026-06-10T00:00:00Z",
			Description:      "Kubernetes Terraform Go pipelines",
		},
	}
	if inserted, err := repo.InsertScrapedLead(lead); err != nil || !inserted {
		t.Fatalf("InsertScrapedLead: inserted=%v err=%v", inserted, err)
	}
	if inserted, err := repo.InsertScrapedLead(lead); err != nil || inserted {
		t.Fatalf("InsertScrapedLead (idempotent): inserted=%v err=%v", inserted, err)
	}

	// metadata upsert (ON CONFLICT(key), datetime('now')).
	if err := repo.WriteHeartbeat("ok", 5, 1, ""); err != nil {
		t.Fatalf("WriteHeartbeat: %v", err)
	}
	if _, ok, err := repo.ScraperHeartbeat(); err != nil || !ok {
		t.Fatalf("ScraperHeartbeat: ok=%v err=%v", ok, err)
	}

	// scoring updates (datetime('now')).
	if err := repo.MarkScoreAttempt(lead.ID); err != nil {
		t.Fatalf("MarkScoreAttempt: %v", err)
	}
	if err := repo.UpdateJobScore(lead.ID, 8, "strong infra match"); err != nil {
		t.Fatalf("UpdateJobScore: %v", err)
	}

	// api_usage upsert (date('now','localtime'), ON CONFLICT(date,model)).
	for i := 0; i < 2; i++ {
		if err := repo.RecordAPICall("gemini-flash"); err != nil {
			t.Fatalf("RecordAPICall: %v", err)
		}
	}
	if n, err := repo.DailyAPIUsage("gemini-flash"); err != nil || n != 2 {
		t.Fatalf("DailyAPIUsage: n=%d err=%v", n, err)
	}
	if n, err := repo.APIUsageToday(); err != nil || n != 2 {
		t.Fatalf("APIUsageToday: n=%d err=%v", n, err)
	}

	// dashboard aggregates (datetime('now','-24 hours'), date('now','localtime')).
	if _, err := repo.ScoringStats(); err != nil {
		t.Fatalf("ScoringStats: %v", err)
	}
	if _, err := repo.GlobalStats(); err != nil {
		t.Fatalf("GlobalStats: %v", err)
	}
	if _, err := repo.StatsRows(); err != nil {
		t.Fatalf("StatsRows: %v", err)
	}
	if _, err := repo.FilteredJobs("all", "created_at DESC"); err != nil {
		t.Fatalf("FilteredJobs: %v", err)
	}

	// company_notes upsert (ON CONFLICT(company), datetime('now')).
	if err := repo.SaveCompanyNotes("acme", "infra", "great team"); err != nil {
		t.Fatalf("SaveCompanyNotes: %v", err)
	}
	if tags, _, found, err := repo.CompanyNotes("acme"); err != nil || !found || tags != "infra" {
		t.Fatalf("CompanyNotes: tags=%q found=%v err=%v", tags, found, err)
	}

	// transaction paths: pipeline stage (COALESCE(applied_at, datetime('now'))) + event log.
	if err := repo.SetPipelineStage(lead.ID, "applied"); err != nil {
		t.Fatalf("SetPipelineStage: %v", err)
	}
	if err := repo.LogEvent(lead.ID, "note", "", "smoke"); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}

	// make_interval rewrite (datetime('now','-'||?||' days')) — just-applied job is
	// not stale, so 0 ghosted, but the query must execute.
	if n, err := repo.AutoGhostStale(14); err != nil {
		t.Fatalf("AutoGhostStale: %v", err)
	} else if n != 0 {
		t.Fatalf("AutoGhostStale ghosted %d, want 0", n)
	}

	// window functions in a transaction.
	if _, err := repo.DedupExistingJobs(); err != nil {
		t.Fatalf("DedupExistingJobs: %v", err)
	}

	// bound interval modifier (datetime('now', ?)) and julianday differences.
	if _, err := repo.CountRejectionsAppliedSince(30); err != nil {
		t.Fatalf("CountRejectionsAppliedSince: %v", err)
	}
	if _, err := repo.RejectionInsights(); err != nil {
		t.Fatalf("RejectionInsights (julianday): %v", err)
	}

	// analytics CTE + joins.
	if _, err := repo.AnalyticsJobs(); err != nil {
		t.Fatalf("AnalyticsJobs: %v", err)
	}
	if _, err := repo.AnalyticsEvents(); err != nil {
		t.Fatalf("AnalyticsEvents: %v", err)
	}
	cutoff := ReachedCutoff(time.Now())
	if _, err := repo.ReachedStageRows(cutoff); err != nil {
		t.Fatalf("ReachedStageRows: %v", err)
	}
	if _, err := repo.AdvancedCountsByScore(cutoff); err != nil {
		t.Fatalf("AdvancedCountsByScore: %v", err)
	}
	if _, err := repo.RecentEvents(); err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}

	// canonicalization: insert an alternate, fold it into a fresh primary row.
	// Exercises INSERT OR IGNORE + COALESCE(NULLIF()) backfill + state merge +
	// dependent re-key + alias upsert + archive, all in one transaction.
	altLead := model.Lead{
		ID: "pg-spike-alt-1",
		JobLead: model.JobLead{
			Title:           "DevOps Engineer",
			Company:         "Acme",
			DirectApplyURL:  "https://aggregator.example/jobs/9",
			ATSPlatformName: "remoteok",
			Location:        "Remote",
			Description:     "infra role via aggregator",
		},
	}
	if _, err := repo.InsertScrapedLead(altLead); err != nil {
		t.Fatalf("InsertScrapedLead alt: %v", err)
	}
	alt := &Job{
		ID:       altLead.ID,
		Title:    sql.NullString{String: altLead.Title, Valid: true},
		Company:  sql.NullString{String: altLead.Company, Valid: true},
		Platform: sql.NullString{String: "remoteok", Valid: true},
		Status:   sql.NullString{String: "pending", Valid: true},
	}
	res := Resolution{
		Status:     "primary",
		Platform:   "greenhouse",
		URL:        "https://boards.greenhouse.io/acme/jobs/123",
		Confidence: 0.92,
		Job: &ResolvedJob{
			ID:       "pg-spike-canon-1",
			Title:    "DevOps Engineer",
			Company:  "Acme",
			URL:      "https://boards.greenhouse.io/acme/jobs/123",
			Platform: "greenhouse",
		},
	}
	out, err := repo.CanonicalizeAlternateJob(alt, res)
	if err != nil {
		t.Fatalf("CanonicalizeAlternateJob: %v", err)
	}
	if out.Action != "canonicalized" || out.CanonicalID != "pg-spike-canon-1" {
		t.Fatalf("CanonicalizeAlternateJob: got %+v", out)
	}

	if n, err := repo.CountAllJobs(); err != nil || n < 2 {
		t.Fatalf("CountAllJobs: n=%d err=%v", n, err)
	}
}
