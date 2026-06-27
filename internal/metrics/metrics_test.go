package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// scrape calls the /metrics handler and returns the exposition body.
func scrape(t *testing.T) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics handler status = %d, want 200", rec.Code)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

func TestHandlerExposesRuntimeAndCustomSeries(t *testing.T) {
	// Emit one of each custom metric so the series are present in the exposition.
	ObserveScrapeCycle(true, 2*time.Second)
	ObserveScrapeCycle(false, time.Second)
	ObserveScrapedJob("Built In")
	ObserveATSResolution("unsupported", "Built In", "unsupported")
	ObserveATSResolution("canonicalized", "Built In", "Workday")
	ObserveScoringPass(5, 1, 3*time.Second)
	ObserveGeminiRequest("score_batch", "ok", "200", 800*time.Millisecond)
	ObserveGeminiTokens("score_batch", 1200, 700, 90, 1290)
	SetGeminiUsage("local", 42)
	SetGeminiHostUsage(100)
	SetGeminiTokenUsage("local", 1200, 700, 90, 1290)
	SetGeminiHostTokenUsage(2400, 1400, 180, 2580)
	SetJobsPending("local", 7)
	ObserveHTTP("GET /healthz", http.MethodGet, "200", 5*time.Millisecond)
	ObserveDashboardFragment("all", "total", "paged", 20*time.Millisecond)

	body := scrape(t)

	for _, want := range []string{
		"go_goroutines",                        // runtime collector
		"process_cpu_seconds_total",            // process collector
		`jsa_scrape_cycles_total{result="ok"}`, //
		`jsa_scrape_cycles_total{result="error"}`,
		`jsa_scraped_jobs_total{platform="Built In"}`,
		`jsa_ats_resolutions_total{action="unsupported",from_platform="Built In",to_platform="unsupported"}`,
		`jsa_ats_resolutions_total{action="canonicalized",from_platform="Built In",to_platform="Workday"}`,
		`jsa_jobs_scored_total{result="ok"}`,
		`jsa_jobs_scored_total{result="failed"}`,
		`jsa_gemini_requests_total{operation="score_batch",result="ok",status="200"}`,
		`jsa_gemini_tokens_total{kind="prompt",operation="score_batch"}`,
		`jsa_gemini_tokens_total{kind="cached_prompt",operation="score_batch"}`,
		`jsa_gemini_request_duration_seconds_bucket{operation="score_batch",result="ok",status="200"`,
		`jsa_gemini_daily_usage{user="local"}`,
		"jsa_gemini_host_daily_usage",
		`jsa_gemini_daily_tokens{kind="total",user="local"}`,
		`jsa_gemini_host_daily_tokens{kind="total"}`,
		`jsa_jobs_pending_unscored{user="local"}`,
		"jsa_http_request_duration_seconds",
		`jsa_dashboard_fragment_duration_seconds_bucket{cache="paged",filter="all",phase="total"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics exposition missing %q", want)
		}
	}
}
