package closedcheck

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"job-search-automation/internal/db"
	"job-search-automation/internal/model"
)

func TestAutoCloseEligibility(t *testing.T) {
	cases := []struct {
		name string
		job  db.CloseCheckJob
		want *Eligibility
	}{
		{
			name: "greenhouse job-boards",
			job: db.CloseCheckJob{
				ID: "greenhouse-5030680008", Platform: "Greenhouse",
				URL: "https://job-boards.greenhouse.io/anthropic/jobs/5030680008",
			},
			want: &Eligibility{Platform: "Greenhouse", Slug: "anthropic", JobID: "5030680008"},
		},
		{
			name: "greenhouse boards with query",
			job: db.CloseCheckJob{
				ID: "greenhouse-7241754", Platform: "greenhouse",
				URL: "https://boards.greenhouse.io/cloudflare/jobs/7241754?gh_jid=7241754",
			},
			want: &Eligibility{Platform: "Greenhouse", Slug: "cloudflare", JobID: "7241754"},
		},
		{
			name: "custom greenhouse skipped",
			job: db.CloseCheckJob{
				ID: "greenhouse-7484028", Platform: "Greenhouse",
				URL: "http://bankrate.com/careers/current-openings?gh_jid=7484028",
			},
		},
		{
			name: "builtin primary url skipped",
			job: db.CloseCheckJob{
				ID: "builtin-123", Platform: "Built In",
				URL: "https://job-boards.greenhouse.io/acme/jobs/123",
			},
		},
		{
			name: "ashby",
			job: db.CloseCheckJob{
				ID: "ashby-6b2ee1c2-509e-4433-9a60-3f79d7dfcd42", Platform: "Ashby",
				URL: "https://jobs.ashbyhq.com/helion/6b2ee1c2-509e-4433-9a60-3f79d7dfcd42",
			},
			want: &Eligibility{Platform: "Ashby", Slug: "helion", JobID: "6b2ee1c2-509e-4433-9a60-3f79d7dfcd42"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AutoCloseEligibility(tc.job)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func mapFetcher(responses map[string]*HTTPResult, seen *[]string) Fetcher {
	return func(ctx context.Context, rawURL string) (*HTTPResult, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		*seen = append(*seen, rawURL)
		return responses[rawURL], nil
	}
}

func TestCheckGreenhouse(t *testing.T) {
	cases := []struct {
		name string
		res  *HTTPResult
		want string
	}{
		{"404 closes", &HTTPResult{StatusCode: 404}, "closed"},
		{"410 closes", &HTTPResult{StatusCode: 410}, "closed"},
		{"explicit closed body closes", &HTTPResult{StatusCode: 400, Body: []byte("job has been removed")}, "closed"},
		{"429 stays open", &HTTPResult{StatusCode: 429}, "open"},
		{"200 open", &HTTPResult{StatusCode: 200}, "open"},
		{"network skip", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen []string
			fetch := mapFetcher(map[string]*HTTPResult{
				"https://api.greenhouse.io/v1/boards/acme/jobs/123": tc.res,
			}, &seen)
			got, err := CheckJob(context.Background(), fetch, db.CloseCheckJob{
				ID: "greenhouse-123", Platform: "Greenhouse", URL: "https://boards.greenhouse.io/acme/jobs/123",
			}, map[string]map[string]bool{})
			if err != nil {
				t.Fatalf("CheckJob: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckAshbyUsesBoardMembership(t *testing.T) {
	var seen []string
	const id = "6b2ee1c2-509e-4433-9a60-3f79d7dfcd42"
	fetch := mapFetcher(map[string]*HTTPResult{
		"https://api.ashbyhq.com/posting-api/job-board/helion": {
			StatusCode: 200,
			Body:       []byte(`{"jobs":[{"id":"other"}]}`),
		},
	}, &seen)
	got, err := CheckJob(context.Background(), fetch, db.CloseCheckJob{
		ID: "ashby-" + id, Platform: "Ashby", URL: "https://jobs.ashbyhq.com/helion/" + id,
	}, map[string]map[string]bool{})
	if err != nil {
		t.Fatalf("CheckJob: %v", err)
	}
	if got != "closed" {
		t.Fatalf("got %q, want closed", got)
	}
}

func newRepo(t *testing.T) *db.Repository {
	t.Helper()
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func testLead(id, platform, url string) model.Lead {
	return model.Lead{
		ID: id,
		JobLead: model.JobLead{
			Title:            "Platform Engineer",
			Company:          "Acme",
			Description:      "Build reliable systems.",
			DirectApplyURL:   url,
			ATSPlatformName:  platform,
			ScrapedTimestamp: "2026-06-01T00:00:00Z",
		},
	}
}

func TestRunMarksClosedJobs(t *testing.T) {
	repo := newRepo(t)
	_, err := repo.InsertScrapedLead(testLead("greenhouse-123", "Greenhouse", "https://boards.greenhouse.io/acme/jobs/123"))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	var seen []string
	fetch := mapFetcher(map[string]*HTTPResult{
		"https://api.greenhouse.io/v1/boards/acme/jobs/123": {StatusCode: 404},
	}, &seen)

	got, err := Run(context.Background(), repo, Config{Fetch: fetch, Delay: time.Nanosecond})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Checked != 1 || got.Closed != 1 || got.Skipped != 0 {
		t.Fatalf("result = %+v", got)
	}
	rows, err := repo.FilteredJobs("closed", "")
	if err != nil {
		t.Fatalf("FilteredJobs: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != "closed" || rows[0].Stage != "closed" {
		t.Fatalf("closed rows = %+v", rows)
	}
}

// TestRunContinuesPastProbeError verifies a single flaky probe no longer aborts
// the whole pass: a later job must still be checked and closed, with the failure
// counted rather than fatal.
func TestRunContinuesPastProbeError(t *testing.T) {
	repo := newRepo(t)
	if _, err := repo.InsertScrapedLead(testLead("greenhouse-123", "Greenhouse", "https://boards.greenhouse.io/acme/jobs/123")); err != nil {
		t.Fatalf("insert 123: %v", err)
	}
	if _, err := repo.InsertScrapedLead(testLead("greenhouse-124", "Greenhouse", "https://boards.greenhouse.io/acme/jobs/124")); err != nil {
		t.Fatalf("insert 124: %v", err)
	}

	// /123 fails (simulated timeout); /124 is genuinely gone (404).
	fetch := func(ctx context.Context, rawURL string) (*HTTPResult, error) {
		if strings.HasSuffix(rawURL, "/123") {
			return nil, errors.New("simulated timeout")
		}
		if strings.HasSuffix(rawURL, "/124") {
			return &HTTPResult{StatusCode: 404}, nil
		}
		return &HTTPResult{StatusCode: 200}, nil
	}

	got, err := Run(context.Background(), repo, Config{Fetch: fetch, Delay: time.Nanosecond})
	if err != nil {
		t.Fatalf("Run returned error, want nil (pass must not abort): %v", err)
	}
	if got.Errored != 1 || got.Closed != 1 {
		t.Fatalf("result = %+v, want errored=1 closed=1", got)
	}

	closed, err := repo.FilteredJobs("closed", "")
	if err != nil {
		t.Fatalf("FilteredJobs: %v", err)
	}
	if len(closed) != 1 {
		t.Fatalf("closed rows = %d, want 1 (the 404 job closed despite the earlier error)", len(closed))
	}
}
