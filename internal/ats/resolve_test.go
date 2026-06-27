package ats

import (
	"context"
	"strings"
	"testing"
)

type fakeGemini struct {
	response  string
	prompts   []string
	maxTokens []int
}

func (g *fakeGemini) CallGemini(ctx context.Context, prompt string, maxTokens int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	g.prompts = append(g.prompts, prompt)
	g.maxTokens = append(g.maxTokens, maxTokens)
	return g.response, nil
}

func TestParseGeminiJSONArray(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "direct array",
			raw:  `[{"platform":"Greenhouse","slug":"acme"}]`,
			want: "acme",
		},
		{
			name: "wrapped candidates",
			raw:  `{"candidates":[{"platform":"Ashby","companySlug":"acme-io"}]}`,
			want: "acme-io",
		},
		{
			name: "fenced json",
			raw:  "```json\n[{\"platform\":\"Lever\",\"board\":\"acme\"}]\n```",
			want: "acme",
		},
		{
			name: "embedded array",
			raw:  `Sure: [{"platform":"Greenhouse","boardToken":"acme"}]`,
			want: "acme",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseGeminiJSONArray(tc.raw)
			if len(got) != 1 || got[0].slugValue() != tc.want {
				t.Fatalf("parseGeminiJSONArray = %#v, want slug %q", got, tc.want)
			}
		})
	}
}

func TestResolveAlternateJobExtractedURL(t *testing.T) {
	const (
		sourceURL = "https://builtin.com/job/staff-sre/123"
		atsURL    = "https://boards.greenhouse.io/acme/jobs/12345"
		apiURL    = "https://boards-api.greenhouse.io/v1/boards/acme/jobs/12345?content=true"
	)
	var seen []string
	fetch := mapFetcher(t, map[string]fakeResponse{
		sourceURL: {body: `<html><a href="https://boards.greenhouse.io/acme/jobs/12345">Apply</a></html>`},
		apiURL: {body: `{
			"id": 12345,
			"title": "Staff SRE",
			"absolute_url": "https://boards.greenhouse.io/acme/jobs/12345",
			"updated_at": "2026-06-01",
			"content": "Own reliability",
			"location": {"name": "Remote"}
		}`},
	}, &seen)

	got, err := ResolveAlternateJob(context.Background(), Job{
		ID: "alt1", Platform: "Built In", Company: "Acme", Title: "Staff SRE", URL: sourceURL,
	}, ResolveOptions{Fetch: fetch})
	if err != nil {
		t.Fatalf("ResolveAlternateJob: %v", err)
	}
	if got == nil || got.Status != "primary" || got.Confidence != 0.9 || got.Job == nil || got.Job.ID != "greenhouse-12345" {
		t.Fatalf("unexpected resolution: %#v", got)
	}
	if got.Platform != "Greenhouse" || got.URL != atsURL {
		t.Fatalf("platform/url = %q/%q", got.Platform, got.URL)
	}
	if got.Evidence["method"] != "extracted-url" || got.Evidence["sourceUrl"] != sourceURL {
		t.Fatalf("evidence = %#v", got.Evidence)
	}
}

func TestResolveAlternateJobUnsupportedSource(t *testing.T) {
	var seen []string
	fetch := mapFetcher(t, map[string]fakeResponse{}, &seen)

	got, err := ResolveAlternateJob(context.Background(), Job{
		ID: "alt1", Company: "Acme", Title: "Staff SRE", URL: "https://www.linkedin.com/jobs/view/123",
	}, ResolveOptions{Fetch: fetch})
	if err != nil {
		t.Fatalf("ResolveAlternateJob: %v", err)
	}
	if got == nil || got.Status != "unsupported" || got.Confidence != 0.75 {
		t.Fatalf("unexpected resolution: %#v", got)
	}
	if got.Evidence["unsupportedPlatform"] != "LinkedIn" {
		t.Fatalf("evidence = %#v", got.Evidence)
	}
}

func TestResolveAlternateJobGeminiFallbackVerified(t *testing.T) {
	const (
		sourceURL = "https://example.com/careers/staff-sre"
		atsURL    = "https://job-boards.greenhouse.io/acme/jobs/12345"
		apiURL    = "https://boards-api.greenhouse.io/v1/boards/acme/jobs/12345?content=true"
	)
	var seen []string
	fetch := mapFetcher(t, map[string]fakeResponse{
		sourceURL: {body: `<html><h1>Staff SRE</h1></html>`},
		apiURL: {body: `{
			"id": 12345,
			"title": "Staff SRE",
			"absolute_url": "https://boards.greenhouse.io/acme/jobs/12345",
			"updated_at": "2026-06-01",
			"content": "Own reliability",
			"location": {"name": "Remote"}
		}`},
	}, &seen)
	gemini := &fakeGemini{response: `[{
		"platform":"Greenhouse",
		"slug":"acme",
		"jobId":"12345",
		"company":"Acme",
		"title":"Staff SRE",
		"rationale":"Title and company match"
	}]`}

	got, err := ResolveAlternateJob(context.Background(), Job{
		ID: "alt1", Company: "Acme", Title: "Staff SRE", URL: sourceURL, Location: "Remote",
	}, ResolveOptions{Fetch: fetch, Gemini: gemini})
	if err != nil {
		t.Fatalf("ResolveAlternateJob: %v", err)
	}
	if got == nil || got.Status != "primary" || got.Confidence != 0.78 || got.Job == nil || got.Job.ID != "greenhouse-12345" {
		t.Fatalf("unexpected resolution: %#v", got)
	}
	if got.Evidence["method"] != "gemini-candidate-api-verified" {
		t.Fatalf("evidence = %#v", got.Evidence)
	}
	if len(gemini.prompts) != 1 || !strings.Contains(gemini.prompts[0], "Company: Acme") || gemini.maxTokens[0] != 1200 {
		t.Fatalf("gemini calls = prompts %#v tokens %#v", gemini.prompts, gemini.maxTokens)
	}
	if !strings.Contains(strings.Join(seen, "\n"), "greenhouse-resolve/acme/12345 "+apiURL) {
		t.Fatalf("seen = %#v; candidate URL was %s", seen, atsURL)
	}
}
