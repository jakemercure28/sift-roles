package ats

import (
	"context"
	"path/filepath"
	"testing"

	"job-search-automation/internal/db"
	"job-search-automation/internal/model"
)

func newATSRepo(t *testing.T) *db.Repository {
	t.Helper()
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open test repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func TestCanonicalizeExistingAppliesPrimaryResolution(t *testing.T) {
	repo := newATSRepo(t)
	inserted, err := repo.InsertScrapedLead(model.Lead{
		ID: "alt1",
		JobLead: model.JobLead{
			Title:            "Staff SRE",
			Company:          "Acme",
			DirectApplyURL:   "https://builtin.com/job/staff-sre/123",
			ATSPlatformName:  "builtin",
			Location:         "Remote",
			ScrapedTimestamp: "2026-06-01T00:00:00Z",
			Description:      "Aggregator copy",
		},
	})
	if err != nil {
		t.Fatalf("InsertScrapedLead: %v", err)
	}
	if !inserted {
		t.Fatal("expected inserted row")
	}

	const apiURL = "https://boards-api.greenhouse.io/v1/boards/acme/jobs/12345?content=true"
	var seen []string
	fetch := mapFetcher(t, map[string]fakeResponse{
		"https://builtin.com/job/staff-sre/123": {body: `<a href="https://boards.greenhouse.io/acme/jobs/12345">Apply</a>`},
		apiURL: {body: `{
			"id": 12345,
			"title": "Staff SRE",
			"absolute_url": "https://boards.greenhouse.io/acme/jobs/12345",
			"updated_at": "2026-06-01",
			"content": "Own reliability",
			"location": {"name": "Remote"}
		}`},
	}, &seen)

	report, err := CanonicalizeExisting(context.Background(), repo, CanonicalizeConfig{
		OnlyPending: true,
		Concurrency: 2,
		Fetch:       fetch,
	})
	if err != nil {
		t.Fatalf("CanonicalizeExisting: %v", err)
	}
	if report.Counts["canonicalized"] != 1 || len(report.Rows) != 1 {
		t.Fatalf("report = %#v", report)
	}
	row := report.Rows[0]
	if row.ID != "alt1" || row.Action != "canonicalized" || row.CanonicalID != "greenhouse-12345" {
		t.Fatalf("row = %#v", row)
	}
	if row.ResolvedPlatform != "Greenhouse" || row.Evidence != "extracted-url" || row.Confidence != 0.9 {
		t.Fatalf("row = %#v", row)
	}

	remaining, err := repo.SelectAlternateJobs(true)
	if err != nil {
		t.Fatalf("SelectAlternateJobs: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining pending alternates = %#v, want none", remaining)
	}
}

func TestCanonicalizeExistingNoRows(t *testing.T) {
	report, err := CanonicalizeExisting(context.Background(), newATSRepo(t), CanonicalizeConfig{OnlyPending: true})
	if err != nil {
		t.Fatalf("CanonicalizeExisting: %v", err)
	}
	if len(report.Rows) != 0 || len(report.Counts) != 0 {
		t.Fatalf("report = %#v, want empty", report)
	}
}
