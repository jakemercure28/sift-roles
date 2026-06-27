package ats

import (
	"context"
	"reflect"
	"testing"
)

type fakeResponse struct {
	body     string
	finalURL string
}

func mapFetcher(t *testing.T, responses map[string]fakeResponse, seen *[]string) Fetcher {
	t.Helper()
	return func(ctx context.Context, u, label string) (*fetchResult, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		*seen = append(*seen, label+" "+u)
		res, ok := responses[u]
		if !ok {
			return nil, nil
		}
		finalURL := res.finalURL
		if finalURL == "" {
			finalURL = u
		}
		return &fetchResult{Body: []byte(res.body), FinalURL: finalURL}, nil
	}
}

func TestFetchPrimaryJobGreenhouse(t *testing.T) {
	const apiURL = "https://boards-api.greenhouse.io/v1/boards/acme/jobs/12345?content=true"
	var seen []string
	fetch := mapFetcher(t, map[string]fakeResponse{
		apiURL: {body: `{
			"id": 12345,
			"title": "Staff SRE",
			"absolute_url": "https://boards.greenhouse.io/acme/jobs/12345",
			"updated_at": "2026-05-01T00:00:00Z",
			"content": "<p>Run &amp; scale systems.</p>",
			"location": {"name": "Remote"}
		}`},
	}, &seen)

	got, err := fetchPrimaryJob(context.Background(), fetch, "Greenhouse", "https://boards.greenhouse.io/acme/jobs/12345", Job{
		Company: "Fallback Co",
		Title:   "Fallback Title",
	})
	if err != nil {
		t.Fatalf("fetchPrimaryJob: %v", err)
	}
	want := &CanonicalJob{
		ID: "greenhouse-12345", Platform: "Greenhouse", Title: "Staff SRE",
		Company: "acme", URL: "https://boards.greenhouse.io/acme/jobs/12345",
		PostedAt: "2026-05-01T00:00:00Z", Description: "Run & scale systems.",
		Location: "Remote",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical job mismatch\n got: %#v\nwant: %#v", got, want)
	}
	if len(seen) != 1 || seen[0] != "greenhouse-resolve/acme/12345 "+apiURL {
		t.Fatalf("seen = %#v", seen)
	}
}

func TestFetchPrimaryJobAshby(t *testing.T) {
	const (
		jobID  = "11111111-2222-3333-4444-555555555555"
		apiURL = "https://api.ashbyhq.com/posting-api/job-board/acme?includeCompensation=true"
	)
	var seen []string
	fetch := mapFetcher(t, map[string]fakeResponse{
		apiURL: {body: `{"jobs":[
			{"id":"other","title":"Other"},
			{"id":"11111111-2222-3333-4444-555555555555","title":"Platform Engineer","jobUrl":"https://jobs.ashbyhq.com/acme/11111111-2222-3333-4444-555555555555","publishedDate":"2026-04-02","descriptionHtml":"<strong>Build</strong> platforms","locationName":"NYC"}
		]}`},
	}, &seen)

	got, err := fetchPrimaryJob(context.Background(), fetch, "ashby", "https://jobs.ashbyhq.com/acme/"+jobID, Job{Company: "Fallback"})
	if err != nil {
		t.Fatalf("fetchPrimaryJob: %v", err)
	}
	if got == nil || got.ID != "ashby-"+jobID || got.Title != "Platform Engineer" || got.Description != "Build platforms" || got.Location != "NYC" {
		t.Fatalf("unexpected canonical job: %#v", got)
	}
}

func TestFetchPrimaryJobLever(t *testing.T) {
	const (
		jobID  = "11111111-2222-3333-4444-555555555555"
		apiURL = "https://api.lever.co/v0/postings/acme/11111111-2222-3333-4444-555555555555"
	)
	var seen []string
	fetch := mapFetcher(t, map[string]fakeResponse{
		apiURL: {body: `{
			"id":"11111111-2222-3333-4444-555555555555",
			"text":"Backend Engineer",
			"hostedUrl":"https://jobs.lever.co/acme/11111111-2222-3333-4444-555555555555",
			"createdAt":1700000000000,
			"descriptionPlain":"Own APIs",
			"lists":[{"text":"About","content":"Small team"}],
			"categories":{"location":"Remote"}
		}`},
	}, &seen)

	got, err := fetchPrimaryJob(context.Background(), fetch, "Lever", "https://jobs.lever.co/acme/"+jobID, Job{})
	if err != nil {
		t.Fatalf("fetchPrimaryJob: %v", err)
	}
	if got == nil || got.ID != "lever-"+jobID || got.PostedAt != "2023-11-14T22:13:20.000Z" || got.Description != "Own APIs\nAbout\nSmall team" {
		t.Fatalf("unexpected canonical job: %#v", got)
	}
}

func TestFetchPrimaryJobWorkday(t *testing.T) {
	const (
		rawURL = "https://acme.wd1.myworkdayjobs.com/en-US/acme/job/SF/Staff-SRE_JR123"
		apiURL = "https://acme.wd1.myworkdayjobs.com/wday/cxs/acme/acme/job/SF/Staff-SRE_JR123"
	)
	var seen []string
	fetch := mapFetcher(t, map[string]fakeResponse{
		apiURL: {body: `{"jobPostingInfo":{
			"title":"Staff SRE",
			"jobDescription":"<p>Keep services up.</p>",
			"postedOn":"2026-03-01",
			"locationsText":"United States"
		}}`},
	}, &seen)

	got, err := fetchPrimaryJob(context.Background(), fetch, "workday", rawURL, Job{Company: "Acme"})
	if err != nil {
		t.Fatalf("fetchPrimaryJob: %v", err)
	}
	want := &CanonicalJob{
		ID: "workday-acme-JR123", Platform: "Workday", Title: "Staff SRE",
		Company: "Acme", URL: rawURL, PostedAt: "2026-03-01",
		Description: "Keep services up.", Location: "United States",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical job mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestFetchPrimaryJobBadJSONReturnsNil(t *testing.T) {
	const apiURL = "https://api.lever.co/v0/postings/acme/11111111-2222-3333-4444-555555555555"
	var seen []string
	fetch := mapFetcher(t, map[string]fakeResponse{apiURL: {body: `{not-json`}}, &seen)

	got, err := fetchPrimaryJob(
		context.Background(),
		fetch,
		"lever",
		"https://jobs.lever.co/acme/11111111-2222-3333-4444-555555555555",
		Job{},
	)
	if err != nil {
		t.Fatalf("fetchPrimaryJob: %v", err)
	}
	if got != nil {
		t.Fatalf("got %#v, want nil", got)
	}
}
