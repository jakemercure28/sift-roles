package trigger

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"job-search-automation/internal/middleware"
)

// fakeRunner mimics the scheduler's in-flight guard: the first TryStart wins
// (returns true) and holds the flag until release() is called; concurrent calls
// see it busy.
type fakeRunner struct {
	running atomic.Bool
	calls   atomic.Int32
	traceID atomic.Value
	userID  atomic.Value
}

func (f *fakeRunner) TryStart(ctx context.Context, _ time.Duration, userID string) bool {
	f.calls.Add(1)
	f.traceID.Store(middleware.TraceID(ctx))
	f.userID.Store(userID)
	return f.running.CompareAndSwap(false, true)
}

func (f *fakeRunner) release() { f.running.Store(false) }

func newServer(r Runner) *Server {
	return New(r, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestScrapePassesTenantIDToRunner(t *testing.T) {
	f := &fakeRunner{}
	srv := httptest.NewServer(newServer(f).Handler())
	defer srv.Close()

	res, err := http.Post(srv.URL+"/scrape", "application/json", bytes.NewBufferString(`{"userId":"user-123"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("scrape: got %d, want 202", res.StatusCode)
	}
	if got, _ := f.userID.Load().(string); got != "user-123" {
		t.Fatalf("runner userID = %q, want user-123", got)
	}
}

func TestScrapeStartsThenBusy(t *testing.T) {
	f := &fakeRunner{}
	srv := httptest.NewServer(newServer(f).Handler())
	defer srv.Close()

	// First trigger starts a cycle.
	res, err := http.Post(srv.URL+"/scrape", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("first scrape: got %d, want 202", res.StatusCode)
	}

	// Second trigger while the first is "running" is rejected as busy.
	res2, err := http.Post(srv.URL+"/scrape", "application/json", nil)
	if err != nil {
		t.Fatalf("post 2: %v", err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusConflict {
		t.Fatalf("second scrape: got %d, want 409", res2.StatusCode)
	}

	// Once the cycle finishes, a new trigger starts again.
	f.release()
	res3, err := http.Post(srv.URL+"/scrape", "application/json", nil)
	if err != nil {
		t.Fatalf("post 3: %v", err)
	}
	res3.Body.Close()
	if res3.StatusCode != http.StatusAccepted {
		t.Fatalf("third scrape: got %d, want 202", res3.StatusCode)
	}
}

func TestScrapeRejectsGet(t *testing.T) {
	srv := httptest.NewServer(newServer(&fakeRunner{}).Handler())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/scrape")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /scrape: got %d, want 405", res.StatusCode)
	}
}

func TestHealthOK(t *testing.T) {
	srv := httptest.NewServer(newServer(&fakeRunner{}).Handler())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /health: got %d, want 200", res.StatusCode)
	}
}

func TestScrapePassesTraceIDToRunner(t *testing.T) {
	f := &fakeRunner{}
	srv := httptest.NewServer(newServer(f).Handler())
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/scrape", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set(middleware.TraceHeader, "ABC123")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	res.Body.Close()

	if got, _ := f.traceID.Load().(string); got != "abc123" {
		t.Fatalf("runner trace ID = %q, want abc123", got)
	}
	if got := res.Header.Get(middleware.TraceHeader); got != "abc123" {
		t.Fatalf("response trace header = %q, want abc123", got)
	}
}
