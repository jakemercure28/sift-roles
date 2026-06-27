package dashboard

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"job-search-automation/internal/db"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newRepo opens a fresh migrated repository in a temp dir.
func newRepo(t *testing.T) *db.Repository {
	t.Helper()
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// newServer builds the dashboard backed by a real repo, serving static files
// from a temp public dir seeded with dashboard.css.
func newServer(t *testing.T) (*Server, *db.Repository) {
	t.Helper()
	pub := t.TempDir()
	if err := os.WriteFile(filepath.Join(pub, "dashboard.css"), []byte("body{color:red}"), 0o644); err != nil {
		t.Fatalf("seed css: %v", err)
	}
	repo := newRepo(t)
	srv, err := New(pub, repo, nil, 4*time.Second, 500, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, repo
}

func TestStaticCachingAndMime(t *testing.T) {
	srv, _ := newServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// With ?v= the asset is immutable; correct MIME; no proxying.
	resp := get(t, ts.URL+"/public/dashboard.css?v=64", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/css" {
		t.Fatalf("content-type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("cache-control = %q", cc)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "body{color:red}" {
		t.Fatalf("body = %q", body)
	}

	// Without ?v= the asset is no-cache.
	resp2 := get(t, ts.URL+"/public/dashboard.css", nil)
	defer resp2.Body.Close()
	if cc := resp2.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("no-version cache-control = %q", cc)
	}
}

func TestStaticGzip(t *testing.T) {
	srv, _ := newServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := get(t, ts.URL+"/public/dashboard.css?v=1", map[string]string{"Accept-Encoding": "gzip"})
	defer resp.Body.Close()
	if enc := resp.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("content-encoding = %q, want gzip", enc)
	}
	// Go's http client auto-decompresses only when it set the header itself; here
	// we set Accept-Encoding manually, so decompress to verify.
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	body, _ := io.ReadAll(gz)
	if string(body) != "body{color:red}" {
		t.Fatalf("decompressed body = %q", body)
	}
}

func TestStaticRejectsUnknownExtAndMissing(t *testing.T) {
	srv, _ := newServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, p := range []string{"/public/secret.txt", "/public/missing.css"} {
		resp := get(t, ts.URL+p, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", p, resp.StatusCode)
		}
	}
}

// TestStaticRejectsTraversal calls the handler directly: ServeMux path-cleans
// "../" before routing, so the in-handler guard is exercised by a crafted path.
func TestStaticRejectsTraversal(t *testing.T) {
	srv, _ := newServer(t)
	for _, p := range []string{"/public/../db/jobs.db", "/public/../../etc/passwd.css"} {
		req := httptest.NewRequest(http.MethodGet, "http://x"+p, nil)
		req.URL.Path = p // preserve the literal traversal path
		rec := httptest.NewRecorder()
		srv.handleStatic(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", p, rec.Code)
		}
	}
}

func TestHealthz(t *testing.T) {
	srvOK, _ := newServer(t)
	ts := httptest.NewServer(srvOK.Handler())
	defer ts.Close()

	resp := get(t, ts.URL+"/healthz", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("ok healthz status = %d", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["ok"] != true {
		t.Fatalf("healthz body = %v", body)
	}

	// Closing the repo makes the ping fail -> 503.
	srvBad, badRepo := newServer(t)
	_ = badRepo.Close()
	ts2 := httptest.NewServer(srvBad.Handler())
	defer ts2.Close()
	resp2 := get(t, ts2.URL+"/healthz", nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("down healthz status = %d, want 503", resp2.StatusCode)
	}
}

// With the Node proxy removed, unmatched routes 404 instead of falling through.
func TestUnknownRouteNotFound(t *testing.T) {
	srv, _ := newServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := get(t, ts.URL+"/api/setup/not-ported", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// --- helpers ---

func get(t *testing.T, url string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// Disable the transport's automatic gzip so manual Accept-Encoding is honored.
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}
