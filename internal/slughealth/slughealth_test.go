package slughealth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

func writeCompaniesJS(t *testing.T, dir string) {
	t.Helper()
	raw := `{
  "SEARCH_TERMS": ["sre"],
  "GREENHOUSE_COMPANIES": ["ghok"],
  "LEVER_COMPANIES": ["leverempty"],
  "WORKABLE_COMPANIES": ["workableok"],
  "ASHBY_COMPANIES": ["ashbybroken"],
  "WORKDAY_COMPANIES": [{ "sub": "acme", "wd": 5, "board": "External", "label": "Acme" }],
  "RIPPLING_COMPANIES": []
}
`
	if err := os.WriteFile(filepath.Join(dir, "companies.json"), []byte(raw), 0o644); err != nil {
		t.Fatalf("write companies.json: %v", err)
	}
}

func TestClassifyFailure(t *testing.T) {
	if got := classifyFailure(404, "", "", nil); got != "broken" {
		t.Fatalf("404 = %q", got)
	}
	if got := classifyFailure(429, "", "", nil); got != "blocked" {
		t.Fatalf("429 = %q", got)
	}
	if got := classifyFailure(403, "", "Cloudflare captcha", nil); got != "blocked" {
		t.Fatalf("cloudflare = %q", got)
	}
	if got := classifyFailure(500, "", "", nil); got != "transient" {
		t.Fatalf("500 = %q", got)
	}
	if got := classifyFailure(0, "network timeout", "", nil); got != "transient" {
		t.Fatalf("timeout = %q", got)
	}
}

func TestRunSkipsFreshCache(t *testing.T) {
	dir := t.TempDir()
	writeCompaniesJS(t, dir)
	out := filepath.Join(dir, "slug-health.json")
	raw := []byte(`{"timestamp":"2026-05-07T12:00:00.000Z"}`)
	if err := os.WriteFile(out, raw, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	called := false
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		return nil, nil
	})}

	summary, err := Run(context.Background(), Config{
		DataDir:    dir,
		OutputPath: out,
		HTTPClient: client,
		Now:        func() time.Time { return time.Date(2026, 5, 7, 18, 0, 0, 0, time.UTC).Add(-time.Millisecond) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !summary.Skipped || summary.Reason != "cache_fresh" {
		t.Fatalf("summary = %+v, want fresh skip", summary)
	}
	if called {
		t.Fatal("HTTP called despite fresh cache")
	}
}

func TestRunWritesDashboardSummary(t *testing.T) {
	dir := t.TempDir()
	writeCompaniesJS(t, dir)
	out := filepath.Join(dir, "slug-health.json")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := http.StatusOK
		body := `{}`
		switch {
		case r.URL.Host == "boards-api.greenhouse.io":
			body = `{"jobs":[{"id":1}]}`
		case r.URL.Host == "api.lever.co":
			body = `[]`
		case r.URL.Host == "api.ashbyhq.com":
			status = http.StatusNotFound
			body = `missing`
		case r.URL.Host == "acme.wd5.myworkdayjobs.com":
			body = `{"total":0}`
		case r.URL.Host == "www.workable.com":
			status = http.StatusNotFound
			body = `missing`
		case r.URL.Host == "apply.workable.com" && strings.Contains(r.URL.Path, "/widget/"):
			body = `{"jobs":[{"title":"Engineer","shortcode":"ABC"}]}`
		default:
			status = http.StatusNotFound
			body = `missing`
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}

	summary, err := Run(context.Background(), Config{
		DataDir:    dir,
		OutputPath: out,
		HTTPClient: client,
		Now:        func() time.Time { return time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC) },
		Delay:      -1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Total.OK != 2 || summary.Total.Empty != 2 || summary.Total.Broken != 1 {
		t.Fatalf("total = %+v", summary.Total)
	}
	if len(summary.Broken) != 1 || summary.Broken[0].ATS != "Ashby" || summary.Broken[0].Slug != "ashbybroken" {
		t.Fatalf("broken = %+v", summary.Broken)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	var written Summary
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatalf("output JSON: %v", err)
	}
	if written.Timestamp != "2026-05-07T12:00:00.000Z" || written.ByATS["Greenhouse"].OK != 1 {
		t.Fatalf("written = %+v", written)
	}
}
