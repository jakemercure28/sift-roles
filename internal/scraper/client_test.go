package scraper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"job-search-automation/internal/middleware"
)

func TestHealthSendsTraceHeader(t *testing.T) {
	const traceID = "abc123"
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(middleware.TraceHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.URL, time.Second)
	ctx := middleware.ContextWithTraceID(context.Background(), traceID)
	if err := client.Health(ctx); err != nil {
		t.Fatalf("Health returned error: %v", err)
	}

	if got != traceID {
		t.Fatalf("trace header = %q, want %q", got, traceID)
	}
}

// Health must hit the shallow /livez, never /health: /health answers 503 while
// a board pass overruns its stuck threshold, which would restart the worker for
// being slow rather than for being broken.
func TestHealthProbesLivez(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := New(server.URL, time.Second).Health(context.Background()); err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if path != "/livez" {
		t.Fatalf("Health probed %q, want %q", path, "/livez")
	}
}

func TestScrapeSendsTraceHeader(t *testing.T) {
	const traceID = "def456"
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(middleware.TraceHeader)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ScrapeResponse{})
	}))
	defer server.Close()

	client := New(server.URL, time.Second)
	ctx := middleware.ContextWithTraceID(context.Background(), traceID)
	if _, err := client.Scrape(ctx, nil, ""); err != nil {
		t.Fatalf("Scrape returned error: %v", err)
	}

	if got != traceID {
		t.Fatalf("trace header = %q, want %q", got, traceID)
	}
}

func TestScrapeSendsProfileDir(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ScrapeResponse{})
	}))
	defer server.Close()

	client := New(server.URL, time.Second)
	if _, err := client.Scrape(context.Background(), nil, "/app/data/storage/users/u1"); err != nil {
		t.Fatalf("Scrape returned error: %v", err)
	}
	if got := body["profileDir"]; got != "/app/data/storage/users/u1" {
		t.Fatalf("profileDir = %v, want /app/data/storage/users/u1", got)
	}

	// Empty profileDir must be omitted so the worker falls back to its DATA_DIR.
	body = nil
	if _, err := client.Scrape(context.Background(), nil, ""); err != nil {
		t.Fatalf("Scrape returned error: %v", err)
	}
	if _, ok := body["profileDir"]; ok {
		t.Fatalf("profileDir should be omitted when empty, got %v", body["profileDir"])
	}
}
