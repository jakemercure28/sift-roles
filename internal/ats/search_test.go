package ats

import (
	"context"
	"testing"
)

func TestSearchPrimaryBoardsFindsAshbyAfterGreenhouseMiss(t *testing.T) {
	const (
		ghURL    = "https://boards-api.greenhouse.io/v1/boards/acme/jobs?content=true"
		ashbyURL = "https://api.ashbyhq.com/posting-api/job-board/acme?includeCompensation=true"
		jobID    = "11111111-2222-3333-4444-555555555555"
	)
	var seen []string
	fetch := mapFetcher(t, map[string]fakeResponse{
		ghURL: {body: `{"jobs":[{"id":999,"title":"Product Manager"}]}`},
		ashbyURL: {body: `{"jobs":[{
			"id":"11111111-2222-3333-4444-555555555555",
			"title":"Staff Site Reliability Engineer",
			"companyName":"Acme, Inc.",
			"jobUrl":"https://jobs.ashbyhq.com/acme/11111111-2222-3333-4444-555555555555",
			"publishedAt":"2026-06-01",
			"descriptionPlain":"Operate production.",
			"location":"Remote"
		}]}`},
	}, &seen)

	got, err := searchPrimaryBoards(context.Background(), fetch, Job{
		Title:   "Staff Site Reliability Engineer",
		Company: "Acme, Inc.",
	})
	if err != nil {
		t.Fatalf("searchPrimaryBoards: %v", err)
	}
	if got == nil || got.ID != "ashby-"+jobID || got.Company != "Acme, Inc." || got.Description != "Operate production." {
		t.Fatalf("unexpected canonical job: %#v", got)
	}
	wantSeen := []string{
		"greenhouse-search/acme " + ghURL,
		"ashby-search/acme " + ashbyURL,
	}
	if len(seen) != len(wantSeen) {
		t.Fatalf("seen = %#v, want %#v", seen, wantSeen)
	}
	for i := range wantSeen {
		if seen[i] != wantSeen[i] {
			t.Fatalf("seen = %#v, want %#v", seen, wantSeen)
		}
	}
}

func TestSearchLeverBoardMatchesDistinctiveTitleTokens(t *testing.T) {
	const (
		url   = "https://api.lever.co/v0/postings/acme?mode=json"
		jobID = "11111111-2222-3333-4444-555555555555"
	)
	var seen []string
	fetch := mapFetcher(t, map[string]fakeResponse{
		url: {body: `[{
			"id":"11111111-2222-3333-4444-555555555555",
			"text":"Senior Infrastructure Engineer, Data Platform",
			"hostedUrl":"https://jobs.lever.co/acme/11111111-2222-3333-4444-555555555555",
			"createdAt":1700000000000,
			"descriptionPlain":"Own the data platform",
			"categories":{"location":"Remote"}
		}]`},
	}, &seen)

	got, err := searchLeverBoard(context.Background(), fetch, Job{
		Title:   "Staff Data Platform Engineer",
		Company: "Acme",
	}, "acme")
	if err != nil {
		t.Fatalf("searchLeverBoard: %v", err)
	}
	if got == nil || got.ID != "lever-"+jobID || got.PostedAt != "2023-11-14T22:13:20.000Z" {
		t.Fatalf("unexpected canonical job: %#v", got)
	}
}
