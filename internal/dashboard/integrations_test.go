package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"job-search-automation/internal/db"
	"job-search-automation/internal/rejectionsync"
)

func newIntegrationsServer(t *testing.T) (*Server, *httptest.Server, *db.Repository) {
	t.Helper()
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	srv, err := New(t.TempDir(), repo, nil, 5*time.Second, 500, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, repo
}

func TestRejectionSyncStatusRoute(t *testing.T) {
	t.Setenv("GMAIL_EMAIL", "")
	t.Setenv("GMAIL_APP_PASSWORD", "")
	t.Setenv("REJECTION_EMAIL_SYNC_DISABLED", "")
	_, ts, repo := newIntegrationsServer(t)

	resp := get(t, ts.URL+"/api/integrations/rejection-sync", nil)
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["configured"] != false || body["paused"] != false || body["status"] != nil {
		t.Fatalf("unconfigured body = %v", body)
	}
	if body["appliedLast7d"].(float64) != 0 {
		t.Fatalf("appliedLast7d = %v", body["appliedLast7d"])
	}

	t.Setenv("GMAIL_EMAIL", "a@b.c")
	t.Setenv("GMAIL_APP_PASSWORD", "pw")
	if err := repo.WriteRejectionSyncStatus(db.RejectionSyncStatus{Status: "ok", Fetched: 5, Applied: 1}); err != nil {
		t.Fatalf("status write: %v", err)
	}
	resp2 := get(t, ts.URL+"/api/integrations/rejection-sync", nil)
	defer resp2.Body.Close()
	body = map[string]any{}
	if err := json.NewDecoder(resp2.Body).Decode(&body); err != nil {
		t.Fatalf("decode 2: %v", err)
	}
	if body["configured"] != true {
		t.Fatalf("configured = %v", body["configured"])
	}
	st, ok := body["status"].(map[string]any)
	if !ok || st["status"] != "ok" || st["applied"].(float64) != 1 {
		t.Fatalf("status = %v", body["status"])
	}
}

func TestRejectionSyncRunRoute(t *testing.T) {
	srv, ts, _ := newIntegrationsServer(t)

	// No runner wired (dashboard-only process) -> 503.
	resp, err := http.Post(ts.URL+"/api/integrations/rejection-sync/run", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no-runner status = %d, want 503", resp.StatusCode)
	}

	srv.SetRejectionSyncRunner(func(context.Context) (rejectionsync.Summary, error) {
		return rejectionsync.Summary{Fetched: 7, Applied: 2, Ignored: 4, Unmatched: 1}, nil
	})
	resp, err = http.Post(ts.URL+"/api/integrations/rejection-sync/run", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post 2: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["ok"] != true || body["applied"].(float64) != 2 || body["fetched"].(float64) != 7 {
		t.Fatalf("run body = %v", body)
	}

	srv.SetRejectionSyncRunner(func(context.Context) (rejectionsync.Summary, error) {
		return rejectionsync.Summary{}, ErrRejectionSyncBusy
	})
	resp, err = http.Post(ts.URL+"/api/integrations/rejection-sync/run", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post 3: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("busy status = %d, want 409", resp.StatusCode)
	}
}
