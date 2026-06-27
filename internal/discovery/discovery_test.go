package discovery

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeGemini struct {
	raw    string
	called int
	prompt string
}

func (f *fakeGemini) CallGemini(_ context.Context, prompt string, _ int) (string, error) {
	f.called++
	f.prompt = prompt
	return f.raw, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

func writeCompaniesJS(t *testing.T, dir string) {
	t.Helper()
	raw := `{
  "SEARCH_TERMS": ["sre", "platform"],
  "GREENHOUSE_COMPANIES": ["stripe"],
  "LEVER_COMPANIES": ["linear"],
  "ASHBY_COMPANIES": ["ramp"],
  "WORKABLE_COMPANIES": ["workableco"],
  "RIPPLING_COMPANIES": ["ripplingco"],
  "WORKDAY_COMPANIES": [
    { "sub": "jcrew", "wd": 1, "board": "JCrewCareers", "label": "J.Crew" }
  ]
}
`
	if err := os.WriteFile(filepath.Join(dir, "companies.json"), []byte(raw), 0o644); err != nil {
		t.Fatalf("write companies.json: %v", err)
	}
}

func seededSuggested(count int, updatedAt string) Suggested {
	var gh []string
	for i := 0; i < count; i++ {
		gh = append(gh, "existing"+strconv.Itoa(i))
	}
	return Suggested{
		Greenhouse: gh,
		Ashby:      []string{},
		Lever:      []string{},
		Workday:    []WorkdayEntry{},
		UpdatedAt:  &updatedAt,
	}
}

func TestSlugVariantsAndKey(t *testing.T) {
	got := SlugVariants("Warby Parker")
	want := []string{"warbyparker", "warby-parker", "warby parker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SlugVariants = %#v, want %#v", got, want)
	}
	if key := SlugKey("Harry's"); key != "harrys" {
		t.Fatalf("SlugKey = %q, want harrys", key)
	}
	if tokens := MaxOutputTokens(50); tokens != 4500 {
		t.Fatalf("MaxOutputTokens = %d, want 4500", tokens)
	}
	if tokens := MaxOutputTokens(10); tokens != 2000 {
		t.Fatalf("MaxOutputTokens small = %d, want 2000", tokens)
	}
}

func TestCacheState(t *testing.T) {
	updated := "2026-05-07T12:00:00.000Z"
	now := time.Date(2026, 5, 7, 17, 0, 0, 0, time.UTC)
	state := CacheState(&updated, 6*time.Hour, now)
	if !state.Fresh || state.AgeHours == nil || *state.AgeHours != 5 {
		t.Fatalf("CacheState = %+v, want fresh age 5", state)
	}
	if state.NextEligibleS != "2026-05-07T18:00:00.000Z" {
		t.Fatalf("NextEligibleS = %q", state.NextEligibleS)
	}

	expired := CacheState(&updated, 6*time.Hour, now.Add(time.Hour))
	if expired.Fresh {
		t.Fatal("cache fresh at TTL boundary")
	}
}

func TestParseWorkdayURL(t *testing.T) {
	got := ParseWorkdayURL("https://capitalone.wd12.myworkdayjobs.com/en-US/Capital_One")
	if got == nil || got.Sub != "capitalone" || got.WD != 12 || got.Board != "Capital_One" {
		t.Fatalf("ParseWorkdayURL board = %+v", got)
	}
	got = ParseWorkdayURL("https://acme.wd5.myworkdayjobs.com/wday/cxs/acme/External/job")
	if got == nil || got.Sub != "acme" || got.WD != 5 || got.Board != "External" {
		t.Fatalf("ParseWorkdayURL cxs = %+v", got)
	}
	if got := ParseWorkdayURL("https://example.com/jobs"); got != nil {
		t.Fatalf("ParseWorkdayURL unsupported = %+v, want nil", got)
	}
}

func TestLoadCompanyConfig(t *testing.T) {
	dir := t.TempDir()
	writeCompaniesJS(t, dir)
	cfg, err := LoadCompanyConfig(dir)
	if err != nil {
		t.Fatalf("LoadCompanyConfig: %v", err)
	}
	if !reflect.DeepEqual(cfg.SearchTerms, []string{"sre", "platform"}) {
		t.Fatalf("SearchTerms = %#v", cfg.SearchTerms)
	}
	if !reflect.DeepEqual(cfg.Greenhouse, []string{"stripe"}) || !reflect.DeepEqual(cfg.Ashby, []string{"ramp"}) || !reflect.DeepEqual(cfg.Lever, []string{"linear"}) {
		t.Fatalf("platform slugs = %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.Workable, []string{"workableco"}) || !reflect.DeepEqual(cfg.Rippling, []string{"ripplingco"}) {
		t.Fatalf("other slugs = %+v", cfg)
	}
	if len(cfg.Workday) != 1 || cfg.Workday[0].Sub != "jcrew" || cfg.Workday[0].Board != "JCrewCareers" {
		t.Fatalf("Workday = %+v", cfg.Workday)
	}
}

func TestBuildPromptUsesResumeWithoutSearchTerms(t *testing.T) {
	prompt := BuildPrompt(map[string]bool{"existing": true}, nil, "Visual Merchandiser\nRetail assortment planning", 12)
	if !strings.Contains(prompt, "Job seeker resume excerpt:") || !strings.Contains(prompt, "Visual Merchandiser") {
		t.Fatalf("prompt missing resume signal:\n%s", prompt)
	}
	if strings.Contains(prompt, "Configured search terms:") {
		t.Fatalf("prompt should omit empty search terms:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Suggest 12 NEW companies") {
		t.Fatalf("prompt missing candidate count:\n%s", prompt)
	}
}

func TestLoadSaveSuggested(t *testing.T) {
	dir := t.TempDir()
	s := Suggested{
		Greenhouse: []string{"gymshark"},
		Ashby:      []string{"away"},
		Lever:      []string{"farfetch"},
		Workday:    []WorkdayEntry{{Sub: "jcrew", WD: 1, Board: "JCrewCareers"}},
	}
	if err := SaveSuggested(dir, s); err != nil {
		t.Fatalf("SaveSuggested: %v", err)
	}
	loaded := LoadSuggested(dir)
	if !reflect.DeepEqual(loaded.Greenhouse, s.Greenhouse) || WorkdayKey(loaded.Workday[0]) != "jcrew/JCrewCareers" {
		t.Fatalf("loaded = %+v", loaded)
	}
}

func TestRunSkipsFreshNonBootstrapCache(t *testing.T) {
	dir := t.TempDir()
	writeCompaniesJS(t, dir)
	updated := "2026-05-07T12:00:00.000Z"
	if err := SaveSuggested(dir, seededSuggested(BootstrapThreshold, updated)); err != nil {
		t.Fatalf("SaveSuggested: %v", err)
	}
	gemini := &fakeGemini{raw: `[]`}
	report, err := Run(context.Background(), Config{
		DataDir:        dir,
		TTLHours:       DefaultTTLHours,
		CandidateCount: DefaultCandidateCount,
		Gemini:         gemini,
		Now:            func() time.Time { return time.Date(2026, 5, 7, 17, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Skipped || report.Reason != "cache_fresh" || report.TotalSuggested != BootstrapThreshold {
		t.Fatalf("report = %+v, want fresh skip", report)
	}
	if gemini.called != 0 {
		t.Fatalf("Gemini called %d times, want 0", gemini.called)
	}
}

func TestRunVerifiesAndSavesSuggestedCompanies(t *testing.T) {
	dir := t.TempDir()
	writeCompaniesJS(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "resume.md"), []byte("Visual Merchandiser\nRetail assortment planning."), 0o644); err != nil {
		t.Fatalf("write resume: %v", err)
	}
	stale := "2026-05-07T00:00:00.000Z"
	if err := SaveSuggested(dir, seededSuggested(BootstrapThreshold, stale)); err != nil {
		t.Fatalf("SaveSuggested: %v", err)
	}
	gemini := &fakeGemini{raw: `[
	  {"name":"Warby Parker","platform":"Greenhouse","slug":"Warby Parker","rationale":"fit"},
	  {"name":"Capital One","platform":"Workday","url":"https://capitalone.wd12.myworkdayjobs.com/en-US/Capital_One","rationale":"fit"},
	  {"name":"Stripe","platform":"Greenhouse","slug":"stripe","rationale":"already tracked"}
	]`}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := http.StatusNotFound
		body := `{}`
		switch {
		case r.URL.Host == "boards-api.greenhouse.io" && strings.Contains(r.URL.Path, "/warbyparker/"):
			status = http.StatusOK
		case r.URL.Host == "capitalone.wd12.myworkdayjobs.com" && r.Method == http.MethodPost:
			status = http.StatusOK
			body = `{"total":42}`
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}

	report, err := Run(context.Background(), Config{
		DataDir:        dir,
		TTLHours:       DefaultTTLHours,
		CandidateCount: DefaultCandidateCount,
		Gemini:         gemini,
		HTTPClient:     client,
		Now:            func() time.Time { return time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Skipped || report.Added != 2 || report.Passes != 1 {
		t.Fatalf("report = %+v, want two additions in one pass", report)
	}
	if !strings.Contains(gemini.prompt, "Visual Merchandiser") {
		t.Fatalf("discovery prompt did not include resume: %s", gemini.prompt)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "suggested-companies.json"))
	if err != nil {
		t.Fatalf("read suggested: %v", err)
	}
	var saved Suggested
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("suggested JSON: %v", err)
	}
	if !slices.Contains(saved.Greenhouse, "warbyparker") {
		t.Fatalf("greenhouse = %#v, want warbyparker", saved.Greenhouse)
	}
	if len(saved.Workday) != 1 || WorkdayKey(saved.Workday[0]) != "capitalone/Capital_One" {
		t.Fatalf("workday = %+v", saved.Workday)
	}
	if slices.Contains(saved.Greenhouse, "stripe") {
		t.Fatalf("static slug was added to suggested list: %#v", saved.Greenhouse)
	}
}
